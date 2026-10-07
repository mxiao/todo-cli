package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// finish runs the permission policy over the pending items: executable
// ones are applied right away, the rest wait for the user (FR-305).
func (s *Service) finish(ctx context.Context, sess *Session, st Settings) (*Session, error) {
	policy := Policy{Mode: sess.Mode, Confirm: st.ConfirmList}
	var auto []int
	for i := range sess.Items {
		it := &sess.Items[i]
		if it.Status != ItemPending {
			continue
		}
		if it.Kind == ItemOptimize {
			it.NeedsConfirm, it.ConfirmReason = true, "自我优化建议需要人工确认，不会静默替换运行版本"
			continue
		}
		v, err := policy.Check(policyAction(it))
		it.Categories = v.Categories
		switch {
		case err != nil:
			it.Status, it.Message = ItemSkipped, err.Error()
		case v.Execute:
			auto = append(auto, it.N)
		default:
			it.NeedsConfirm, it.ConfirmReason = true, v.Reason
		}
	}
	if len(auto) > 0 {
		s.applyItems(ctx, sess, auto, "auto")
	}
	sess.refreshStatus()
	sess.Result = sess.tally()
	return sess, s.saveSession(sess)
}

// Apply accepts items of a session (all pending ones when ns is empty):
// accept one, accept all (FR-406), or confirm items waiting in confirm
// mode. An explicit user action is the confirmation, also for items on the
// confirm list.
func (s *Service) Apply(ctx context.Context, id string, ns []int) (*Session, error) {
	sess, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	if err := checkOpen(sess); err != nil {
		return nil, err
	}
	ns, err = selectItems(sess, ns, ItemPending)
	if err != nil {
		return nil, err
	}
	s.applyItems(ctx, sess, ns, "user")
	sess.refreshStatus()
	sess.Result = sess.tally()
	return sess, s.saveSession(sess)
}

// Reject rejects items; with no item numbers it ignores every pending item.
func (s *Service) Reject(id string, ns []int) (*Session, error) {
	sess, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	if sess.Status == StatusNeedsInput && len(ns) == 0 {
		sess.Status = StatusRejected
		return sess, s.saveSession(sess)
	}
	if err := checkOpen(sess); err != nil {
		return nil, err
	}
	ns, err = selectItems(sess, ns, ItemPending)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		it := sess.item(n)
		it.Status, it.Message = ItemRejected, "已拒绝"
	}
	sess.refreshStatus()
	sess.Result = sess.tally()
	return sess, s.saveSession(sess)
}

func checkOpen(sess *Session) error {
	switch sess.Status {
	case StatusNeedsInput:
		return fmt.Errorf("%w: session %s waits for answers to its questions (todo ai answer)", ErrState, sess.ID)
	case StatusSuperseded:
		return fmt.Errorf("%w: session %s was replaced by %s", ErrState, sess.ID, sess.SupersededBy)
	case StatusUndone:
		return fmt.Errorf("%w: session %s was undone", ErrState, sess.ID)
	}
	return nil
}

// selectItems returns ns (or every item in state want when ns is empty),
// checking they exist and are in that state.
func selectItems(sess *Session, ns []int, want string) ([]int, error) {
	if len(ns) == 0 {
		for _, it := range sess.Items {
			if it.Status == want {
				ns = append(ns, it.N)
			}
		}
		if len(ns) == 0 {
			return nil, fmt.Errorf("%w: session %s has no %s items", ErrState, sess.ID, want)
		}
		return ns, nil
	}
	for _, n := range ns {
		it := sess.item(n)
		if it == nil {
			return nil, fmt.Errorf("%w: session %s has no item #%d", ErrNotFound, sess.ID, n)
		}
		if it.Status != want {
			return nil, fmt.Errorf("%w: item #%d is %s, not %s", ErrState, n, it.Status, want)
		}
	}
	return ns, nil
}

type itemOutcome struct {
	status, msg, taskID string
}

// applyItems applies the given pending items. Task changes form one
// atomic, undoable operation with history actor "llm"; an item that fails
// is rolled back alone and reported (FR-306).
func (s *Service) applyItems(ctx context.Context, sess *Session, ns []int, by string) {
	var taskItems, cmdItems, optItems []*Item
	for _, n := range ns {
		it := sess.item(n)
		switch it.Kind {
		case ItemCommand, ItemAgent:
			cmdItems = append(cmdItems, it)
		case ItemOptimize:
			optItems = append(optItems, it)
		default:
			taskItems = append(taskItems, it)
		}
	}
	if len(taskItems) > 0 {
		s.applyTaskItems(sess, taskItems, by)
	}
	for _, it := range optItems {
		s.applySuggestion(sess, it, by)
	}
	for _, it := range cmdItems {
		s.runCommandItem(ctx, sess, it, by)
	}
}

