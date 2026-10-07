package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Revision is a store-wide, monotonically increasing change counter: the id
// of the newest history entry. Every committed task change (from any
// process) raises it, so clients can ask for ChangesSince(revision).
func (s *Store) Revision() (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT max(id) FROM task_history`).Scan(&v)
	return v.Int64, err
}

// ChangesSince returns up to limit history entries newer than revision,
// oldest first. Each entry's ID is its revision.
func (s *Store) ChangesSince(revision int64, limit int) ([]HistoryEntry, error) {
	return s.historyLimit(`WHERE id > ?`, limit, revision)
}

// TaskVersion is the full state of a task at one version.
type TaskVersion struct {
	Version   int64     `json:"version"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
	Task      Task      `json:"task"`
}

// TaskVersions lists the stored versions of a task, oldest first. Versions
// written before schema v3 are not available, except the live one.
func (s *Store) TaskVersions(id string) ([]TaskVersion, error) {
	rows, err := s.db.Query(`SELECT version, actor, created_at, snapshot FROM task_versions WHERE task_id = ? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskVersion{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Tasks last written before schema v3 have no snapshot of their live version.
	cur, err := s.Get(id)
	if err != nil {
		if len(out) > 0 && errors.Is(err, ErrNotFound) {
			return out, nil // removed by undoing its creation
		}
		return nil, err
	}
	if len(out) == 0 || out[len(out)-1].Version < cur.Version {
		out = append(out, TaskVersion{Version: cur.Version, CreatedAt: cur.UpdatedAt, Task: *cur})
	}
	return out, nil
}

// GetVersion returns a task as it was at the given version.
func (s *Store) GetVersion(id string, version int64) (*Task, error) {
	v, err := scanVersion(s.db.QueryRow(`SELECT version, actor, created_at, snapshot FROM task_versions WHERE task_id = ? AND version = ?`, id, version))
	if err == nil {
		return &v.Task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if cur, err := s.Get(id); err == nil && cur.Version == version {
		return cur, nil
	}
	return nil, fmt.Errorf("%w: task %s has no stored version %d", ErrNotFound, id, version)
}

func scanVersion(r scanner) (*TaskVersion, error) {
	var v TaskVersion
	var created, snap string
	if err := r.Scan(&v.Version, &v.Actor, &created, &snap); err != nil {
		return nil, err
	}
	var err error
	if v.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(snap), &v.Task); err != nil {
		return nil, fmt.Errorf("version %d: corrupt snapshot: %w", v.Version, err)
	}
	if v.Task.Tags == nil {
		v.Task.Tags = []string{}
	}
	return &v, nil
}

// Conflict resolutions.
const (
	ResolveMine   = "mine"   // the rejected edit was applied on top of the current version
	ResolveTheirs = "theirs" // the rejected edit was discarded
)

var ErrConflictResolved = errors.New("conflict_resolved")

// Conflict is a write rejected by optimistic locking, kept so the user can
// review it and apply or discard it later instead of losing it.
type Conflict struct {
	ID             int64           `json:"id"`
	TaskID         string          `json:"task_id"`
	BaseVersion    int64           `json:"base_version"`
	CurrentVersion int64           `json:"current_version"`
	Actor          string          `json:"actor"`
	Patch          json.RawMessage `json:"patch"`
	CreatedAt      time.Time       `json:"created_at"`
	ResolvedAt     *time.Time      `json:"resolved_at"`
	Resolution     string          `json:"resolution,omitempty"`
}

// RecordConflict stores a rejected edit. patch is the edit as submitted
// (see DecodePatch).
func (s *Store) RecordConflict(taskID string, baseVersion, currentVersion int64, patch json.RawMessage) (*Conflict, error) {
	if !json.Valid(patch) {
		return nil, fmt.Errorf("%w: conflict patch is not valid JSON", ErrInvalid)
	}
	now := s.clock()
	res, err := s.db.Exec(`INSERT INTO conflicts(task_id, base_version, current_version, actor, patch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, baseVersion, currentVersion, s.actor, string(patch), fmtTime(now))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.Conflict(id)
}

const conflictColumns = `id, task_id, base_version, current_version, actor, patch, created_at, resolved_at, resolution`

// Conflict returns one stored conflict.
func (s *Store) Conflict(id int64) (*Conflict, error) {
	c, err := scanConflict(s.db.QueryRow(`SELECT `+conflictColumns+` FROM conflicts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: conflict %d", ErrNotFound, id)
	}
	return c, err
}

// Conflicts lists a task's conflicts, oldest first; resolved ones only when
// includeResolved is set.
func (s *Store) Conflicts(taskID string, includeResolved bool) ([]Conflict, error) {
	q := `SELECT ` + conflictColumns + ` FROM conflicts WHERE task_id = ?`
	if !includeResolved {
		q += ` AND resolved_at IS NULL`
	}
	rows, err := s.db.Query(q+` ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conflict{}
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// MarkConflictResolved closes an open conflict with ResolveMine or
// ResolveTheirs.
func (s *Store) MarkConflictResolved(id int64, resolution string) (*Conflict, error) {
	if resolution != ResolveMine && resolution != ResolveTheirs {
		return nil, fmt.Errorf("%w: unknown resolution %q (want %s|%s)", ErrInvalid, resolution, ResolveMine, ResolveTheirs)
	}
	res, err := s.db.Exec(`UPDATE conflicts SET resolved_at = ?, resolution = ? WHERE id = ? AND resolved_at IS NULL`,
		fmtTime(s.clock()), resolution, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c, err := s.Conflict(id)
		if err != nil {
			return nil, err
		}
		return c, fmt.Errorf("%w: conflict %d was already resolved (%s)", ErrConflictResolved, id, c.Resolution)
	}
	return s.Conflict(id)
}

func scanConflict(r scanner) (*Conflict, error) {
	var c Conflict
	var patch, created string
	var resolved sql.NullString
	if err := r.Scan(&c.ID, &c.TaskID, &c.BaseVersion, &c.CurrentVersion, &c.Actor, &patch, &created, &resolved, &c.Resolution); err != nil {
		return nil, err
	}
	c.Patch = json.RawMessage(patch)
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if c.ResolvedAt, err = parseTimePtr(resolved); err != nil {
		return nil, err
	}
	return &c, nil
}

// DiffTasks lists user-visible field changes from a to b; a may be nil.
func DiffTasks(a, b *Task) map[string]Change { return diffTasks(a, b) }

// FieldValues returns the user-visible fields of t in the same form as
// history changes, for side-by-side comparisons (e.g. conflict views).
func FieldValues(t *Task) map[string]any {
	return map[string]any{
		"title": strVal(t.Title), "description": strVal(t.Description), "notes": strVal(t.Notes),
		"category": strVal(t.Category), "parent_id": strVal(t.ParentID), "due_at": timeVal(t.DueAt),
		"priority": t.Priority.String(), "status": strVal(string(t.Status)), "tags": nilIfEmpty(t.Tags),
		"deleted_at": timeVal(t.DeletedAt),
	}
}

// Revert restores the editable fields of a task (title, description, notes,
// category, parent, due, priority, tags, status) to a stored earlier
// version, as a new version. expected enables optimistic locking.
func (s *Store) Revert(id string, version, expected int64) (*Task, error) {
	old, err := s.GetVersion(id, version)
	if err != nil {
		return nil, err
	}
	res, err := s.mutate("revert", fmt.Sprintf("revert to v%d", version), []string{id}, nil, func(x *txn, t *Task) error {
		if err := checkVersion(t, expected); err != nil {
			return err
		}
		t.Title, t.Description, t.Notes, t.Category, t.ParentID = old.Title, old.Description, old.Notes, old.Category, old.ParentID
		t.DueAt, t.Priority, t.Tags = old.DueAt, old.Priority, slices.Clone(old.Tags)
		applyStatus(t, old.Status, x.now)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res.Tasks[0], nil
}
