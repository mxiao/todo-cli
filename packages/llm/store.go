package llm

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// moduleMigrations are the llm tables in the task database (append only).
var moduleMigrations = []string{
	`
CREATE TABLE llm_sessions (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	status     TEXT NOT NULL,
	data       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_llm_sessions_kind ON llm_sessions(kind, created_at);
CREATE TABLE llm_actions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	item       INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	request    TEXT NOT NULL,
	status     TEXT NOT NULL,
	result     TEXT NOT NULL DEFAULT '{}',
	actor      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE llm_calls (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	purpose    TEXT NOT NULL,
	profile    TEXT NOT NULL,
	model      TEXT NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE llm_config_versions (
	version    INTEGER PRIMARY KEY,
	config     TEXT NOT NULL,
	source     TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
`,
}

// ErrNotFound means no such session, action or version.
var ErrNotFound = core.ErrNotFound

// ErrState rejects an operation that does not fit the current state (e.g.
// applying a rejected item).
var ErrState = errors.New("invalid_state")

func newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "s" + hex.EncodeToString(b[:])
}

func itoa(n int) string { return strconv.Itoa(n) }

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (s *Service) db() *sql.DB { return s.store.DB() }

func (s *Service) saveSession(sess *Session) error {
	sess.UpdatedAt = s.store.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = sess.UpdatedAt
	}
	if sess.Items == nil {
		sess.Items = []Item{}
	}
	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	data := Redact(string(b), s.secrets()...)
	_, err = s.db().Exec(`INSERT INTO llm_sessions(id, kind, status, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, data=excluded.data, updated_at=excluded.updated_at`,
		sess.ID, sess.Kind, sess.Status, data, fmtTime(sess.CreatedAt), fmtTime(sess.UpdatedAt))
	return err
}

// Session loads a session by id or unique id prefix.
func (s *Service) Session(id string) (*Session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: empty session id", ErrInvalid)
	}
	rows, err := s.db().Query(`SELECT data FROM llm_sessions WHERE id = ? OR id LIKE ? ORDER BY id = ? DESC LIMIT 2`, id, id+"%", id)
	if err != nil {
		return nil, err
	}
	var datas []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		datas = append(datas, d)
	}
	rows.Close()
	if len(datas) == 0 {
		return nil, fmt.Errorf("%w: model session %s", ErrNotFound, id)
	}
	var sess Session
	if err := json.Unmarshal([]byte(datas[0]), &sess); err != nil {
		return nil, err
	}
	if len(datas) > 1 && sess.ID != id {
		return nil, fmt.Errorf("%w: %q matches several sessions", core.ErrAmbiguousID, id)
	}
	return &sess, nil
}

// SessionSummary is a list entry.
type SessionSummary struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Mode      Mode      `json:"mode"`
	Input     string    `json:"input,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Items     int       `json:"items"`
	Pending   int       `json:"pending"`
	Degraded  bool      `json:"degraded,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Sessions lists recent sessions, newest first; kind "" means all.
func (s *Service) Sessions(kind string, limit int) ([]SessionSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT data FROM llm_sessions`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY created_at DESC, id LIMIT ?`
	args = append(args, limit)
	all, err := s.loadSessions(q, args...)
	if err != nil {
		return nil, err
	}
	out := []SessionSummary{}
	for _, sess := range all {
		sum := SessionSummary{ID: sess.ID, Kind: sess.Kind, Status: sess.Status, Mode: sess.Mode, Input: truncate(sess.Input, 80),
			Summary: truncate(sess.Summary, 120), Items: len(sess.Items), Degraded: sess.Degraded, CreatedAt: sess.CreatedAt}
		for _, it := range sess.Items {
			if it.Status == ItemPending {
				sum.Pending++
			}
		}
		out = append(out, sum)
	}
	return out, nil
}

func (s *Service) loadSessions(q string, args ...any) ([]Session, error) {
	rows, err := s.db().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		var sess Session
		if err := json.Unmarshal([]byte(d), &sess); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// CallRecord is one audited model call.
type CallRecord struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Purpose   string    `json:"purpose"`
	Profile   string    `json:"profile"`
	Model     string    `json:"model"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) recordCall(sessionID, purpose string, r *Resolved, latency time.Duration, err error) {
	status, msg := "ok", ""
	if err != nil {
		status = "error"
		if e, ok := AsError(err); ok {
			status = e.Kind
		}
		msg = Redact(err.Error(), s.secrets()...)
	}
	profile, model := "", ""
	if r != nil {
		profile, model = r.Name, r.Model
	}
	_, _ = s.db().Exec(`INSERT INTO llm_calls(session_id, purpose, profile, model, status, error, latency_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, purpose, profile, model, status, msg, latency.Milliseconds(), fmtTime(s.store.Now()))
}

// Calls lists recent model calls, newest first.
func (s *Service) Calls(limit int) ([]CallRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db().Query(`SELECT id, session_id, purpose, profile, model, status, error, latency_ms, created_at
		FROM llm_calls ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CallRecord{}
	for rows.Next() {
		var c CallRecord
		var created string
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Purpose, &c.Profile, &c.Model, &c.Status, &c.Error, &c.LatencyMS, &created); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, c)
	}
	return out, rows.Err()
}
