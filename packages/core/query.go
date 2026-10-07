package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SortField selects the list order.
type SortField string

const (
	SortManual   SortField = "manual"   // user-defined order, top first
	SortDue      SortField = "due"      // soonest first, undated last
	SortPriority SortField = "priority" // most urgent first
	SortCreated  SortField = "created"  // newest first
	SortUpdated  SortField = "updated"  // most recently updated first
)

// ParseSortField validates a sort name.
func ParseSortField(s string) (SortField, error) {
	switch f := SortField(strings.ToLower(strings.TrimSpace(s))); f {
	case "":
		return SortManual, nil
	case SortManual, SortDue, SortPriority, SortCreated, SortUpdated:
		return f, nil
	}
	return "", fmt.Errorf("%w: unknown sort %q (want manual|due|priority|created|updated)", ErrInvalid, s)
}

// Filter narrows List results. Zero values mean "no constraint". Without a
// status filter, archived tasks are hidden unless IncludeArchived is set.
type Filter struct {
	Query           string
	Statuses        []Status
	Priorities      []Priority
	MinPriority     *Priority
	Tags            []string // task must carry all of them
	Category        string
	ParentID        *string // pointer to "" lists top-level tasks
	DueBefore       *time.Time
	DueAfter        *time.Time
	Overdue         bool
	HasDue          *bool
	IncludeArchived bool
	IncludeDeleted  bool
	OnlyDeleted     bool
	Sort            SortField
	Reverse         bool
	Limit           int
}

// List returns tasks matching f in the requested order.
func (s *Store) List(f Filter) ([]Task, error) {
	var where []string
	var args []any
	switch {
	case f.OnlyDeleted:
		where = append(where, "t.deleted_at IS NOT NULL")
	case !f.IncludeDeleted:
		where = append(where, "t.deleted_at IS NULL")
	}
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "t.status IN ("+strings.Join(ph, ",")+")")
	} else if !f.IncludeArchived {
		where = append(where, "t.status != 'archived'")
	}
	if len(f.Priorities) > 0 {
		ph := make([]string, len(f.Priorities))
		for i, p := range f.Priorities {
			ph[i] = "?"
			args = append(args, int(p))
		}
		where = append(where, "t.priority IN ("+strings.Join(ph, ",")+")")
	}
	if f.MinPriority != nil {
		where = append(where, "t.priority >= ?")
		args = append(args, int(*f.MinPriority))
	}
	for _, tag := range NormalizeTags(f.Tags) {
		where = append(where, "EXISTS (SELECT 1 FROM task_tags g WHERE g.task_id = t.id AND g.tag = ?)")
		args = append(args, tag)
	}
	if c := strings.TrimSpace(f.Category); c != "" {
		where = append(where, "t.category = ?")
		args = append(args, c)
	}
	if f.ParentID != nil {
		where = append(where, "t.parent_id = ?")
		args = append(args, *f.ParentID)
	}
	if f.DueBefore != nil {
		where = append(where, "t.due_at IS NOT NULL AND t.due_at <= ?")
		args = append(args, fmtTime(*f.DueBefore))
	}
	if f.DueAfter != nil {
		where = append(where, "t.due_at IS NOT NULL AND t.due_at >= ?")
		args = append(args, fmtTime(*f.DueAfter))
	}
	if f.Overdue {
		where = append(where, "t.due_at IS NOT NULL AND t.due_at < ? AND t.status IN ('todo','in_progress')")
		args = append(args, fmtTime(s.clock()))
	}
	if f.HasDue != nil {
		if *f.HasDue {
			where = append(where, "t.due_at IS NOT NULL")
		} else {
			where = append(where, "t.due_at IS NULL")
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		for _, term := range strings.Fields(q) {
			like := "%" + escapeLike(term) + "%"
			where = append(where, `(t.title LIKE ? ESCAPE '\' OR t.description LIKE ? ESCAPE '\' OR t.notes LIKE ? ESCAPE '\'
				OR t.category LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM task_tags g WHERE g.task_id = t.id AND g.tag LIKE ? ESCAPE '\'))`)
			args = append(args, like, like, like, like, like)
		}
	}

	sql := `SELECT ` + taskColumns + ` FROM tasks t`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY " + orderBy(f.Sort, f.Reverse)
	if f.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func orderBy(f SortField, reverse bool) string {
	dir := func(natural string) string {
		if !reverse {
			return natural
		}
		if natural == "ASC" {
			return "DESC"
		}
		return "ASC"
	}
	var primary string
	switch f {
	case SortDue:
		// Undated tasks always go last, regardless of direction.
		primary = "t.due_at IS NULL, t.due_at " + dir("ASC")
	case SortPriority:
		primary = "t.priority " + dir("DESC") + ", t.due_at IS NULL, t.due_at ASC"
	case SortCreated:
		primary = "t.created_at " + dir("DESC")
	case SortUpdated:
		primary = "t.updated_at " + dir("DESC")
	default:
		primary = "t.position " + dir("ASC")
	}
	return primary + ", t.position ASC, t.created_at ASC, t.id ASC"
}

// HistoryEntry is one recorded change to a task.
type HistoryEntry struct {
	ID          int64             `json:"id"`
	TaskID      string            `json:"task_id"`
	OperationID int64             `json:"operation_id,omitempty"`
	Action      string            `json:"action"`
	Actor       string            `json:"actor"`
	Changes     map[string]Change `json:"changes"`
	CreatedAt   time.Time         `json:"created_at"`
}

// History returns a task's change log, oldest first.
func (s *Store) History(taskID string) ([]HistoryEntry, error) {
	return s.history(`WHERE task_id = ?`, taskID)
}

func (s *Store) history(where string, args ...any) ([]HistoryEntry, error) {
	return s.historyLimit(where, 0, args...)
}

func (s *Store) historyLimit(where string, limit int, args ...any) ([]HistoryEntry, error) {
	q := `SELECT id, task_id, coalesce(operation_id, 0), action, actor, changes, created_at
		FROM task_history ` + where + ` ORDER BY id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		var changes, created string
		if err := rows.Scan(&h.ID, &h.TaskID, &h.OperationID, &h.Action, &h.Actor, &changes, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(changes), &h.Changes); err != nil {
			return nil, fmt.Errorf("history %d: %w", h.ID, err)
		}
		if h.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
