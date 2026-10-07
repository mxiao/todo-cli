package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Fixed-width UTC timestamps sort lexicographically in SQL.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseTimePtr(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const tagSep = "\x1f"

const taskColumns = `t.id, t.title, t.description, t.due_at, t.priority, t.category, t.parent_id, t.notes,
	t.status, t.position, t.created_at, t.updated_at, t.completed_at, t.archived_at, t.deleted_at, t.version,
	(SELECT group_concat(tag, char(31)) FROM task_tags WHERE task_id = t.id)`

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (*Task, error) {
	var t Task
	var due, completed, archived, deleted, tags sql.NullString
	var created, updated string
	if err := r.Scan(&t.ID, &t.Title, &t.Description, &due, &t.Priority, &t.Category, &t.ParentID, &t.Notes,
		&t.Status, &t.Position, &created, &updated, &completed, &archived, &deleted, &t.Version, &tags); err != nil {
		return nil, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	for _, p := range []struct {
		dst **time.Time
		src sql.NullString
	}{{&t.DueAt, due}, {&t.CompletedAt, completed}, {&t.ArchivedAt, archived}, {&t.DeletedAt, deleted}} {
		if *p.dst, err = parseTimePtr(p.src); err != nil {
			return nil, err
		}
	}
	t.Tags = []string{}
	if tags.Valid && tags.String != "" {
		t.Tags = strings.Split(tags.String, tagSep)
		slices.Sort(t.Tags)
	}
	return &t, nil
}

// txn wraps one write transaction and the undo snapshots it accumulates.
type txn struct {
	s         *Store
	tx        *sql.Tx
	now       time.Time
	opID      int64
	snapshots []snapshot
	seen      map[string]bool
}

// snapshot captures a task's state before an operation; Before is nil when
// the operation created the task.
type snapshot struct {
	TaskID string `json:"task_id"`
	Before *Task  `json:"before"`
}

// write runs fn in a transaction recorded as one undoable operation. If fn
// changes nothing, no operation is recorded.
func (s *Store) write(kind, summary string, fn func(x *txn) error) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	x := &txn{s: s, tx: tx, now: s.clock(), seen: map[string]bool{}}
	// Reserve the operation row first so history can reference it.
	res, err := tx.Exec(`INSERT INTO operations(kind, actor, summary, snapshots, created_at) VALUES (?, ?, ?, '[]', ?)`,
		kind, s.actor, summary, fmtTime(x.now))
	if err != nil {
		return 0, err
	}
	if x.opID, err = res.LastInsertId(); err != nil {
		return 0, err
	}
	if err := fn(x); err != nil {
		return 0, err
	}
	if len(x.snapshots) == 0 {
		return 0, nil // rollback: nothing happened
	}
	b, err := json.Marshal(x.snapshots)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE operations SET snapshots = ? WHERE id = ?`, string(b), x.opID); err != nil {
		return 0, err
	}
	return x.opID, tx.Commit()
}

func (x *txn) get(id string) (*Task, error) {
	t, err := scanTask(x.tx.QueryRow(`SELECT `+taskColumns+` FROM tasks t WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	return t, err
}

// getLive loads a task that must not be soft-deleted.
func (x *txn) getLive(id string) (*Task, error) {
	t, err := x.get(id)
	if err != nil {
		return nil, err
	}
	if t.Deleted() {
		return nil, fmt.Errorf("%w: task %s is deleted (use restore or undo)", ErrDeleted, id)
	}
	return t, nil
}

// remember records the pre-operation state of a task once per operation.
func (x *txn) remember(id string, before *Task) {
	if x.seen[id] {
		return
	}
	x.seen[id] = true
	var b *Task
	if before != nil {
		b = before.clone()
	}
	x.snapshots = append(x.snapshots, snapshot{TaskID: id, Before: b})
}

// put upserts a task row and its tags verbatim.
func (x *txn) put(t *Task) error {
	_, err := x.tx.Exec(`INSERT INTO tasks(id, title, description, due_at, priority, category, parent_id, notes, status,
		position, created_at, updated_at, completed_at, archived_at, deleted_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, description=excluded.description, due_at=excluded.due_at,
		priority=excluded.priority, category=excluded.category, parent_id=excluded.parent_id, notes=excluded.notes,
		status=excluded.status, position=excluded.position, created_at=excluded.created_at, updated_at=excluded.updated_at,
		completed_at=excluded.completed_at, archived_at=excluded.archived_at, deleted_at=excluded.deleted_at,
		version=excluded.version`,
		t.ID, t.Title, t.Description, fmtTimePtr(t.DueAt), int(t.Priority), t.Category, t.ParentID, t.Notes, string(t.Status),
		t.Position, fmtTime(t.CreatedAt), fmtTime(t.UpdatedAt), fmtTimePtr(t.CompletedAt), fmtTimePtr(t.ArchivedAt),
		fmtTimePtr(t.DeletedAt), t.Version)
	if err != nil {
		return err
	}
	if _, err := x.tx.Exec(`DELETE FROM task_tags WHERE task_id = ?`, t.ID); err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := x.tx.Exec(`INSERT INTO task_tags(task_id, tag) VALUES (?, ?)`, t.ID, tag); err != nil {
			return err
		}
	}
	return nil
}

