package agent

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Result types (FR-508).
const (
	ResultText          = "text"           // text answer or report
	ResultFile          = "file"           // a file path with its summary
	ResultCommit        = "commit"         // repository, branch and commit hash
	ResultCommandOutput = "command_output" // a command line with exit code and output
	ResultData          = "data"           // structured JSON data
	ResultTaskStatus    = "task_status"    // the status of an external task (and optionally of this task)
)

// ResultTypes lists every result type.
var ResultTypes = []string{ResultText, ResultFile, ResultCommit, ResultCommandOutput, ResultData, ResultTaskStatus}

// ManualRun is the run id of results written back without a run (through
// the API or `todo agent result add`).
const ManualRun = "manual"

// Result is one written-back result. Results are append-only: writing the
// same idempotency key again with identical content returns the stored
// result, different content becomes a new version and earlier versions
// stay (FR-509).
type Result struct {
	ID      int64  `json:"id"`
	TaskID  string `json:"task_id"`
	RunID   string `json:"run_id"`
	Attempt int    `json:"attempt,omitempty"`
	Type    string `json:"type"`
	// Key is the idempotency key: task id, run id and result type, plus the
	// agent's own key when one run writes several results of a type.
	Key     string         `json:"key"`
	Version int            `json:"version"`
	Agent   string         `json:"agent"`
	Summary string         `json:"summary"`
	Data    map[string]any `json:"data"`
	Hash    string         `json:"hash"`
	// Duplicate is set on a write that matched an existing version.
	Duplicate bool      `json:"duplicate,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ResultKey is the idempotency key of a result.
func ResultKey(taskID, runID, typ, key string) string {
	k := taskID + ":" + runID + ":" + typ
	if key != "" {
		k += ":" + key
	}
	return k
}

var (
	commitRE = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	keyRE    = regexp.MustCompile(`^[\pL\pN._/@#:+\-]{1,120}$`)
)

func normType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "text", "message", "summary", "文本":
		return ResultText
	case "file", "files", "path", "文件":
		return ResultFile
	case "commit", "git_commit", "提交":
		return ResultCommit
	case "command_output", "command", "cli", "output", "命令输出":
		return ResultCommandOutput
	case "data", "json", "structured", "结构化数据":
		return ResultData
	case "task_status", "status", "external_status", "任务状态":
		return ResultTaskStatus
	}
	return ""
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return fmt.Sprint(v)
		}
	}
	return ""
}

