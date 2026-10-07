package llm

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// IntakeInput is a natural-language request to create or change tasks.
type IntakeInput struct {
	Text string
	// Selected are task ids the user has selected; the model may update
	// them or add subtasks under them (FR-302).
	Selected []string
	// Mode overrides the configured permission mode.
	Mode    Mode
	Profile string
}

const intakeFormat = `Return one JSON object only:
{
  "questions": ["..."],          // only when information that the tasks need is missing or ambiguous
  "items": [{
    "action": "create" | "update" | "subtask",
    "ref": "N1",                 // id for new tasks so other items can refer to them
    "task": "T3",                // update: the existing task ref
    "parent": "T3" | "N1",       // subtask: parent task ref
    "title": "...", "description": "...",
    "due": "YYYY-MM-DD HH:MM",   // local time; omit when the user gave no deadline
    "due_text": "...",           // the exact words of the user that state the deadline
    "priority": "none|low|medium|high|urgent",
    "tags": ["..."], "category": "...",
    "depends_on": ["N1", "T2"],  // tasks that must be finished first
    "reason": "..."              // why this item (new / update / subtask)
  }],
  "summary": "..."
}
Decide per item whether it is a new task, a change to an existing task (use the given refs, prefer selected tasks) or a subtask.
Only include fields the user actually stated; never invent deadlines, priorities or requirements.
Write titles in the user's language.`

type intakeResponse struct {
	Questions []string        `json:"questions"`
	Items     []rawIntakeItem `json:"items"`
	Summary   string          `json:"summary"`
}

type rawIntakeItem struct {
	Action      string    `json:"action"`
	Ref         string    `json:"ref"`
	Task        string    `json:"task"`
	Parent      string    `json:"parent"`
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Due         *string   `json:"due"`
	DueText     string    `json:"due_text"`
	Priority    *string   `json:"priority"`
	Tags        *[]string `json:"tags"`
	Category    *string   `json:"category"`
	DependsOn   []string  `json:"depends_on"`
	Reason      string    `json:"reason"`
}

// Intake turns natural language into proposed task changes. When key
// information is missing the session asks once (status needs_input);
// otherwise the items are applied or wait for confirmation per the mode.
func (s *Service) Intake(ctx context.Context, in IntakeInput) (*Session, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, fmt.Errorf("%w: describe the tasks to create", ErrInvalid)
	}
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	mode := st.Mode
	if in.Mode != "" {
		mode = in.Mode
	}
	sess := &Session{ID: newSessionID(), Kind: KindIntake, Mode: mode, Input: s.Redact(text), Selected: in.Selected,
		Origin: s.opts.Origin, Profile: in.Profile}
	return s.runIntake(ctx, sess)
}

// Answer supplies the answers to a session's clarifying questions and
// continues; the model is not allowed to ask again (FR-303).
func (s *Service) Answer(ctx context.Context, id, answers string) (*Session, error) {
	sess, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	if sess.Status != StatusNeedsInput {
		return nil, fmt.Errorf("%w: session %s is %s, it does not wait for answers", ErrState, sess.ID, sess.Status)
	}
	if strings.TrimSpace(answers) == "" {
		return nil, fmt.Errorf("%w: an answer is required", ErrInvalid)
	}
	sess.Answers = s.Redact(strings.TrimSpace(answers))
	sess.Status = StatusPending
	return s.runIntake(ctx, sess)
}