// save validates and persists a changed task, bumping version and recording
// history. before is nil for newly created tasks. It reports whether
// anything changed.
func (x *txn) save(action string, before, after *Task) (bool, error) {
	if err := validateTask(after); err != nil {
		return false, err
	}
	if before == nil || before.ParentID != after.ParentID {
		if err := x.checkParent(after); err != nil {
			return false, err
		}
	}
	changes := diffTasks(before, after)
	if before != nil && len(changes) == 0 {
		return false, nil
	}
	x.remember(after.ID, before)
	after.UpdatedAt = x.now
	if before != nil {
		after.Version = before.Version + 1
	}
	if err := x.put(after); err != nil {
		return false, err
	}
	return true, x.history(after.ID, action, changes)
}

func (x *txn) history(taskID, action string, changes map[string]Change) error {
	if changes == nil {
		changes = map[string]Change{}
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = x.tx.Exec(`INSERT INTO task_history(task_id, operation_id, action, actor, changes, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, x.opID, action, x.s.actor, string(b), fmtTime(x.now))
	return err
}

// checkParent ensures the parent exists, is live, and creates no cycle.
func (x *txn) checkParent(t *Task) error {
	if t.ParentID == "" {
		return nil
	}
	pid := t.ParentID
	for depth := 0; pid != ""; depth++ {
		if pid == t.ID {
			return fmt.Errorf("%w: parent %s would create a cycle", ErrInvalid, t.ParentID)
		}
		if depth > 1000 {
			return fmt.Errorf("%w: parent chain too deep", ErrInvalid)
		}
		p, err := x.get(pid)
		if err != nil {
			return fmt.Errorf("%w: parent %s does not exist", ErrInvalid, pid)
		}
		if p.Deleted() && pid == t.ParentID {
			return fmt.Errorf("%w: parent %s is deleted", ErrInvalid, pid)
		}
		pid = p.ParentID
	}
	return nil
}

func (x *txn) nextPosition() (float64, error) {
	var p sql.NullFloat64
	err := x.tx.QueryRow(`SELECT max(position) FROM tasks`).Scan(&p)
	return p.Float64 + 1, err
}

// Change is one field's before/after value in a history entry.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

func timeVal(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func strVal(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// diffTasks lists user-visible field changes; before may be nil (creation).
func diffTasks(before, after *Task) map[string]Change {
	b := before
	if b == nil {
		b = &Task{}
	}
	out := map[string]Change{}
	add := func(field string, from, to any, differ bool) {
		if differ {
			out[field] = Change{From: from, To: to}
		}
	}
	add("title", strVal(b.Title), strVal(after.Title), b.Title != after.Title)
	add("description", strVal(b.Description), strVal(after.Description), b.Description != after.Description)
	add("notes", strVal(b.Notes), strVal(after.Notes), b.Notes != after.Notes)
	add("category", strVal(b.Category), strVal(after.Category), b.Category != after.Category)
	add("parent_id", strVal(b.ParentID), strVal(after.ParentID), b.ParentID != after.ParentID)
	add("due_at", timeVal(b.DueAt), timeVal(after.DueAt), !timeEq(b.DueAt, after.DueAt))
	add("priority", b.Priority.String(), after.Priority.String(), b.Priority != after.Priority)
	add("status", strVal(string(b.Status)), string(after.Status), b.Status != after.Status)
	add("tags", nilIfEmpty(b.Tags), nilIfEmpty(after.Tags), !slices.Equal(b.Tags, after.Tags))
	add("deleted_at", timeVal(b.DeletedAt), timeVal(after.DeletedAt), !timeEq(b.DeletedAt, after.DeletedAt))
	if before != nil {
		add("position", b.Position, after.Position, b.Position != after.Position)
	}
	return out
}

func nilIfEmpty(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

func timeEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