func strList(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = x
	case string:
		out = strings.Fields(x)
	}
	return out
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// NormalizeResult validates a result message and returns its type,
// summary and stored fields. Relative file paths resolve against dir.
func NormalizeResult(raw map[string]any, dir string) (typ, key, summary string, data map[string]any, err error) {
	typ = normType(str(raw, "result_type", "kind", "result"))
	if typ == "" {
		return "", "", "", nil, fmt.Errorf("%w: unknown result type %q (want %s)", ErrInvalid, str(raw, "result_type", "kind"), strings.Join(ResultTypes, ", "))
	}
	key = str(raw, "key")
	if key != "" && !keyRE.MatchString(key) {
		return "", "", "", nil, fmt.Errorf("%w: result key %q may only use letters, digits and ._/@#:+-", ErrInvalid, key)
	}
	summary = str(raw, "summary")
	data = map[string]any{}
	switch typ {
	case ResultText:
		text := str(raw, "text", "content", "message")
		if text == "" {
			return "", "", "", nil, fmt.Errorf("%w: a text result needs text", ErrInvalid)
		}
		data["text"] = clip(text, 256<<10)
		if summary == "" {
			summary = firstLine(text, 120)
		}
	case ResultFile:
		p := str(raw, "path", "file")
		if p == "" {
			return "", "", "", nil, fmt.Errorf("%w: a file result needs a path", ErrInvalid)
		}
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		if !filepath.IsAbs(p) && dir != "" {
			p = filepath.Join(dir, p)
		}
		p = filepath.Clean(p)
		data["path"] = p
		if rel, err := filepath.Rel(dir, p); dir != "" && (err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			// Recorded for review: the agent wrote outside its working directory.
			data["outside_workdir"] = true
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			data["exists"], data["size"] = true, fi.Size()
			data["modified_at"] = fi.ModTime().UTC().Format(time.RFC3339)
			if fi.Size() <= 64<<20 {
				if f, err := os.Open(p); err == nil {
					h := sha256.New()
					if _, err := io.Copy(h, f); err == nil {
						data["sha256"] = hex.EncodeToString(h.Sum(nil))
					}
					f.Close()
				}
			}
		} else {
			data["exists"] = false
		}
		if mt := str(raw, "mime", "media_type"); mt != "" {
			data["media_type"] = mt
		}
		if summary == "" {
			summary = "文件 " + filepath.Base(p)
		}
	case ResultCommit:
		repo, branch, hash := str(raw, "repo", "repository"), str(raw, "branch"), str(raw, "commit", "hash", "sha")
		if repo == "" || branch == "" || hash == "" {
			return "", "", "", nil, fmt.Errorf("%w: a commit result needs repo, branch and commit", ErrInvalid)
		}
		if !commitRE.MatchString(hash) {
			return "", "", "", nil, fmt.Errorf("%w: commit %q is not a commit hash", ErrInvalid, hash)
		}
		data["repo"], data["branch"], data["commit"] = repo, branch, strings.ToLower(hash)
		if m := str(raw, "message"); m != "" {
			data["message"] = clip(m, 4000)
		}
		if u := str(raw, "url"); u != "" {
			data["url"] = u
		}
		if files := strList(raw["files"]); len(files) > 0 {
			data["files"] = files
		}
		if summary == "" {
			short := hash
			if len(short) > 10 {
				short = short[:10]
			}
			summary = fmt.Sprintf("%s@%s %s", filepath.Base(repo), branch, short)
			if m := str(raw, "message"); m != "" {
				summary += " " + firstLine(m, 80)
			}
		}
	case ResultCommandOutput:
		argv := strList(raw["argv"])
		cmdline := str(raw, "command", "cmd")
		if len(argv) == 0 && cmdline == "" {
			return "", "", "", nil, fmt.Errorf("%w: a command_output result needs argv or command", ErrInvalid)
		}
		code, ok := raw["exit_code"].(float64)
		if !ok {
			return "", "", "", nil, fmt.Errorf("%w: a command_output result needs exit_code", ErrInvalid)
		}
		if len(argv) > 0 {
			data["argv"] = argv
			cmdline = strings.Join(argv, " ")
		}
		data["command"], data["exit_code"] = cmdline, int(code)
		data["stdout"], data["stderr"] = clip(str(raw, "stdout"), 64<<10), clip(str(raw, "stderr"), 64<<10)
		if summary == "" {
			summary = fmt.Sprintf("%s → 退出码 %d", firstLine(cmdline, 80), int(code))
		}
	case ResultData:
		v, ok := raw["data"]
		if !ok || v == nil {
			return "", "", "", nil, fmt.Errorf("%w: a data result needs data", ErrInvalid)
		}
		data["data"] = v
		if s := str(raw, "schema"); s != "" {
			data["schema"] = s
		}
		if summary == "" {
			b, _ := json.Marshal(v)
			summary = "结构化数据 " + firstLine(string(b), 80)
		}
	case ResultTaskStatus:
		st := str(raw, "status", "state")
		if st == "" {
			return "", "", "", nil, fmt.Errorf("%w: a task_status result needs status", ErrInvalid)
		}
		data["status"] = st
		for _, k := range []string{"url", "system", "external_id"} {
			if v := str(raw, k); v != "" {
				data[k] = v
			}
		}
		if ls := str(raw, "local_status"); ls != "" {
			s, err := core.ParseStatus(ls)
			if err != nil {
				return "", "", "", nil, err
			}
			data["local_status"] = string(s)
		}
		if summary == "" {
			summary = "外部状态 " + st
			if sys := str(raw, "system"); sys != "" {
				summary = sys + " 状态 " + st
			}
		}
	}
	return typ, key, firstLine(summary, 200), data, nil
}