func (s *Service) applyTaskItems(sess *Session, items []*Item, by string) {
	created := map[string]string{}
	for _, it := range sess.Items {
		if it.Ref != "" && it.ResultTaskID != "" && it.Status == ItemApplied {
			created[it.Ref] = it.ResultTaskID
		}
	}
	var out map[int]itemOutcome
	resolve := func(ref string) (string, bool) {
		if refPattern.MatchString(ref) {
			id, ok := created[ref]
			return id, ok
		}
		return ref, true
	}
	opID, err := s.llm.Do("llm_"+sess.Kind, fmt.Sprintf("llm %s %s (%s)", sess.Kind, sess.ID, by), func(tx *core.Tx) error {
		out = map[int]itemOutcome{}
		try := func(it *Item, fn func() (string, error)) {
			var taskID string
			err := tx.Try(func() error {
				var err error
				taskID, err = fn()
				return err
			})
			if err != nil {
				out[it.N] = itemOutcome{status: ItemFailed, msg: friendly(err)}
				return
			}
			out[it.N] = itemOutcome{status: ItemApplied, taskID: taskID}
			if it.Ref != "" {
				created[it.Ref] = taskID
			}
		}
		// New tasks first, parents and dependencies before dependants.
		var creates, others []*Item
		var orders []*Item
		for _, it := range items {
			switch {
			case it.Kind == ItemCreate || it.Kind == ItemSubtask:
				creates = append(creates, it)
			case it.Kind == ItemChange && it.Change.Field == "order":
				orders = append(orders, it)
			default:
				others = append(others, it)
			}
		}
		for progress := true; progress && len(creates) > 0; {
			progress = false
			var wait []*Item
			for _, it := range creates {
				if !s.ready(it, resolve) {
					wait = append(wait, it)
					continue
				}
				progress = true
				try(it, func() (string, error) {
					in, err := s.newTask(it, resolve)
					if err != nil {
						return "", err
					}
					t, err := tx.Create(in)
					if err != nil {
						return "", err
					}
					return t.ID, nil
				})
			}
			creates = wait
		}
		for _, it := range creates {
			out[it.N] = itemOutcome{status: ItemFailed, msg: "父任务或依赖未能先创建"}
		}
		for _, it := range others {
			try(it, func() (string, error) {
				if it.Kind == ItemChange && it.Change.Field == "delete" {
					_, err := tx.Delete(it.TaskID)
					return it.TaskID, err
				}
				p, err := s.patchFor(it, resolve)
				if err != nil {
					return "", err
				}
				_, err = tx.Update(it.TaskID, p)
				return it.TaskID, err
			})
		}
		sort.SliceStable(orders, func(i, j int) bool { return orderRank(orders[i]) < orderRank(orders[j]) })
		prev := ""
		for _, it := range orders {
			try(it, func() (string, error) {
				pl := core.Placement{Top: prev == "", After: prev}
				_, err := tx.Reorder(it.TaskID, pl)
				return it.TaskID, err
			})
			if out[it.N].status == ItemApplied {
				prev = it.TaskID
			}
		}
		return nil
	})
	for _, it := range items {
		o, ok := out[it.N]
		switch {
		case err != nil:
			it.Status, it.Message = ItemFailed, friendly(err)
		case !ok:
			it.Status, it.Message = ItemFailed, "未执行"
		default:
			it.Status, it.Message, it.ResultTaskID = o.status, o.msg, o.taskID
			if o.status == ItemApplied {
				it.Operation, it.AppliedBy, it.NeedsConfirm = opID, by, false
			}
		}
	}
	if err == nil && opID != 0 {
		sess.Operations = append(sess.Operations, opID)
	}
}

func orderRank(it *Item) int {
	n, _ := toInt(it.Change.To)
	return n
}

// ready reports whether an item's parent and dependencies already exist.
func (s *Service) ready(it *Item, resolve func(string) (string, bool)) bool {
	if it.ParentRef != "" {
		if _, ok := resolve(it.ParentRef); !ok {
			return false
		}
	}
	if it.Fields != nil && it.Fields.DependsOn != nil {
		for _, d := range *it.Fields.DependsOn {
			if _, ok := resolve(d); !ok {
				return false
			}
		}
	}
	return true
}