func (s *Service) runIntake(ctx context.Context, sess *Session) (*Session, error) {
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	cfg, cfgVersion, err := s.AssistConfig()
	if err != nil {
		return nil, err
	}
	c, r, err := s.client(sess.Profile)
	if err != nil {
		return nil, err
	}
	sess.Profile, sess.Model, sess.ConfigVersion = r.Name, r.Model, cfgVersion
	tasks, refOf, refs, err := s.candidates(sess.Selected, 60, nil)
	if err != nil {
		return nil, err
	}
	sess.Refs = refs
	final := sess.Asked
	system := cfg.Prompts.Intake + "\n\nCurrent local time: " + s.nowLine() + "\n\n" + intakeFormat
	if final {
		system += "\nThe user already answered your questions. Do not ask again: leave unknown fields empty."
	}
	var selRefs []string
	for _, id := range sess.Selected {
		selRefs = append(selRefs, refOf[id])
	}
	user := map[string]any{"input": sess.Input, "tasks": s.taskContext(tasks, st.SendFields, refOf, sess.Selected)}
	if len(selRefs) > 0 {
		user["selected"] = selRefs
	}
	if final {
		user["questions"], user["answers"] = sess.Questions, sess.Answers
	}
	var resp intakeResponse
	if err := s.ask(ctx, c, r, sess.ID, "intake", []Message{{Role: "system", Content: system}, {Role: "user", Content: mustJSON(user)}}, &resp); err != nil {
		return nil, err
	}
	if resp.Summary != "" {
		sess.Summary = s.Redact(truncate(resp.Summary, 500))
	}
	sess.Items = sess.Items[:0]
	sess.Warnings = nil
	for i, raw := range resp.Items {
		sess.Items = append(sess.Items, s.intakeItem(sess, i+1, raw))
	}
	questions := cleanList(resp.Questions, 3)
	if !final {
		for _, it := range sess.Items {
			if it.Status == ItemSkipped && it.Message == msgNoTitle {
				questions = append(questions, fmt.Sprintf("第 %d 条任务缺少标题，它具体要做什么？", it.N))
			}
		}
	}
	switch {
	case len(questions) > 0 && !final:
		sess.Questions, sess.Asked, sess.Status = questions, true, StatusNeedsInput
		return sess, s.saveSession(sess)
	case len(questions) > 0:
		sess.Warnings = append(sess.Warnings, "模型再次提问，已忽略（仅追问一次）："+strings.Join(questions, "；"))
	}
	if len(sess.Items) == 0 {
		sess.Warnings = append(sess.Warnings, "模型没有识别出任务")
	}
	s.linkRefs(sess)
	return s.finish(ctx, sess, st)
}

const msgNoTitle = "缺少标题，未创建"

var refPattern = regexp.MustCompile(`^N\d+$`)

// intakeItem validates one model item into a reviewable change.
func (s *Service) intakeItem(sess *Session, n int, raw rawIntakeItem) Item {
	it := Item{N: n, Status: ItemPending, Reason: s.Redact(truncate(raw.Reason, 300)), Fields: &TaskFields{}}
	skip := func(msg string) Item {
		it.Status, it.Message = ItemSkipped, msg
		return it
	}
	switch strings.ToLower(strings.TrimSpace(raw.Action)) {
	case "create", "new", "add", "":
		it.Kind = ItemCreate
	case "subtask", "child", "sub":
		it.Kind = ItemSubtask
	case "update", "modify", "edit", "change":
		it.Kind = ItemUpdate
	default:
		return skip(fmt.Sprintf("未知操作 %q", raw.Action))
	}
	f := it.Fields
	if raw.Title != nil {
		t := strings.TrimSpace(s.Redact(truncate(*raw.Title, 200)))
		f.Title = &t
	}
	if raw.Description != nil {
		d := s.Redact(truncate(*raw.Description, 4000))
		f.Description = &d
	}
	if raw.Category != nil {
		c := strings.TrimSpace(*raw.Category)
		f.Category = &c
	}
	if raw.Tags != nil {
		tags := core.NormalizeTags(*raw.Tags)
		f.Tags = &tags
	}
	if raw.Priority != nil && strings.TrimSpace(*raw.Priority) != "" {
		if p, err := parsePriority(*raw.Priority); err == nil {
			name := p.String()
			f.Priority = &name
		} else {
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 优先级 %q 无法识别，已忽略", n, *raw.Priority))
		}
	}
	if raw.Due != nil {
		due := strings.TrimSpace(*raw.Due)
		switch {
		case due == "" && it.Kind == ItemUpdate:
			f.Due = &due // clears the due time
		case due == "":
		case !s.hasEvidence(sess, raw.DueText):
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 截止时间 %q 在原文中没有依据，已忽略（不编造截止时间）", n, due))
		default:
			if t, err := s.parseDue(due, raw.DueText); err == nil {
				v := t.Format(time.RFC3339)
				f.Due, f.DueText = &v, raw.DueText
			} else {
				sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 截止时间 %q 无法解析，已忽略", n, due))
			}
		}
	}
	if len(raw.DependsOn) > 0 {
		deps := []string{}
		for _, d := range raw.DependsOn {
			if id, ok := sess.Refs[d]; ok {
				deps = append(deps, id)
			} else if refPattern.MatchString(d) {
				deps = append(deps, d)
			} else {
				sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 依赖 %q 不存在，已忽略", n, d))
			}
		}
		f.DependsOn = &deps
	}
	switch it.Kind {
	case ItemUpdate:
		id, ok := sess.Refs[raw.Task]
		if !ok && len(sess.Selected) == 1 && raw.Task == "" {
			id, ok = sess.Selected[0], true
		}
		if !ok {
			return skip(fmt.Sprintf("要修改的任务 %q 不存在", raw.Task))
		}
		it.TaskID = id
	case ItemSubtask:
		switch {
		case refPattern.MatchString(raw.Parent):
			it.ParentRef = raw.Parent
		case sess.Refs[raw.Parent] != "":
			it.ParentRef = sess.Refs[raw.Parent]
		case raw.Parent == "" && len(sess.Selected) == 1:
			it.ParentRef = sess.Selected[0]
		default:
			return skip(fmt.Sprintf("父任务 %q 不存在", raw.Parent))
		}
	}
	if it.Kind != ItemUpdate {
		it.Ref = strings.TrimSpace(raw.Ref)
		if !refPattern.MatchString(it.Ref) || slices.ContainsFunc(sess.Items, func(o Item) bool { return o.Ref == it.Ref }) {
			it.Ref = "N" + itoa(n)
		}
		if f.Title == nil || *f.Title == "" {
			return skip(msgNoTitle)
		}
	}
	if err := s.describeItem(sess, &it); err != nil {
		return skip(err.Error())
	}
	return it
}

