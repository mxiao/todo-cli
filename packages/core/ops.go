package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WithActor returns a view of the store that records changes under another
// history actor (e.g. "llm" for model-driven changes) over the same
// database. Close only the original store.
func (s *Store) WithActor(actor string) *Store {
	c := *s
	c.actor = actor
	c.LastMigration = nil
	return &c
}

// Now is the store clock in UTC.
func (s *Store) Now() time.Time { return s.clock() }

// DB exposes the database to packages that keep their own tables in it
// (see MigrateModule). Task tables must only be changed through Store.
func (s *Store) DB() *sql.DB { return s.db }

// MigrateModule applies a module's own append-only schema migrations
// (migrations[i] upgrades version i to i+1) in one transaction, tracking
// the applied version per module.
func (s *Store) MigrateModule(module string, migrations []string) error {
	if s.schema < 4 {
		return fmt.Errorf("%w: module schemas need database schema v4", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur sql.NullInt64
	if err := tx.QueryRow(`SELECT max(version) FROM module_migrations WHERE module = ?`, module).Scan(&cur); err != nil {
		return err
	}
	if int(cur.Int64) > len(migrations) {
		return fmt.Errorf("%w: %s schema v%d is newer than this build supports (v%d); upgrade todo-cli",
			ErrSchemaTooNew, module, cur.Int64, len(migrations))
	}
	if int(cur.Int64) == len(migrations) {
		return nil
	}
	for v := int(cur.Int64) + 1; v <= len(migrations); v++ {
		if _, err := tx.Exec(migrations[v-1]); err != nil {
			return fmt.Errorf("%s migration v%d failed (rolled back): %w", module, v, err)
		}
		if _, err := tx.Exec(`INSERT INTO module_migrations(module, version, applied_at) VALUES (?, ?, ?)`,
			module, v, fmtTime(s.clock())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Tx is a multi-step write: every change made through it is one atomic,
// undoable operation (see Store.Do).
type Tx struct {
	x  *txn
	sp int
}

// Do runs fn as a single transaction recorded as one undoable operation of
// the given kind. It returns the operation id, or 0 when nothing changed.
func (s *Store) Do(kind, summary string, fn func(tx *Tx) error) (int64, error) {
	return s.write(kind, summary, func(x *txn) error { return fn(&Tx{x: x}) })
}

// Now is the transaction timestamp.
func (t *Tx) Now() time.Time { return t.x.now }

// Get loads a task (including soft-deleted ones).
func (t *Tx) Get(id string) (*Task, error) { return t.x.get(id) }

// Create inserts a new task.
func (t *Tx) Create(in NewTask) (*Task, error) {
	pos, err := t.x.nextPosition()
	if err != nil {
		return nil, err
	}
	task := &Task{
		ID: newID(), Title: in.Title, Description: in.Description, DueAt: in.DueAt, Priority: in.Priority,
		Tags: in.Tags, Category: in.Category, ParentID: in.ParentID, DependsOn: in.DependsOn, Notes: in.Notes,
		Status: in.Status, Position: pos, CreatedAt: t.x.now, Version: 1,
	}
	if task.Status == "" {
		task.Status = StatusTodo
	}
	applyStatusStamps(task, t.x.now)
	if _, err := t.x.save("create", nil, task); err != nil {
		return nil, err
	}
	return task, nil
}

// Update applies a patch to a live task.
func (t *Tx) Update(id string, p TaskPatch) (*Task, error) {
	task, err := t.x.getLive(id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(task, p.ExpectedVersion); err != nil {
		return nil, err
	}
	before := task.clone()
	if err := ApplyPatch(task, p, t.x.now); err != nil {
		return nil, err
	}
	if _, err := t.x.save("update", before, task); err != nil {
		return nil, err
	}
	return task, nil
}

// Reorder moves a live task within the manual order.
func (t *Tx) Reorder(id string, pl Placement) (*Task, error) {
	task, err := t.x.getLive(id)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(task, pl.ExpectedVersion); err != nil {
		return nil, err
	}
	before := task.clone()
	if task.Position, err = t.x.placement(task.ID, pl); err != nil {
		return nil, err
	}
	if _, err := t.x.save("reorder", before, task); err != nil {
		return nil, err
	}
	return task, nil
}

// Delete soft-deletes a live task.
func (t *Tx) Delete(id string) (*Task, error) {
	task, err := t.x.getLive(id)
	if err != nil {
		return nil, err
	}
	before := task.clone()
	task.DeletedAt = &t.x.now
	if _, err := t.x.save("delete", before, task); err != nil {
		return nil, err
	}
	return task, nil
}

// Try runs one step of a multi-step write under a savepoint: when fn fails,
// its changes are rolled back and the rest of the transaction carries on.
func (t *Tx) Try(fn func() error) error {
	t.sp++
	name := fmt.Sprintf("step_%d", t.sp)
	if _, err := t.x.tx.Exec(`SAVEPOINT ` + name); err != nil {
		return err
	}
	nSnap := len(t.x.snapshots)
	err := fn()
	if err == nil {
		_, err = t.x.tx.Exec(`RELEASE ` + name)
		return err
	}
	if _, rerr := t.x.tx.Exec(`ROLLBACK TO ` + name); rerr != nil {
		return errors.Join(err, rerr)
	}
	if _, rerr := t.x.tx.Exec(`RELEASE ` + name); rerr != nil {
		return errors.Join(err, rerr)
	}
	for _, sn := range t.x.snapshots[nSnap:] {
		delete(t.x.seen, sn.TaskID)
	}
	t.x.snapshots = t.x.snapshots[:nSnap]
	return err
}

// OperationInfo describes a recorded operation.
type OperationInfo struct {
	Operation
	CreatedAt time.Time  `json:"created_at"`
	UndoneAt  *time.Time `json:"undone_at"`
	TaskIDs   []string   `json:"task_ids"`
}

// OperationByID returns one recorded operation.
func (s *Store) OperationByID(id int64) (*OperationInfo, error) {
	var op OperationInfo
	var raw, created string
	var undone sql.NullString
	err := s.db.QueryRow(`SELECT id, kind, actor, summary, snapshots, created_at, undone_at FROM operations WHERE id = ?`, id).
		Scan(&op.ID, &op.Kind, &op.Actor, &op.Summary, &raw, &created, &undone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: operation %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var snaps []snapshot
	if err := json.Unmarshal([]byte(raw), &snaps); err != nil {
		return nil, fmt.Errorf("operation %d: corrupt snapshot: %w", id, err)
	}
	op.TaskIDs = []string{}
	for _, sn := range snaps {
		op.TaskIDs = append(op.TaskIDs, sn.TaskID)
	}
	if op.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if op.UndoneAt, err = parseTimePtr(undone); err != nil {
		return nil, err
	}
	return &op, nil
}

// ErrAlreadyUndone means the operation was undone before.
var ErrAlreadyUndone = errors.New("already_undone")

// UndoOperation reverts one specific operation (e.g. the changes of one
// model session), as long as none of its tasks changed after it in a way
// that has not been undone; otherwise it fails with ErrConflict.
func (s *Store) UndoOperation(id int64) (*UndoResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var op Operation
	var raw string
	var undone sql.NullString
	err = tx.QueryRow(`SELECT id, kind, actor, summary, snapshots, undone_at FROM operations WHERE id = ?`, id).
		Scan(&op.ID, &op.Kind, &op.Actor, &op.Summary, &raw, &undone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: operation %d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if undone.Valid {
		return nil, fmt.Errorf("%w: operation %d (%s) was already undone", ErrAlreadyUndone, id, op.Summary)
	}
	var snaps []snapshot
	if err := json.Unmarshal([]byte(raw), &snaps); err != nil {
		return nil, fmt.Errorf("operation %d: corrupt snapshot: %w", op.ID, err)
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("%w: operation %d changed nothing", ErrNothingToUndo, id)
	}
	ids := make([]string, len(snaps))
	args := []any{op.ID}
	for i, sn := range snaps {
		ids[i] = "?"
		args = append(args, sn.TaskID)
	}
	var later sql.NullString
	err = tx.QueryRow(`SELECT min(h.task_id) FROM task_history h JOIN operations o ON o.id = h.operation_id
		WHERE h.operation_id > ? AND o.undone_at IS NULL AND h.task_id IN (`+strings.Join(ids, ",")+`)`, args...).Scan(&later)
	if err != nil {
		return nil, err
	}
	if later.Valid {
		return nil, fmt.Errorf("%w: task %s changed after operation %d; undo or revert the later change first",
			ErrConflict, later.String, op.ID)
	}
	res, err := s.undoSnapshots(tx, op, snaps)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}