func (s *Service) newTask(it *Item, resolve func(string) (string, bool)) (core.NewTask, error) {
	f := it.Fields
	in := core.NewTask{Title: deref(f.Title), Description: deref(f.Description), Category: deref(f.Category)}
	if f.Tags != nil {
		in.Tags = *f.Tags
	}
	if f.Priority != nil {
		p, err := core.ParsePriority(*f.Priority)
		if err != nil {
			return in, err
		}
		in.Priority = p
	}
	if f.Due != nil && *f.Due != "" {
		t, err := time.Parse(time.RFC3339, *f.Due)
		if err != nil {
			return in, fmt.Errorf("%w: due %q", core.ErrInvalid, *f.Due)
		}
		in.DueAt = &t
	}
	if it.ParentRef != "" {
		in.ParentID, _ = resolve(it.ParentRef)
	}
	if f.DependsOn != nil {
		for _, d := range *f.DependsOn {
			id, _ := resolve(d)
			in.DependsOn = append(in.DependsOn, id)
		}
	}
	return in, nil
}

func (s *Service) patchFor(it *Item, resolve func(string) (string, bool)) (core.TaskPatch, error) {
	var p core.TaskPatch
	if it.Kind == ItemChange {
		c := it.Change
		switch c.Field {
		case "priority":
			pr, err := core.ParsePriority(fmt.Sprint(c.To))
			if err != nil {
				return p, err
			}
			p.Priority = &pr
		case "due_at":
			v, _ := c.To.(string)
			if v == "" {
				p.ClearDue = true
			} else {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					return p, fmt.Errorf("%w: due %q", core.ErrInvalid, v)
				}
				p.DueAt = &t
			}
		case "status":
			st, err := core.ParseStatus(fmt.Sprint(c.To))
			if err != nil {
				return p, err
			}
			p.Status = &st
		case "depends_on":
			deps := []string{}
			for _, d := range toStrings(c.To) {
				id, ok := resolve(d)
				if !ok {
					return p, fmt.Errorf("%w: dependency %s was not created", core.ErrInvalid, d)
				}
				deps = append(deps, id)
			}
			p.DependsOn = &deps
		default:
			return p, fmt.Errorf("%w: unknown change field %q", core.ErrInvalid, c.Field)
		}
		return p, nil
	}
	f := it.Fields
	p.Title, p.Description, p.Category = f.Title, f.Description, f.Category
	p.Tags = f.Tags
	if f.Priority != nil {
		pr, err := core.ParsePriority(*f.Priority)
		if err != nil {
			return p, err
		}
		p.Priority = &pr
	}
	if f.Due != nil {
		if *f.Due == "" {
			p.ClearDue = true
		} else {
			t, err := time.Parse(time.RFC3339, *f.Due)
			if err != nil {
				return p, fmt.Errorf("%w: due %q", core.ErrInvalid, *f.Due)
			}
			p.DueAt = &t
		}
	}
	if f.DependsOn != nil {
		deps := []string{}
		for _, d := range *f.DependsOn {
			id, ok := resolve(d)
			if !ok {
				return p, fmt.Errorf("%w: dependency %s was not created", core.ErrInvalid, d)
			}
			deps = append(deps, id)
		}
		p.DependsOn = &deps
	}
	return p, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

func toStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case string:
		if l == "" {
			return []string{}
		}
		return strings.Split(l, ",")
	}
	return []string{}
}

// friendly turns store errors into short messages.
func friendly(err error) string {
	var ce *core.ConflictError
	switch {
	case errors.As(err, &ce):
		return "任务在此期间被修改过，请重新决策"
	case errors.Is(err, core.ErrDeleted):
		return "任务已被删除"
	case errors.Is(err, core.ErrNotFound):
		return "任务不存在"
	}
	return err.Error()
}

// Undo reverts applied items of a session (all of them when ns is empty):
// task changes through their recorded operations, configuration changes
// through a new configuration version. Commands that already ran cannot be
// undone and are reported.
func (s *Service) Undo(id string, ns []int) (*Session, error) {
	sess, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	ns, err = selectItems(sess, ns, ItemApplied)
	if err != nil {
		return nil, err
	}
	var ops []int64
	for _, n := range ns {
		it := sess.item(n)
		if it.Operation != 0 && !slices.Contains(ops, it.Operation) {
			ops = append(ops, it.Operation)
		}
	}
	slices.Sort(ops)
	slices.Reverse(ops)
	var firstErr error
	for _, op := range ops {
		if _, err := s.store.UndoOperation(op); err != nil && !errors.Is(err, core.ErrAlreadyUndone) {
			firstErr = err
			break
		}
		for i := range sess.Items {
			if it := &sess.Items[i]; it.Operation == op && it.Status == ItemApplied {
				it.Status, it.Message = ItemUndone, "已撤销"
			}
		}
	}
	if firstErr == nil {
		for _, n := range ns {
			it := sess.item(n)
			switch it.Kind {
			case ItemOptimize:
				if err := s.undoSuggestion(it); err != nil {
					firstErr = err
				}
			case ItemCommand, ItemAgent:
				sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 命令已执行，无法自动撤销", n))
			}
		}
	}
	undone := true
	for _, it := range sess.Items {
		if it.Status == ItemApplied {
			undone = false
		}
	}
	if undone {
		sess.Status = StatusUndone
	} else {
		sess.refreshStatus()
	}
	sess.Result = sess.tally()
	if err := s.saveSession(sess); err != nil {
		return nil, err
	}
	return sess, firstErr
}