// linkRefs rewrites model refs (N…) to the session's item refs and drops
// links to skipped items.
func (s *Service) linkRefs(sess *Session) {
	valid := map[string]bool{}
	for _, it := range sess.Items {
		if it.Ref != "" && it.Status != ItemSkipped {
			valid[it.Ref] = true
		}
	}
	for i := range sess.Items {
		it := &sess.Items[i]
		if it.Status == ItemSkipped {
			continue
		}
		if refPattern.MatchString(it.ParentRef) && !valid[it.ParentRef] {
			it.Status, it.Message = ItemSkipped, "父任务 "+it.ParentRef+" 未被创建"
			continue
		}
		if it.Fields != nil && it.Fields.DependsOn != nil {
			deps := (*it.Fields.DependsOn)[:0]
			for _, d := range *it.Fields.DependsOn {
				if refPattern.MatchString(d) && (!valid[d] || d == it.Ref) {
					sess.Warnings = append(sess.Warnings, fmt.Sprintf("#%d 依赖 %s 未被创建，已去掉", it.N, d))
					continue
				}
				deps = append(deps, d)
			}
			*it.Fields.DependsOn = deps
		}
		_ = s.describeItem(sess, it)
	}
}

// hasEvidence checks that the words stating a deadline really occur in the
// user's input or answers.
func (s *Service) hasEvidence(sess *Session, quote string) bool {
	norm := func(v string) string { return strings.Join(strings.Fields(strings.ToLower(v)), "") }
	q := norm(quote)
	return q != "" && strings.Contains(norm(sess.Input+" "+sess.Answers), q)
}

func (s *Service) parseDue(due, quote string) (time.Time, error) {
	now := s.now()
	if t, err := core.ParseDue(due, now); err == nil {
		return t, nil
	}
	if t, err := core.ParseTimeInput(due, true); err == nil {
		return t, nil
	}
	return core.ParseDue(quote, now)
}

func parsePriority(v string) (core.Priority, error) {
	switch strings.TrimSpace(v) {
	case "紧急", "非常紧急", "最高":
		return core.PriorityUrgent, nil
	case "高", "重要", "高优先级":
		return core.PriorityHigh, nil
	case "中", "普通", "中等":
		return core.PriorityMedium, nil
	case "低", "不急", "低优先级":
		return core.PriorityLow, nil
	case "无":
		return core.PriorityNone, nil
	}
	return core.ParsePriority(v)
}

func cleanList(in []string, limit int) []string {
	var out []string
	for _, q := range in {
		if q = strings.TrimSpace(q); q != "" && len(out) < limit {
			out = append(out, q)
		}
	}
	return out
}