func hashResult(typ string, data map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ResultInput is a result written back for a task.
type ResultInput struct {
	TaskID  string
	RunID   string // "" = ManualRun
	Attempt int
	// Agent is the source shown with the result (FR-511).
	Agent string
	// Raw is the result message: {"result_type": "file", "path": …}.
	Raw map[string]any
	// Dir resolves relative file paths.
	Dir string
}

// SaveResult validates and stores a result, idempotently (see Result).
func (m *Manager) SaveResult(in ResultInput) (*Result, error) {
	if in.RunID == "" {
		in.RunID = ManualRun
	}
	t, err := m.store.Get(in.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Deleted() {
		return nil, fmt.Errorf("%w: task %s is deleted", core.ErrDeleted, t.ID)
	}
	typ, key, summary, data, err := NormalizeResult(in.Raw, in.Dir)
	if err != nil {
		return nil, err
	}
	data = m.redactMap(data, nil)
	summary = m.redact(summary, nil)
	r := &Result{TaskID: t.ID, RunID: in.RunID, Attempt: in.Attempt, Type: typ, Key: ResultKey(t.ID, in.RunID, typ, key),
		Agent: in.Agent, Summary: summary, Data: data, Hash: hashResult(typ, data), CreatedAt: m.store.Now()}
	tx, err := m.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, version, hash, created_at FROM agent_results WHERE idem_key = ? ORDER BY version`, r.Key)
	if err != nil {
		return nil, err
	}
	var dupID int64
	for rows.Next() {
		var id int64
		var v int
		var h, created string
		if err := rows.Scan(&id, &v, &h, &created); err != nil {
			rows.Close()
			return nil, err
		}
		r.Version = v
		if h == r.Hash {
			dupID = id
		}
	}
	rows.Close()
	if dupID != 0 {
		tx.Rollback()
		got, err := m.result(dupID)
		if err != nil {
			return nil, err
		}
		got.Duplicate = true
		return got, nil
	}
	r.Version++
	b, _ := json.Marshal(r.Data)
	res, err := tx.Exec(`INSERT INTO agent_results(task_id, run_id, attempt, type, idem_key, version, agent, summary, data, hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.TaskID, r.RunID, r.Attempt, r.Type, r.Key, r.Version, r.Agent, r.Summary,
		string(b), r.Hash, fmtTime(r.CreatedAt))
	if err != nil {
		return nil, err
	}
	r.ID, _ = res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	changes := map[string]core.Change{"result": {To: r.Summary}, "result_type": {To: r.Type}, "run": {To: r.RunID}}
	if r.Version > 1 {
		changes["result_version"] = core.Change{From: r.Version - 1, To: r.Version}
	}
	_ = m.store.WithActor(agentActor(r.Agent)).RecordEvent(r.TaskID, "agent_result", changes)
	return r, nil
}

const resultColumns = `id, task_id, run_id, attempt, type, idem_key, version, agent, summary, data, hash, created_at`

func scanResult(sc interface{ Scan(...any) error }) (*Result, error) {
	var r Result
	var data, created string
	if err := sc.Scan(&r.ID, &r.TaskID, &r.RunID, &r.Attempt, &r.Type, &r.Key, &r.Version, &r.Agent, &r.Summary, &data, &r.Hash, &created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(data), &r.Data)
	r.CreatedAt = parseTime(created)
	return &r, nil
}

func (m *Manager) result(id int64) (*Result, error) {
	r, err := scanResult(m.db().QueryRow(`SELECT `+resultColumns+` FROM agent_results WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: result %d", ErrNotFound, id)
	}
	return r, err
}

func (m *Manager) results(where string, args ...any) ([]Result, error) {
	rows, err := m.db().Query(`SELECT `+resultColumns+` FROM agent_results `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Result{}
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// TaskResults lists every result (all versions) written back for a task,
// oldest first.
func (m *Manager) TaskResults(taskID string) ([]Result, error) {
	return m.results(`WHERE task_id = ?`, taskID)
}

// RunResults lists the results of a run.
func (m *Manager) RunResults(runID string) ([]Result, error) {
	return m.results(`WHERE run_id = ?`, runID)
}
