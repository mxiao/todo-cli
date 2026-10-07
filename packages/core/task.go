// Package core holds the todo-cli task model and its SQLite-backed store.
// Every entry point (CLI, TUI, web) must go through this package so that
// task identity, fields and status transitions stay identical everywhere.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Status is the lifecycle state of a task.
type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	StatusArchived   Status = "archived"
)

// Statuses lists every valid status in lifecycle order.
var Statuses = []Status{StatusTodo, StatusInProgress, StatusDone, StatusArchived}

// ParseStatus accepts canonical names and a few common aliases.
func ParseStatus(s string) (Status, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "todo", "open", "pending":
		return StatusTodo, nil
	case "in_progress", "in-progress", "doing", "progress", "started":
		return StatusInProgress, nil
	case "done", "completed", "complete":
		return StatusDone, nil
	case "archived", "archive":
		return StatusArchived, nil
	}
	return "", fmt.Errorf("%w: unknown status %q (want todo|in_progress|done|archived)", ErrInvalid, s)
}

func (s Status) valid() bool { return slices.Contains(Statuses, s) }

// Priority orders tasks by urgency; higher is more urgent.
type Priority int

const (
	PriorityNone Priority = iota
	PriorityLow
	PriorityMedium
	PriorityHigh
	PriorityUrgent
)

var priorityNames = []string{"none", "low", "medium", "high", "urgent"}

func (p Priority) String() string {
	if p < PriorityNone || p > PriorityUrgent {
		return strconv.Itoa(int(p))
	}
	return priorityNames[p]
}

func (p Priority) valid() bool { return p >= PriorityNone && p <= PriorityUrgent }

// ParsePriority accepts names (high), short forms (h, med) and numbers 0-4.
func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "", "0", "p0":
		return PriorityNone, nil
	case "low", "l", "1", "p1":
		return PriorityLow, nil
	case "medium", "med", "m", "2", "p2":
		return PriorityMedium, nil
	case "high", "h", "3", "p3":
		return PriorityHigh, nil
	case "urgent", "u", "4", "p4":
		return PriorityUrgent, nil
	}
	return 0, fmt.Errorf("%w: unknown priority %q (want none|low|medium|high|urgent)", ErrInvalid, s)
}

func (p Priority) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

func (p *Priority) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*p = Priority(n)
		if !p.valid() {
			return fmt.Errorf("%w: priority %d out of range", ErrInvalid, n)
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParsePriority(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// Task is the single canonical task record shared by all entry points.
type Task struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	DueAt       *time.Time `json:"due_at"`
	Priority    Priority   `json:"priority"`
	Tags        []string   `json:"tags"`
	Category    string     `json:"category"`
	ParentID    string     `json:"parent_id"`
	// DependsOn lists the ids of tasks that must be finished first.
	DependsOn   []string   `json:"depends_on"`
	Notes       string     `json:"notes"`
	Status      Status     `json:"status"`
	Position    float64    `json:"position"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at"`
	ArchivedAt  *time.Time `json:"archived_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
	Version     int64      `json:"version"`
}

// Deleted reports whether the task is soft-deleted.
func (t *Task) Deleted() bool { return t.DeletedAt != nil }

func (t *Task) clone() *Task {
	c := *t
	c.Tags = slices.Clone(t.Tags)
	c.DependsOn = slices.Clone(t.DependsOn)
	return &c
}

// NewTask is the input for Store.Create. Only Title is required.
type NewTask struct {
	Title       string
	Description string
	DueAt       *time.Time
	Priority    Priority
	Tags        []string
	Category    string
	ParentID    string
	DependsOn   []string
	Notes       string
	Status      Status
}

// TaskPatch describes a partial update; nil fields are left untouched.
type TaskPatch struct {
	Title       *string
	Description *string
	DueAt       *time.Time
	ClearDue    bool
	Priority    *Priority
	Tags        *[]string
	AddTags     []string
	RemoveTags  []string
	Category    *string
	ParentID    *string
	DependsOn   *[]string
	Notes       *string
	Status      *Status
	// ExpectedVersion enables optimistic locking when non-zero: the update
	// fails with ErrConflict if the stored version differs.
	ExpectedVersion int64
}

var (
	ErrNotFound          = errors.New("not_found")
	ErrAmbiguousID       = errors.New("ambiguous_id")
	ErrInvalid           = errors.New("invalid_input")
	ErrConflict          = errors.New("version_conflict")
	ErrDeleted           = errors.New("task_deleted")
	ErrNothingToUndo     = errors.New("nothing_to_undo")
	ErrSchemaTooNew      = errors.New("schema_too_new")
	ErrInvalidImportFile = errors.New("invalid_import_file")
)

// ConflictError reports a failed optimistic-lock check. It matches
// ErrConflict with errors.Is.
type ConflictError struct {
	TaskID   string
	Expected int64
	Actual   int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v: task %s is at version %d, expected %d (it was changed elsewhere; reload and retry)",
		ErrConflict, e.TaskID, e.Actual, e.Expected)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// checkVersion fails with a ConflictError when expected is set and differs
// from the task's version.
func checkVersion(t *Task, expected int64) error {
	if expected != 0 && expected != t.Version {
		return &ConflictError{TaskID: t.ID, Expected: expected, Actual: t.Version}
	}
	return nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NormalizeTags trims, strips a leading '#', drops empties and duplicates,
// and sorts so that tag sets compare deterministically.
func NormalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimPrefix(strings.TrimSpace(t), "#")
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// applyStatus moves t to status s and maintains completion/archive stamps.
func applyStatus(t *Task, s Status, now time.Time) {
	if t.Status == s {
		return
	}
	t.Status = s
	switch s {
	case StatusDone:
		t.CompletedAt = &now
		t.ArchivedAt = nil
	case StatusArchived:
		t.ArchivedAt = &now
	default:
		t.CompletedAt = nil
		t.ArchivedAt = nil
	}
}

func validateTask(t *Task) error {
	t.Title = strings.TrimSpace(t.Title)
	if t.Title == "" {
		return fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if !t.Status.valid() {
		return fmt.Errorf("%w: invalid status %q", ErrInvalid, t.Status)
	}
	if !t.Priority.valid() {
		return fmt.Errorf("%w: invalid priority %d", ErrInvalid, int(t.Priority))
	}
	if t.ParentID == t.ID && t.ID != "" {
		return fmt.Errorf("%w: a task cannot be its own parent", ErrInvalid)
	}
	t.Tags = NormalizeTags(t.Tags)
	t.Category = strings.TrimSpace(t.Category)
	t.DependsOn = normalizeIDs(t.DependsOn)
	if slices.Contains(t.DependsOn, t.ID) && t.ID != "" {
		return fmt.Errorf("%w: a task cannot depend on itself", ErrInvalid)
	}
	return nil
}

// normalizeIDs trims, drops empties and duplicates, and sorts task ids.
func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