// describeItem fills the diff, title and policy kind of a task item.
func (s *Service) describeItem(sess *Session, it *Item) error {
	f := it.Fields
	var cur *core.Task
	if it.Kind == ItemUpdate {
		t, err := s.store.Get(it.TaskID)
		if err != nil {
			return fmt.Errorf("任务不存在：%v", err)
		}
		if t.Deleted() {
			return fmt.Errorf("任务已删除")
		}
		cur = t
		it.TaskTitle = t.Title
	}
	it.Diff = nil
	add := func(field string, before, after any) {
		it.Diff = append(it.Diff, Diff{Field: field, Before: before, After: after})
	}
	val := func(p *string) any {
		if p == nil || *p == "" {
			return nil
		}
		return *p
	}
	cv := func(get func(t *core.Task) any) any {
		if cur == nil {
			return nil
		}
		return get(cur)
	}
	if f.Title != nil && (cur == nil || cur.Title != *f.Title) {
		add("title", cv(func(t *core.Task) any { return t.Title }), *f.Title)
	}
	if f.Description != nil && (cur == nil || cur.Description != *f.Description) {
		add("description", cv(func(t *core.Task) any { return nilStr(t.Description) }), val(f.Description))
	}
	if f.Due != nil {
		var after any
		if *f.Due != "" {
			t, _ := time.Parse(time.RFC3339, *f.Due)
			after = s.localTime(t)
		}
		var before any
		if cur != nil && cur.DueAt != nil {
			before = s.localTime(*cur.DueAt)
		}
		if before != after {
			add("due_at", before, after)
		}
	}
	if f.Priority != nil && (cur == nil || cur.Priority.String() != *f.Priority) {
		add("priority", cv(func(t *core.Task) any { return t.Priority.String() }), *f.Priority)
	}
	if f.Tags != nil && (cur == nil || !slices.Equal(cur.Tags, *f.Tags)) {
		add("tags", cv(func(t *core.Task) any { return t.Tags }), *f.Tags)
	}
	if f.Category != nil && (cur == nil || cur.Category != *f.Category) {
		add("category", cv(func(t *core.Task) any { return nilStr(t.Category) }), val(f.Category))
	}
	if f.DependsOn != nil {
		after := s.refTitles(sess, *f.DependsOn)
		var before []string
		if cur != nil {
			before = s.refTitles(sess, cur.DependsOn)
		}
		if !slices.Equal(before, after) {
			add("depends_on", before, after)
		}
	}
	if it.Kind == ItemSubtask {
		add("parent", nil, s.refTitles(sess, []string{it.ParentRef})[0])
	}
	if it.Kind == ItemUpdate && len(it.Diff) == 0 {
		return fmt.Errorf("没有需要修改的内容")
	}
	return nil
}

func nilStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// refTitles names tasks or session refs for display.
func (s *Service) refTitles(sess *Session, refs []string) []string {
	out := []string{}
	for _, r := range refs {
		switch {
		case refPattern.MatchString(r):
			name := r
			for _, it := range sess.Items {
				if it.Ref == r && it.Fields != nil && it.Fields.Title != nil {
					name = r + " " + *it.Fields.Title
				}
			}
			out = append(out, name)
		default:
			if t, err := s.store.Get(r); err == nil {
				out = append(out, t.Title)
			} else {
				out = append(out, r)
			}
		}
	}
	return out
}

// policyKind maps an item to the policy operation kind.
func policyAction(it *Item) Action {
	switch it.Kind {
	case ItemCreate, ItemSubtask:
		return Action{Kind: OpTaskCreate}
	case ItemUpdate:
		for _, d := range it.Diff {
			if d.Field == "title" || d.Field == "description" {
				return Action{Kind: OpTaskOverwrite}
			}
		}
		return Action{Kind: OpTaskUpdate}
	case ItemChange:
		if it.Change != nil && it.Change.Field == "delete" {
			return Action{Kind: OpTaskDelete}
		}
		return Action{Kind: OpTaskUpdate}
	case ItemCommand:
		return Action{Kind: OpCommand, Argv: it.Command.Argv}
	case ItemAgent:
		return Action{Kind: OpAgentStart, Argv: it.Command.Argv}
	}
	return Action{Kind: OpConfigChange}
}
