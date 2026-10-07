package agent

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// moduleMigrations are the agent tables in the task database (append only).
var moduleMigrations = []string{
	`
CREATE TABLE agent_runs (
	id              TEXT PRIMARY KEY,
	task_id         TEXT NOT NULL,
	agent           TEXT NOT NULL,
	adapter         TEXT NOT NULL,
	status          TEXT NOT NULL,
	attempt         INTEGER NOT NULL DEFAULT 0,
	active          INTEGER NOT NULL DEFAULT 0,
	initiator       TEXT NOT NULL,
	selection       TEXT NOT NULL DEFAULT '{}',
	session_id      TEXT NOT NULL DEFAULT '',
	item            INTEGER NOT NULL DEFAULT 0,
	prompt          TEXT NOT NULL,
	input           TEXT NOT NULL,
	spec            TEXT NOT NULL,
	options         TEXT NOT NULL DEFAULT '{}',
	control         TEXT NOT NULL DEFAULT '',
	stage           TEXT NOT NULL DEFAULT '',
	progress        REAL NOT NULL DEFAULT 0,
	error           TEXT NOT NULL DEFAULT '',
	error_kind      TEXT NOT NULL DEFAULT '',
	exit_code       INTEGER,
	owner_pid       INTEGER NOT NULL DEFAULT 0,
	next_attempt_at TEXT,
	paused_at       TEXT,
	paused_ms       INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT NOT NULL,
	started_at      TEXT,
	ended_at        TEXT,
	updated_at      TEXT NOT NULL
);
CREATE INDEX idx_agent_runs_task ON agent_runs(task_id, created_at);
CREATE INDEX idx_agent_runs_status ON agent_runs(status);
CREATE TABLE agent_attempts (
	run_id      TEXT NOT NULL,
	attempt     INTEGER NOT NULL,
	status      TEXT NOT NULL,
	started_at  TEXT NOT NULL,
	ended_at    TEXT,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	exit_code   INTEGER,
	error       TEXT NOT NULL DEFAULT '',
	error_kind  TEXT NOT NULL DEFAULT '',
	results     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, attempt)
);
CREATE TABLE agent_events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id     TEXT NOT NULL,
	attempt    INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	stream     TEXT NOT NULL DEFAULT '',
	message    TEXT NOT NULL,
	data       TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL
);
CREATE INDEX idx_agent_events_run ON agent_events(run_id, id);
CREATE TABLE agent_results (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    TEXT NOT NULL,
	run_id     TEXT NOT NULL,
	attempt    INTEGER NOT NULL DEFAULT 0,
	type       TEXT NOT NULL,
	idem_key   TEXT NOT NULL,
	version    INTEGER NOT NULL,
	agent      TEXT NOT NULL,
	summary    TEXT NOT NULL DEFAULT '',
	data       TEXT NOT NULL,
	hash       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE (idem_key, version)
);
CREATE INDEX idx_agent_results_task ON agent_results(task_id, id);
CREATE INDEX idx_agent_results_run ON agent_results(run_id, id);
`,
}

func newRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "r" + hex.EncodeToString(b[:])
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTimePtr(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func agentActor(name string) string {
	if name == "" {
		return "agent"
	}
	return "agent/" + name
}

func (m *Manager) db() *sql.DB { return m.store.DB() }

const runColumns = `id, task_id, agent, adapter, status, attempt, active, initiator, selection, session_id, item, prompt, input, spec,
	options, control, stage, progress, error, error_kind, exit_code, owner_pid, next_attempt_at, paused_at, paused_ms,
	created_at, started_at, ended_at, updated_at`

// runRow is a run with the bookkeeping columns the API does not show.
type runRow struct {
	Run
	active   bool
	pausedAt *time.Time
}

func scanRun(sc interface{ Scan(...any) error }) (*runRow, error) {
	var r runRow
	var sel, input, spec, opts, created, updated string
	var exit sql.NullInt64
	var next, pausedAt, started, ended sql.NullString
	if err := sc.Scan(&r.ID, &r.TaskID, &r.Agent, &r.Adapter, &r.Status, &r.Attempt, &r.active, &r.Initiator, &sel, &r.SessionID,
		&r.Item, &r.Prompt, &input, &spec, &opts, &r.Control, &r.Stage, &r.Progress, &r.Error, &r.ErrorKind, &exit, &r.OwnerPID,
		&next, &pausedAt, &r.PausedMS, &created, &started, &ended, &updated); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(sel), &r.Selection)
	r.Input = &Input{}
	_ = json.Unmarshal([]byte(input), r.Input)
	_ = json.Unmarshal([]byte(spec), &r.Spec)
	_ = json.Unmarshal([]byte(opts), &r.Options)
	if exit.Valid {
		c := int(exit.Int64)
		r.ExitCode = &c
	}
	r.NextAt, r.pausedAt = parseTimePtr(next), parseTimePtr(pausedAt)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	r.StartedAt, r.EndedAt = parseTimePtr(started), parseTimePtr(ended)
	r.StatusLabel = StatusLabel(r.Status)
	return &r, nil
}