// ItemEdit changes a proposed item before it is saved (FR-304). Dates and
// priorities use the same syntax as the CLI ("明天 18:00", "high").
type ItemEdit struct {
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
	Due         *string   `json:"due,omitempty"` // "" clears
	Priority    *string   `json:"priority,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	Category    *string   `json:"category,omitempty"`
	DependsOn   *[]string `json:"depends_on,omitempty"` // task ids/prefixes or refs N1
	// To replaces the target value of a decision change.
	To *string `json:"to,omitempty"`
}

// EditItem edits a pending (or skipped) item; a skipped item that becomes
// valid is pending again.
func (s *Service) EditItem(id string, n int, e ItemEdit) (*Session, error) {
	sess, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	if err := checkOpen(sess); err != nil {
		return nil, err
	}
	it := sess.item(n)
	if it == nil {
		return nil, fmt.Errorf("%w: session %s has no item #%d", ErrNotFound, sess.ID, n)
	}
	if it.Status != ItemPending && it.Status != ItemSkipped {
		return nil, fmt.Errorf("%w: item #%d is %s and can no longer be edited", ErrState, n, it.Status)
	}
	switch it.Kind {
	case ItemCreate, ItemSubtask, ItemUpdate:
		if err := s.editFields(sess, it, e); err != nil {
			return nil, err
		}
		if err := s.describeItem(sess, it); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	case ItemChange:
		if e.To == nil {
			return nil, fmt.Errorf("%w: a decision change is edited with --to", ErrInvalid)
		}
		task, err := s.store.Get(it.TaskID)
		if err != nil {
			return nil, err
		}
		to, err := s.changeValue(it.Change.Field, *e.To, sess.Refs, true)
		if err != nil {
			return nil, err
		}
		it.Change.To = to
		it.Diff = []Diff{s.changeDiff(sess, task, it.Change)}
	default:
		return nil, fmt.Errorf("%w: %s items cannot be edited", ErrInvalid, it.Kind)
	}
	if it.Status == ItemSkipped {
		it.Status, it.Message = ItemPending, ""
		it.NeedsConfirm = true
	}
	it.Message = "已手动修改"
	sess.refreshStatus()
	sess.Result = sess.tally()
	return sess, s.saveSession(sess)
}

func (s *Service) editFields(sess *Session, it *Item, e ItemEdit) error {
	f := it.Fields
	if f == nil {
		f = &TaskFields{}
		it.Fields = f
	}
	if e.Title != nil {
		t := strings.TrimSpace(*e.Title)
		if t == "" {
			return fmt.Errorf("%w: the title cannot be empty", ErrInvalid)
		}
		f.Title = &t
	}
	if e.Description != nil {
		f.Description = e.Description
	}
	if e.Category != nil {
		c := strings.TrimSpace(*e.Category)
		f.Category = &c
	}
	if e.Tags != nil {
		tags := core.NormalizeTags(*e.Tags)
		f.Tags = &tags
	}
	if e.Priority != nil {
		p, err := parsePriority(*e.Priority)
		if err != nil {
			return err
		}
		name := p.String()
		f.Priority = &name
	}
	if e.Due != nil {
		if strings.TrimSpace(*e.Due) == "" {
			if it.Kind == ItemUpdate {
				empty := ""
				f.Due = &empty
			} else {
				f.Due = nil
			}
		} else {
			t, err := core.ParseDue(*e.Due, s.now())
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			v := t.Format(time.RFC3339)
			f.Due, f.DueText = &v, *e.Due
		}
	}
	if e.DependsOn != nil {
		deps := []string{}
		for _, d := range *e.DependsOn {
			d = strings.TrimSpace(d)
			switch {
			case d == "":
			case refPattern.MatchString(d):
				if !slices.ContainsFunc(sess.Items, func(o Item) bool { return o.Ref == d && o.Status != ItemSkipped && o.Status != ItemRejected }) {
					return fmt.Errorf("%w: no new task %s in this session", ErrInvalid, d)
				}
				deps = append(deps, d)
			default:
				id, err := s.store.ResolveID(d)
				if err != nil {
					return err
				}
				deps = append(deps, id)
			}
		}
		f.DependsOn = &deps
	}
	if (it.Kind == ItemCreate || it.Kind == ItemSubtask) && (f.Title == nil || *f.Title == "") {
		return fmt.Errorf("%w: a new task needs a title", ErrInvalid)
	}
	return nil
}
