package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Tuesday 2026-10-06 10:00 in UTC+8.
var cst = time.FixedZone("CST", 8*3600)
var now = time.Date(2026, 10, 6, 10, 0, 0, 0, cst)

type harness struct {
	t     *testing.T
	dir   string
	env   map[string]string
	stdin string
	tick  int
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, dir: t.TempDir(), env: map[string]string{}}
}

func (h *harness) run(args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	env := Env{
		Stdin: strings.NewReader(h.stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return h.env[k] },
		Now: func() time.Time {
			h.tick++
			return now.Add(time.Duration(h.tick) * time.Millisecond)
		},
	}
	code = Run(append([]string{"--data-dir", h.dir}, args...), env)
	return out.String(), errb.String(), code
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, errOut, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("todo %v exited %d: %s", args, code, errOut)
	}
	return out
}

func (h *harness) json(v any, args ...string) {
	h.t.Helper()
	out := h.ok(append([]string{"--json"}, args...)...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		h.t.Fatalf("todo %v: invalid JSON %v:\n%s", args, err, out)
	}
}

type taskOut struct {
	Task     core.Task           `json:"task"`
	Children []core.Task         `json:"children"`
	History  []core.HistoryEntry `json:"history"`
}

type listOut struct {
	Count int         `json:"count"`
	Tasks []core.Task `json:"tasks"`
}

func (h *harness) add(args ...string) core.Task {
	h.t.Helper()
	var o taskOut
	h.json(&o, append([]string{"add"}, args...)...)
	return o.Task
}

