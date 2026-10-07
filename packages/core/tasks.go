package core

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Result is returned by every mutating call.
type Result struct {
	// OperationID identifies the undoable operation; 0 when nothing changed.
	OperationID int64 `json:"operation_id,omitempty"`
	// Changed counts tasks that were actually modified.
	Changed int `json:"changed"`
	// Tasks holds the current state of every targeted task.
	Tasks []Task `json:"tasks"`
}

// Create inserts a new task.
func (s *Store) Create(in NewTask) (*Task, error) {
	var out *Task
	_, err := s.write("create", "create "+strings.TrimSpace(in.Title), func(x *txn) error {
		pos, err := x.nextPosition()
		if err != nil {
			return err
		}
		t := &Task{
			ID: newID(), Title: in.Title, Description: in.Description, DueAt: in.DueAt, Priority: in.Priority,
			Tags: in.Tags, Category: in.Category, ParentID: in.ParentID, DependsOn: in.DependsOn, Notes: in.Notes, Status: in.Status,
			Position: pos, CreatedAt: x.now, Version: 1,
		}
		if t.Status == "" {
			t.Status = StatusTodo
		}
		applyStatusStamps(t, x.now)
		if _, err := x.save("create", nil, t); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// applyStatusStamps sets completion/archive stamps for a freshly built task.
func applyStatusStamps(t *Task, now time.Time) {
	s := t.Status
	t.Status = ""
	applyStatus(t, s, now)
}

// Get returns a task by full ID, including soft-deleted tasks.
func (s *Store) Get(id string) (*Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+s.taskColumns()+` FROM tasks t WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	return t, err
}

// ResolveID expands a unique ID prefix (as shown in short listings) to a
// full task ID.
func (s *Store) ResolveID(prefix string) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return "", fmt.Errorf("%w: empty task id", ErrInvalid)
	}
	rows, err := s.db.Query(`SELECT id FROM tasks WHERE id = ? OR id LIKE ? ESCAPE '\' ORDER BY id = ? DESC LIMIT 3`,
		prefix, escapeLike(prefix)+"%", prefix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("%w: no task matches %q", ErrNotFound, prefix)
	case ids[0] == prefix || len(ids) == 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%w: %q matches several tasks; use more characters", ErrAmbiguousID, prefix)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Update applies a partial patch to one task.
func (s *Store) Update(id string, p TaskPatch) (*Task, error) {
	res, err := s.mutate("update", "update", []string{id}, nil, func(x *txn, t *Task) error {
		if err := checkVersion(t, p.ExpectedVersion); err != nil {
			return err
		}
		return ApplyPatch(t, p, x.now)
	})
	if err != nil {
		return nil, err
	}
	return &res.Tasks[0], nil
}

// ApplyPatch applies p to t in memory (no version check, no persistence).
// Update uses it; callers may use it to preview an edit, e.g. to show
// what a rejected concurrent edit would have produced.
func ApplyPatch(t *Task, p TaskPatch, now time.Time) error {
	setStr(&t.Title, p.Title)
	setStr(&t.Description, p.Description)
	setStr(&t.Notes, p.Notes)
	setStr(&t.Category, p.Category)
	setStr(&t.ParentID, p.ParentID)
	if p.ClearDue {
		t.DueAt = nil
	} else if p.DueAt != nil {
		d := p.DueAt.UTC()
		t.DueAt = &d
	}
	if p.Priority != nil {
		t.Priority = *p.Priority
	}
	if p.Tags != nil {
		t.Tags = slices.Clone(*p.Tags)
	}
	t.Tags = NormalizeTags(append(t.Tags, p.AddTags...))
	if rm := NormalizeTags(p.RemoveTags); len(rm) > 0 {
		t.Tags = slices.DeleteFunc(t.Tags, func(tag string) bool { return slices.Contains(rm, tag) })
	}
	if p.DependsOn != nil {
		t.DependsOn = normalizeIDs(*p.DependsOn)
	}
	if p.Status != nil {
		if !p.Status.valid() {
			return fmt.Errorf("%w: invalid status %q", ErrInvalid, *p.Status)
		}
		applyStatus(t, *p.Status, now.UTC())
	}
	return nil
}

func setStr(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// Complete marks tasks done. Already-done tasks are left unchanged.
func (s *Store) Complete(ids ...string) (*Result, error) {
	return s.setStatus("complete", StatusDone, ids)
}

// Start marks tasks in progress.
func (s *Store) Start(ids ...string) (*Result, error) {
	return s.setStatus("start", StatusInProgress, ids)
}

// Reopen moves done or archived tasks back to todo.
func (s *Store) Reopen(ids ...string) (*Result, error) {
	return s.setStatus("reopen", StatusTodo, ids)
}

// Archive archives tasks.
func (s *Store) Archive(ids ...string) (*Result, error) {
	return s.setStatus("archive", StatusArchived, ids)
}

func (s *Store) setStatus(action string, st Status, ids []string) (*Result, error) {
	return s.setStatusChecked(action, st, ids, nil)
}

func (s *Store) setStatusChecked(action string, st Status, ids []string, expect map[string]int64) (*Result, error) {
	return s.mutate(action, action, ids, expect, func(x *txn, t *Task) error {
		applyStatus(t, st, x.now)
		return nil
	})
}

// SetPriority changes the priority of tasks.
func (s *Store) SetPriority(p Priority, ids ...string) (*Result, error) {
	return s.setPriority(p, ids, nil)
}

func (s *Store) setPriority(p Priority, ids []string, expect map[string]int64) (*Result, error) {
	return s.mutate("priority", "set priority "+p.String(), ids, expect, func(x *txn, t *Task) error {
		t.Priority = p
		return nil
	})
}

// MoveTarget selects where Move puts tasks; nil fields are left untouched
// and an empty string clears the category or parent.
type MoveTarget struct {
	Category *string
	ParentID *string
}

// Move re-files tasks under a category and/or parent task.
func (s *Store) Move(target MoveTarget, ids ...string) (*Result, error) {
	return s.move(target, ids, nil)
}

func (s *Store) move(target MoveTarget, ids []string, expect map[string]int64) (*Result, error) {
	if target.Category == nil && target.ParentID == nil {
		return nil, fmt.Errorf("%w: move needs a category or parent", ErrInvalid)
	}
	return s.mutate("move", "move", ids, expect, func(x *txn, t *Task) error {
		setStr(&t.Category, target.Category)
		setStr(&t.ParentID, target.ParentID)
		return nil
	})
}

// Delete soft-deletes tasks; they disappear from listings but can be
// restored or brought back with Undo.
func (s *Store) Delete(ids ...string) (*Result, error) {
	return s.delete(ids, nil)
}

func (s *Store) delete(ids []string, expect map[string]int64) (*Result, error) {
	return s.mutate("delete", "delete", ids, expect, func(x *txn, t *Task) error {
		t.DeletedAt = &x.now
		return nil
	})
}

// Restore brings soft-deleted tasks back.
func (s *Store) Restore(ids ...string) (*Result, error) {
	return s.restore(ids, nil)
}

func (s *Store) restore(ids []string, expect map[string]int64) (*Result, error) {
	ids = dedupe(ids)
	res := &Result{}
	opID, err := s.write("restore", "restore", func(x *txn) error {
		for _, id := range ids {
			t, err := x.get(id)
			if err != nil {
				return err
			}
			if err := checkVersion(t, expect[id]); err != nil {
				return err
			}
			before := t.clone()
			t.DeletedAt = nil
			changed, err := x.save("restore", before, t)
			if err != nil {
				return err
			}
			if changed {
				res.Changed++
			}
			res.Tasks = append(res.Tasks, *t)
		}
		return nil
	})
	res.OperationID = opID
	return res, err
}

// Placement positions a task in the manual order.
type Placement struct {
	Before string
	After  string
	Top    bool
	Bottom bool
	// ExpectedVersion enables optimistic locking when non-zero.
	ExpectedVersion int64
}

// Reorder moves a task within the manual sort order.
func (s *Store) Reorder(id string, pl Placement) (*Task, error) {
	n := 0
	for _, set := range []bool{pl.Before != "", pl.After != "", pl.Top, pl.Bottom} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("%w: choose exactly one of before, after, top or bottom", ErrInvalid)
	}
	res, err := s.mutate("reorder", "reorder", []string{id}, nil, func(x *txn, t *Task) error {
		if err := checkVersion(t, pl.ExpectedVersion); err != nil {
			return err
		}
		pos, err := x.placement(t.ID, pl)
		if err != nil {
			return err
		}
		t.Position = pos
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res.Tasks[0], nil
}

func (x *txn) placement(self string, pl Placement) (float64, error) {
	var v sql.NullFloat64
	switch {
	case pl.Top:
		err := x.tx.QueryRow(`SELECT min(position) FROM tasks WHERE deleted_at IS NULL AND id != ?`, self).Scan(&v)
		return v.Float64 - 1, err
	case pl.Bottom:
		err := x.tx.QueryRow(`SELECT max(position) FROM tasks WHERE deleted_at IS NULL AND id != ?`, self).Scan(&v)
		return v.Float64 + 1, err
	}
	anchorID, after := pl.Before, false
	if pl.After != "" {
		anchorID, after = pl.After, true
	}
	if anchorID == self {
		return 0, fmt.Errorf("%w: cannot place a task relative to itself", ErrInvalid)
	}
	anchor, err := x.getLive(anchorID)
	if err != nil {
		return 0, err
	}
	q := `SELECT max(position) FROM tasks WHERE deleted_at IS NULL AND id != ? AND position < ?`
	if after {
		q = `SELECT min(position) FROM tasks WHERE deleted_at IS NULL AND id != ? AND position > ?`
	}
	if err := x.tx.QueryRow(q, self, anchor.Position).Scan(&v); err != nil {
		return 0, err
	}
	switch {
	case !v.Valid && after:
		return anchor.Position + 1, nil
	case !v.Valid:
		return anchor.Position - 1, nil
	}
	return (anchor.Position + v.Float64) / 2, nil
}

// mutate loads each live task, applies fn and saves it, all in one
// transaction recorded as a single undoable operation. expect optionally
// pins task versions (optimistic locking); a mismatch aborts everything.
func (s *Store) mutate(kind, summary string, ids []string, expect map[string]int64, fn func(x *txn, t *Task) error) (*Result, error) {
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: no task ids given", ErrInvalid)
	}
	res := &Result{}
	opID, err := s.write(kind, fmt.Sprintf("%s %d task(s)", summary, len(ids)), func(x *txn) error {
		for _, id := range ids {
			t, err := x.getLive(id)
			if err != nil {
				return err
			}
			if err := checkVersion(t, expect[id]); err != nil {
				return err
			}
			before := t.clone()
			if err := fn(x, t); err != nil {
				return err
			}
			changed, err := x.save(kind, before, t)
			if err != nil {
				return err
			}
			if changed {
				res.Changed++
			}
			res.Tasks = append(res.Tasks, *t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.OperationID = opID
	return res, nil
}

func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Batch actions accepted by ApplyBatch.
const (
	BatchComplete = "complete"
	BatchStart    = "start"
	BatchReopen   = "reopen"
	BatchArchive  = "archive"
	BatchDelete   = "delete"
	BatchRestore  = "restore"
	BatchPriority = "priority"
	BatchMove     = "move"
)

// BatchActions lists every ApplyBatch action.
var BatchActions = []string{BatchComplete, BatchStart, BatchReopen, BatchArchive, BatchDelete, BatchRestore, BatchPriority, BatchMove}

// BatchRequest is one action applied to many tasks in one transaction.
type BatchRequest struct {
	Action string
	IDs    []string
	// Expect optionally pins the version each task must still have; any
	// mismatch fails the whole batch with a ConflictError.
	Expect   map[string]int64
	Priority Priority   // for BatchPriority
	Move     MoveTarget // for BatchMove
}

// ApplyBatch runs a batch action atomically as one undoable operation.
func (s *Store) ApplyBatch(b BatchRequest) (*Result, error) {
	switch b.Action {
	case BatchComplete:
		return s.setStatusChecked("complete", StatusDone, b.IDs, b.Expect)
	case BatchStart:
		return s.setStatusChecked("start", StatusInProgress, b.IDs, b.Expect)
	case BatchReopen:
		return s.setStatusChecked("reopen", StatusTodo, b.IDs, b.Expect)
	case BatchArchive:
		return s.setStatusChecked("archive", StatusArchived, b.IDs, b.Expect)
	case BatchDelete:
		return s.delete(b.IDs, b.Expect)
	case BatchRestore:
		return s.restore(b.IDs, b.Expect)
	case BatchPriority:
		if !b.Priority.valid() {
			return nil, fmt.Errorf("%w: invalid priority %d", ErrInvalid, int(b.Priority))
		}
		return s.setPriority(b.Priority, b.IDs, b.Expect)
	case BatchMove:
		return s.move(b.Move, b.IDs, b.Expect)
	}
	return nil, fmt.Errorf("%w: unknown batch action %q (want %s)", ErrInvalid, b.Action, strings.Join(BatchActions, "|"))
}

// StatusAction maps a target status to the batch action that reaches it.
func StatusAction(st Status) (string, error) {
	switch st {
	case StatusTodo:
		return BatchReopen, nil
	case StatusInProgress:
		return BatchStart, nil
	case StatusDone:
		return BatchComplete, nil
	case StatusArchived:
		return BatchArchive, nil
	}
	return "", fmt.Errorf("%w: invalid status %q", ErrInvalid, st)
}