func (m *Manager) loadRow(id string) (*runRow, error) {
	r, err := scanRun(m.db().QueryRow(`SELECT `+runColumns+` FROM agent_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: agent run %s", ErrNotFound, id)
	}
	return r, err
}

// ResolveRun expands a unique run id prefix.
func (m *Manager) ResolveRun(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("%w: empty run id", ErrInvalid)
	}
	rows, err := m.db().Query(`SELECT id FROM agent_runs WHERE id = ? OR id LIKE ? ORDER BY id = ? DESC LIMIT 2`,
		ref, strings.ReplaceAll(ref, "%", "")+"%", ref)
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	switch {
	case len(ids) == 0:
		return "", fmt.Errorf("%w: agent run %s", ErrNotFound, ref)
	case len(ids) > 1 && ids[0] != ref:
		return "", fmt.Errorf("%w: %q matches several runs", core.ErrAmbiguousID, ref)
	}
	return ids[0], nil
}

// Get loads a run with its attempts and results.
func (m *Manager) Get(id string) (*Run, error) {
	id, err := m.ResolveRun(id)
	if err != nil {
		return nil, err
	}
	r, err := m.loadRow(id)
	if err != nil {
		return nil, err
	}
	if r.Attempts, err = m.attempts(id); err != nil {
		return nil, err
	}
	if r.Results, err = m.RunResults(id); err != nil {
		return nil, err
	}
	m.fillDuration(r)
	return &r.Run, nil
}

// fillDuration sums the attempts' running time without pauses.
func (m *Manager) fillDuration(r *runRow) {
	now := m.store.Now()
	var total int64
	for _, a := range r.Attempts {
		if a.EndedAt != nil {
			total += a.DurationMS
		} else if a.Attempt == r.Attempt && r.active {
			d := now.Sub(a.StartedAt).Milliseconds() - r.PausedMS
			if r.pausedAt != nil {
				d -= now.Sub(*r.pausedAt).Milliseconds()
			}
			total += max(d, 0)
		}
	}
	r.DurationMS = total
}

func (m *Manager) attempts(runID string) ([]Attempt, error) {
	rows, err := m.db().Query(`SELECT attempt, status, started_at, ended_at, duration_ms, exit_code, error, error_kind, results
		FROM agent_attempts WHERE run_id = ? ORDER BY attempt`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var started string
		var ended sql.NullString
		var exit sql.NullInt64
		if err := rows.Scan(&a.Attempt, &a.Status, &started, &ended, &a.DurationMS, &exit, &a.Error, &a.ErrorKind, &a.Results); err != nil {
			return nil, err
		}
		a.StartedAt, a.EndedAt = parseTime(started), parseTimePtr(ended)
		if exit.Valid {
			c := int(exit.Int64)
			a.ExitCode = &c
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListFilter narrows List.
type ListFilter struct {
	TaskID   string
	Statuses []string
	Limit    int
}

// List returns runs, newest first, without their logs.
func (m *Manager) List(f ListFilter) ([]Run, error) {
	q := `SELECT ` + runColumns + ` FROM agent_runs`
	var where []string
	var args []any
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "status IN (?"+strings.Repeat(", ?", len(f.Statuses)-1)+")")
		for _, s := range f.Statuses {
			args = append(args, s)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q += " ORDER BY created_at DESC, id LIMIT ?"
	args = append(args, f.Limit)
	rows, err := m.db().Query(q, args...)
	if err != nil {
		return nil, err
	}
	var list []*runRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(list))
	for _, r := range list {
		if r.Attempts, err = m.attempts(r.ID); err != nil {
			return nil, err
		}
		m.fillDuration(r)
		r.Attempts = nil
		r.Input = nil
		out = append(out, r.Run)
	}
	return out, nil
}

// EventFilter narrows Events.
type EventFilter struct {
	// After returns only events with a larger id (for following a log).
	After   int64
	Attempt int
	Kinds   []string
	Limit   int
}

// Events returns a run's log, oldest first (FR-503).
func (m *Manager) Events(runID string, f EventFilter) ([]Event, error) {
	runID, err := m.ResolveRun(runID)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, run_id, attempt, kind, stream, message, data, created_at FROM agent_events WHERE run_id = ? AND id > ?`
	args := []any{runID, f.After}
	if f.Attempt > 0 {
		q += ` AND attempt = ?`
		args = append(args, f.Attempt)
	}
	if len(f.Kinds) > 0 {
		q += ` AND kind IN (?` + strings.Repeat(", ?", len(f.Kinds)-1) + `)`
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.Limit <= 0 || f.Limit > 10000 {
		f.Limit = 10000
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, f.Limit)
	rows, err := m.db().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var data, created string
		if err := rows.Scan(&e.ID, &e.RunID, &e.Attempt, &e.Kind, &e.Stream, &e.Message, &data, &created); err != nil {
			return nil, err
		}
		if data != "{}" {
			_ = json.Unmarshal([]byte(data), &e.Data)
		}
		e.At = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// addEvent stores one redacted log entry.
func (m *Manager) addEvent(runID string, attempt int, kind, stream, msg string, data map[string]any, secrets []string) {
	msg = clip(m.redact(msg, secrets), maxEventBytes)
	b := []byte("{}")
	if len(data) > 0 {
		b, _ = json.Marshal(m.redactMap(data, secrets))
	}
	_, _ = m.db().Exec(`INSERT INTO agent_events(run_id, attempt, kind, stream, message, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, attempt, kind, stream, msg, string(b), fmtTime(m.store.Now()))
}