func (h *harness) list(args ...string) []string {
	h.t.Helper()
	var o listOut
	h.json(&o, append([]string{"list"}, args...)...)
	if o.Count != len(o.Tasks) {
		h.t.Fatalf("count %d != %d", o.Count, len(o.Tasks))
	}
	titles := make([]string, len(o.Tasks))
	for i, t := range o.Tasks {
		titles[i] = t.Title
	}
	return titles
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAddShowJSON(t *testing.T) {
	h := newHarness(t)
	parent := h.add("Release", "prep")
	if parent.Title != "Release prep" {
		t.Fatalf("multi-word title: %q", parent.Title)
	}
	task := h.add("写周报", "-p", "high", "--due", "tomorrow", "-t", "work,weekly", "--tag", "team",
		"-c", "office", "-d", "summary", "-n", "remember metrics", "--parent", parent.ID[:6])
	if task.Priority != core.PriorityHigh || task.Category != "office" || task.Description != "summary" ||
		task.Notes != "remember metrics" || task.ParentID != parent.ID || task.Status != core.StatusTodo || task.Version != 1 {
		t.Fatalf("task: %+v", task)
	}
	eq(t, task.Tags, []string{"team", "weekly", "work"})
	wantDue := time.Date(2026, 10, 7, 23, 59, 59, 0, cst)
	if task.DueAt == nil || !task.DueAt.Equal(wantDue) {
		t.Fatalf("due = %v, want %v", task.DueAt, wantDue)
	}

	// Raw JSON uses snake_case keys and string priorities for scripts.
	out := h.ok("--json", "show", task.ID[:8])
	for _, key := range []string{`"id":`, `"due_at": "2026-10-07T15:59:59Z"`, `"priority": "high"`, `"parent_id":`, `"version": 1`, `"history":`} {
		if !strings.Contains(out, key) {
			t.Errorf("show JSON missing %s:\n%s", key, out)
		}
	}
	var shown taskOut
	h.json(&shown, "show", parent.ID)
	if len(shown.Children) != 1 || shown.Children[0].ID != task.ID || shown.History[0].Action != "create" {
		t.Fatalf("show parent: %+v", shown)
	}

	if _, stderr, code := h.run("add"); code != ExitUsage || !strings.Contains(stderr, "title is required") {
		t.Fatalf("missing title: %d %s", code, stderr)
	}
	if _, _, code := h.run("add", "x", "--due", "someday"); code != ExitUsage {
		t.Fatalf("bad due accepted: %d", code)
	}
}

func TestEditAndConflict(t *testing.T) {
	h := newHarness(t)
	task := h.add("draft", "-t", "a,b", "--due", "today")
	var o taskOut
	h.json(&o, "edit", task.ID[:6], "--title", "final", "-p", "urgent", "--tag", "c", "--untag", "a",
		"--no-due", "--status", "doing", "--append-notes", "line1", "--if-version", "1")
	got := o.Task
	if got.Title != "final" || got.Priority != core.PriorityUrgent || got.DueAt != nil || got.Status != core.StatusInProgress ||
		got.Notes != "line1" || got.Version != 2 {
		t.Fatalf("edit: %+v", got)
	}
	eq(t, got.Tags, []string{"b", "c"})
	h.json(&o, "edit", task.ID, "--append-notes", "line2", "--tags", "x", "--category", "home")
	if o.Task.Notes != "line1\nline2" || o.Task.Category != "home" {
		t.Fatalf("append notes: %+v", o.Task)
	}
	eq(t, o.Task.Tags, []string{"x"})

	_, stderr, code := h.run("--json", "edit", task.ID, "--title", "stale", "--if-version", "1")
	if code != ExitConflict || !strings.Contains(stderr, `"code":"version_conflict"`) {
		t.Fatalf("conflict: %d %s", code, stderr)
	}
}

func TestLifecycleCommands(t *testing.T) {
	h := newHarness(t)
	a := h.add("a")
	b := h.add("b")
	var r core.Result
	h.json(&r, "done", a.ID[:5], b.ID[:5])
	if r.Changed != 2 || r.Tasks[0].Status != core.StatusDone || r.Tasks[0].CompletedAt == nil {
		t.Fatalf("done: %+v", r)
	}
	eq(t, h.list("--status", "done"), []string{"a", "b"})
	h.json(&r, "reopen", a.ID)
	if r.Tasks[0].Status != core.StatusTodo || r.Tasks[0].CompletedAt != nil {
		t.Fatalf("reopen: %+v", r.Tasks[0])
	}
	h.json(&r, "start", a.ID)
	if r.Tasks[0].Status != core.StatusInProgress {
		t.Fatalf("start: %+v", r.Tasks[0])
	}
	h.ok("archive", b.ID)
	eq(t, h.list(), []string{"a"})
	eq(t, h.list("--all"), []string{"a", "b"})
	h.ok("delete", a.ID)
	eq(t, h.list("--all"), []string{"b"})
	eq(t, h.list("--deleted"), []string{"a"})
	h.ok("restore", a.ID)
	eq(t, h.list(), []string{"a"})

	var hist struct {
		TaskID  string              `json:"task_id"`
		History []core.HistoryEntry `json:"history"`
	}
	h.json(&hist, "history", a.ID)
	var actions []string
	for _, e := range hist.History {
		actions = append(actions, e.Action)
	}
	eq(t, actions, []string{"create", "complete", "reopen", "start", "delete", "restore"})

	_, stderr, code := h.run("--json", "done", "ffffffffff")
	if code != ExitNotFound || !strings.Contains(stderr, `"code":"not_found"`) {
		t.Fatalf("not found: %d %s", code, stderr)
	}
}

func TestFilterAndSortFlags(t *testing.T) {
	h := newHarness(t)
	report := h.add("write report", "-p", "high", "--due", "+2d", "-t", "work", "-c", "office")
	h.add("buy milk", "-p", "low", "-t", "home", "-n", "lactose free")
	h.add("file taxes", "-p", "urgent", "--due", "2026-10-01", "-t", "home,money")
	h.add("slides", "-p", "medium", "--due", "2026-10-20 09:00", "-t", "work", "-c", "office", "--parent", report.ID)
	h.add("old thing", "--status", "done")

	eq(t, h.list("--status", "todo,done", "--priority", "high,urgent"), []string{"write report", "file taxes"})
	eq(t, h.list("--min-priority", "medium"), []string{"write report", "file taxes", "slides"})
	eq(t, h.list("-t", "home", "--tag", "money"), []string{"file taxes"})
	eq(t, h.list("-c", "office"), []string{"write report", "slides"})
	eq(t, h.list("--parent", report.ID[:6]), []string{"slides"})
	eq(t, h.list("--parent", "none", "--no-due"), []string{"buy milk", "old thing"})
	eq(t, h.list("--due-before", "2026-10-08"), []string{"write report", "file taxes"})
	eq(t, h.list("--due-after", "tomorrow"), []string{"write report", "slides"})
	eq(t, h.list("--overdue"), []string{"file taxes"})
	eq(t, h.list("-q", "lactose"), []string{"buy milk"})
	eq(t, h.list("--sort", "due"), []string{"file taxes", "write report", "slides", "buy milk", "old thing"})
	eq(t, h.list("--sort", "priority", "--limit", "2"), []string{"file taxes", "write report"})
	eq(t, h.list("--sort", "created", "--reverse"), []string{"write report", "buy milk", "file taxes", "slides", "old thing"})
	eq(t, h.list("--sort", "updated", "--limit", "1"), []string{"old thing"})

	var o listOut
	h.json(&o, "search", "taxes", "--tag", "money")
	if o.Count != 1 || o.Tasks[0].Title != "file taxes" {
		t.Fatalf("search: %+v", o)
	}
	if _, _, code := h.run("search"); code != ExitUsage {
		t.Fatal("search without keywords should be a usage error")
	}
	if _, _, code := h.run("list", "--sort", "random"); code != ExitError {
		t.Fatalf("bad sort exit %d", code)
	}

	// Manual order via move.
	var all listOut
	h.json(&all, "list")
	last := all.Tasks[len(all.Tasks)-1]
	h.ok("move", last.ID, "--top")
	h.ok("move", report.ID, "--after", all.Tasks[2].ID)
	eq(t, h.list("--sort", "manual"), []string{"old thing", "buy milk", "file taxes", "write report", "slides"})
	if _, _, code := h.run("move", report.ID, "--top", "--category", "x"); code != ExitUsage {
		t.Fatalf("mixed move flags: %d", code)
	}
}

func TestBatchCommands(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.add("a"), h.add("b"), h.add("c")
	p := h.add("parent")
	var r core.Result
	h.json(&r, "batch", "priority", "high", a.ID, b.ID, c.ID)
	if r.Changed != 3 || r.OperationID == 0 {
		t.Fatalf("batch priority: %+v", r)
	}
	h.json(&r, "batch", "move", a.ID, b.ID, "--category", "work", "--parent", p.ID[:6])
	if r.Changed != 2 || r.Tasks[0].Category != "work" || r.Tasks[1].ParentID != p.ID {
		t.Fatalf("batch move: %+v", r)
	}
	h.json(&r, "batch", "done", a.ID, b.ID)
	if r.Changed != 2 {
		t.Fatalf("batch done: %+v", r)
	}
	h.json(&r, "batch", "archive", a.ID, b.ID, c.ID)
	if r.Changed != 3 {
		t.Fatalf("batch archive: %+v", r)
	}
	eq(t, h.list(), []string{"parent"})

	// One bad id aborts the whole batch.
	if _, _, code := h.run("batch", "reopen", a.ID, "deadbeef00"); code != ExitNotFound {
		t.Fatalf("bad batch exit %d", code)
	}
	eq(t, h.list("--status", "archived"), []string{"a", "b", "c"})

	// Undo reverts the whole batch in one step.
	var u core.UndoResult
	h.json(&u, "undo")
	if u.Operation.Kind != "archive" || len(u.Restored) != 3 {
		t.Fatalf("undo: %+v", u)
	}
	eq(t, h.list("--status", "done"), []string{"a", "b"})
	if _, _, code := h.run("batch", "explode", a.ID); code != ExitUsage {
		t.Fatal("unknown batch action accepted")
	}
}

func TestUndoCommand(t *testing.T) {
	h := newHarness(t)
	_, stderr, code := h.run("--json", "undo")
	if code != ExitError || !strings.Contains(stderr, "nothing_to_undo") {
		t.Fatalf("empty undo: %d %s", code, stderr)
	}
	a := h.add("keep")
	h.ok("delete", a.ID)
	out := h.ok("undo")
	if !strings.Contains(out, "restored") || !strings.Contains(out, a.ID[:8]) {
		t.Fatalf("undo output: %s", out)
	}
	eq(t, h.list(), []string{"keep"})
}

func TestExportImportCommands(t *testing.T) {
	src := newHarness(t)
	src.add("one", "-t", "x")
	two := src.add("two", "-p", "high")
	src.add("child", "--parent", two.ID)
	file := filepath.Join(t.TempDir(), "export.json")
	src.ok("export", "-o", file)
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("export file: %v %v", fi, err)
	}
	stdoutDoc := src.ok("export")
	var doc core.ExportDocument
	if err := json.Unmarshal([]byte(stdoutDoc), &doc); err != nil || doc.Format != core.ExportFormat || len(doc.Tasks) != 3 {
		t.Fatalf("export stdout: %v %+v", err, doc)
	}

	dst := newHarness(t)
	var r core.ImportResult
	dst.json(&r, "import", file)
	if r.Inserted != 3 {
		t.Fatalf("import: %+v", r)
	}
	eq(t, dst.list(), []string{"one", "two", "child"})

	other := newHarness(t)
	other.stdin = stdoutDoc
	other.add("local")
	other.json(&r, "import", "-", "--replace")
	if r.Inserted != 3 || r.Deleted != 1 {
		t.Fatalf("import --replace: %+v", r)
	}
	eq(t, other.list(), []string{"one", "two", "child"})

	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"format":"nope"}`), 0o600)
	_, stderr, code := dst.run("--json", "import", bad)
	if code != ExitError || !strings.Contains(stderr, "invalid_import_file") {
		t.Fatalf("bad import: %d %s", code, stderr)
	}
}

func TestStatusCommand(t *testing.T) {
	h := newHarness(t)
	h.add("a", "--due", "2026-10-01")
	h.add("b", "--status", "done")
	h.env[EnvModelProvider] = "openai"
	h.env[EnvModelName] = "gpt-test"
	h.env[EnvModelBaseURL] = "https://user:secret@api.example.com/v1?key=leak"
	h.env[EnvModelAPIKey] = "sk-supersecret"

	out := h.ok("--json", "status")
	for _, secret := range []string{"sk-supersecret", "secret@", "leak"} {
		if strings.Contains(out, secret) {
			t.Fatalf("status leaks %q:\n%s", secret, out)
		}
	}
	var r StatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.State != "ok" || r.Version != Version || r.DataDir != h.dir || r.Database.Path != filepath.Join(h.dir, core.DBFileName) {
		t.Fatalf("status: %+v", r)
	}
	if r.Database.SchemaVersion != core.LatestSchemaVersion() || r.Database.SizeBytes == 0 {
		t.Fatalf("database: %+v", r.Database)
	}
	if r.Tasks == nil || r.Tasks.Total != 2 || r.Tasks.ByStatus[core.StatusDone] != 1 || r.Tasks.Overdue != 1 {
		t.Fatalf("tasks: %+v", r.Tasks)
	}
	m := r.Model
	if m.Status != "configured" || m.Provider != "openai" || m.Model != "gpt-test" || !m.APIKeySet ||
		m.BaseURL != "https://api.example.com/v1" || m.Source != "env" {
		t.Fatalf("model: %+v", m)
	}
	if r.Runtime.OS == "" || r.Runtime.PID == 0 {
		t.Fatalf("runtime: %+v", r.Runtime)
	}

	human := h.ok("status")
	for _, want := range []string{"data dir:    " + h.dir, "schema v2/v2", "model:       configured (openai/gpt-test", "api key set"} {
		if !strings.Contains(human, want) {
			t.Errorf("human status missing %q:\n%s", want, human)
		}
	}

	// Without model config, task management still works and status says so.
	bare := newHarness(t)
	out = bare.ok("status")
	if !strings.Contains(out, "not_configured") || !strings.Contains(out, "api key missing") {
		t.Fatalf("unconfigured status: %s", out)
	}
}

func TestStatusReportsUnusableDataDir(t *testing.T) {
	h := newHarness(t)
	h.dir = filepath.Join(t.TempDir(), "file")
	os.WriteFile(h.dir, []byte("x"), 0o600)
	out, _, code := h.run("--json", "status")
	var r StatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("status must still print JSON: %v\n%s", err, out)
	}
	if code != ExitError || r.State != "error" || r.Error == "" || r.DataDir != h.dir {
		t.Fatalf("code %d report %+v", code, r)
	}
}

func TestDataDirFromEnv(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	env := Env{Stdout: &out, Stderr: &out, Getenv: func(k string) string {
		if k == core.EnvHome {
			return dir
		}
		return ""
	}}
	if code := Run([]string{"add", "x"}, env); code != 0 {
		t.Fatalf("add: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, core.DBFileName)); err != nil {
		t.Fatal("TODO_CLI_HOME not honored")
	}
}

func TestHumanOutputAndHelp(t *testing.T) {
	h := newHarness(t)
	task := h.add("写周报", "-p", "high", "--due", "2026-10-01", "-t", "work", "-c", "office")
	out := h.ok("list")
	for _, want := range []string{"[ ] " + task.ID[:8], "!high 写周报", "due 2026-10-01 23:59 (overdue)", "#work", "@office", "1 task(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	out = h.ok("delete", task.ID)
	if !strings.Contains(out, "undo with `todo undo`") {
		t.Errorf("delete should hint at undo: %s", out)
	}
	if out := h.ok("list"); !strings.Contains(out, "No tasks.") {
		t.Errorf("empty list: %s", out)
	}
	help := h.ok("help")
	for _, cmd := range []string{"add", "list", "search", "edit", "done", "reopen", "archive", "delete", "batch", "undo", "export", "import", "status"} {
		if !strings.Contains(help, "\n  "+cmd) {
			t.Errorf("help missing %s", cmd)
		}
	}
	if out := h.ok("add", "-h"); !strings.Contains(out, "usage: todo add") || !strings.Contains(out, "-priority") {
		t.Errorf("add -h: %s", out)
	}
	if out := h.ok("help", "list"); !strings.Contains(out, "-sort") {
		t.Errorf("help list: %s", out)
	}
	if _, stderr, code := h.run("frobnicate"); code != ExitUsage || !strings.Contains(stderr, "unknown command") {
		t.Errorf("unknown command: %d %s", code, stderr)
	}
	if _, stderr, code := h.run("list", "--bogus"); code != ExitUsage || !strings.Contains(stderr, "bogus") {
		t.Errorf("unknown flag: %d %s", code, stderr)
	}
	var v map[string]string
	h.json(&v, "version")
	if v["version"] != Version {
		t.Errorf("version: %v", v)
	}
}

func TestParseWhen(t *testing.T) {
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, cst) }
	cases := map[string]struct {
		want     time.Time
		dateOnly bool
	}{
		"today":                {day(10, 6), true},
		"今天":                   {day(10, 6), true},
		"tomorrow":             {day(10, 7), true},
		"明天":                   {day(10, 7), true},
		"后天":                   {day(10, 8), true},
		"+3d":                  {day(10, 9), true},
		"+1w":                  {day(10, 13), true},
		"+2h":                  {time.Date(2026, 10, 6, 12, 0, 0, 0, cst), false},
		"fri":                  {day(10, 9), true},
		"tue":                  {day(10, 13), true}, // today is Tuesday: next one
		"周五":                   {day(10, 9), true},
		"下周一":                  {day(10, 12), true},
		"next sunday":          {day(10, 18), true},
		"2026-11-01":           {day(11, 1), true},
		"2026-11-01 18:30":     {time.Date(2026, 11, 1, 18, 30, 0, 0, cst), false},
		"2026-11-01T18:30":     {time.Date(2026, 11, 1, 18, 30, 0, 0, cst), false},
		"2026-11-01T18:30:00Z": {time.Date(2026, 11, 1, 18, 30, 0, 0, time.UTC), false},
	}
	for in, c := range cases {
		got, dateOnly, err := parseWhen(in, now)
		if err != nil || !got.Equal(c.want) || dateOnly != c.dateOnly {
			t.Errorf("parseWhen(%q) = %v %v %v, want %v %v", in, got, dateOnly, err, c.want, c.dateOnly)
		}
	}
	for _, bad := range []string{"", "someday", "+xd", "2026-13-01"} {
		if _, _, err := parseWhen(bad, now); err == nil {
			t.Errorf("parseWhen(%q) should fail", bad)
		}
	}
}
