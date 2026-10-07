package server_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mxiao/todo-cli/packages/core"
)

func TestCreateAndGet(t *testing.T) {
	e := start(t, t.TempDir())
	parent := e.create(map[string]any{"title": "发布准备"})
	r := e.must(http.StatusCreated, "POST", "/api/tasks", map[string]any{
		"title": "整理需求", "description": "d", "notes": "n", "priority": "high", "tags": []string{"#work", "web"},
		"category": "office", "parent_id": parent.ID[:8], "due_at": "2026-10-09T18:00:00+08:00", "status": "in_progress",
	})
	var created taskResp
	r.json(t, &created)
	task := created.Task
	if r.header.Get("Location") != "/api/tasks/"+task.ID || r.header.Get("ETag") != `"1"` {
		t.Errorf("headers: %v", r.header)
	}
	if task.Title != "整理需求" || task.Priority != core.PriorityHigh || !slices.Equal(task.Tags, []string{"web", "work"}) ||
		task.ParentID != parent.ID || task.Status != core.StatusInProgress || task.Version != 1 ||
		task.DueAt == nil || task.DueAt.UTC().Format("2006-01-02T15:04") != "2026-10-09T10:00" {
		t.Fatalf("created: %+v", task)
	}
	if created.Revision == 0 {
		t.Error("write responses carry the store revision")
	}

	var detail struct {
		Task      core.Task           `json:"task"`
		Subtasks  []core.Task         `json:"subtasks"`
		History   []core.HistoryEntry `json:"history"`
		Conflicts []core.Conflict     `json:"conflicts"`
	}
	e.must(http.StatusOK, "GET", "/api/tasks/"+parent.ID, nil).json(t, &detail)
	if len(detail.Subtasks) != 1 || detail.Subtasks[0].ID != task.ID || len(detail.History) != 1 ||
		detail.History[0].Action != "create" || detail.History[0].Actor != "web" || detail.Conflicts == nil {
		t.Fatalf("detail: %+v", detail)
	}
	// Short id prefixes work like in the CLI.
	if got := e.get(task.ID[:8]); got.ID != task.ID {
		t.Fatalf("prefix lookup: %+v", got)
	}
}

func TestCreateErrors(t *testing.T) {
	e := start(t, t.TempDir())
	cases := []struct {
		name   string
		body   any
		hdr    []string
		status int
		code   string
	}{
		{"missing title", map[string]any{"title": "  "}, nil, 400, "invalid_input"},
		{"unknown field", map[string]any{"title": "x", "colour": "red"}, nil, 400, "invalid_input"},
		{"bad priority", map[string]any{"title": "x", "priority": "huge"}, nil, 400, "invalid_input"},
		{"bad status", map[string]any{"title": "x", "status": "sleeping"}, nil, 400, "invalid_input"},
		{"bad due", map[string]any{"title": "x", "due_at": "someday"}, nil, 400, "invalid_input"},
		{"missing parent", map[string]any{"title": "x", "parent_id": "ffffffff"}, nil, 400, "invalid_input"},
		{"malformed json", `{"title": `, nil, 400, "invalid_json"},
		{"form post", `title=x`, []string{"Content-Type", "application/x-www-form-urlencoded"}, 415, "unsupported_media_type"},
		{"too large", `{"title":"` + strings.Repeat("x", 2<<20) + `"}`, nil, 413, "body_too_large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.do("POST", "/api/tasks", c.body, c.hdr...)
			if r.status != c.status || r.errorCode(t) != c.code {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}
	if n := len(e.list("?status=all&deleted=include")); n != 0 {
		t.Fatalf("failed creates stored %d tasks", n)
	}
}

func TestGetErrors(t *testing.T) {
	e := start(t, t.TempDir())
	a := e.create(map[string]any{"title": "a"})
	if r := e.do("GET", "/api/tasks/ffffffff", nil); r.status != 404 || r.errorCode(t) != "not_found" {
		t.Fatalf("missing task: %d %s", r.status, r.body)
	}
	if r := e.do("GET", "/api/nope", nil); r.status != 404 || r.errorCode(t) != "not_found" {
		t.Fatalf("unknown endpoint: %d %s", r.status, r.body)
	}
	if r := e.do("GET", "/api/tasks/"+a.ID+"/versions/x", nil); r.status != 400 {
		t.Fatalf("bad version: %d", r.status)
	}
	if r := e.do("GET", "/api/tasks/"+a.ID+"/versions/9", nil); r.status != 404 {
		t.Fatalf("missing version: %d", r.status)
	}
	if r := e.do("PUT", "/api/tasks/"+a.ID, nil); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", r.status)
	}
}

func TestListSearchFilterSort(t *testing.T) {
	e := start(t, t.TempDir())
	e.create(map[string]any{"title": "写周报", "priority": "high", "tags": []string{"work"}, "due_at": "2026-10-08", "category": "office"})
	e.create(map[string]any{"title": "买菜", "priority": "low", "tags": []string{"home"}, "due_at": "2026-10-07"})
	e.create(map[string]any{"title": "读书", "priority": "urgent", "notes": "周报之后"})
	done := e.create(map[string]any{"title": "旧任务", "status": "done"})
	archived := e.create(map[string]any{"title": "已归档", "status": "archived"})

	check := func(query string, want ...string) {
		t.Helper()
		if got := titles(e.list(query)); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
	check("", "写周报", "买菜", "读书", "旧任务")
	check("?q=周报", "写周报", "读书")
	check("?status=todo&sort=priority", "读书", "写周报", "买菜")
	check("?priority=high,urgent", "写周报", "读书")
	check("?min_priority=high&sort=priority&reverse=true", "写周报", "读书")
	check("?tag=work", "写周报")
	check("?category=office", "写周报")
	check("?sort=due", "买菜", "写周报", "读书", "旧任务")
	check("?due_before=2026-10-07", "买菜")
	check("?due_after=2026-10-08", "写周报")
	check("?has_due=false&status=todo", "读书")
	check("?status=done", done.Title)
	check("?status=archived", archived.Title)
	check("?status=all&limit=2", "写周报", "买菜")
	check("?include_archived=1&q=已归档", "已归档")
	check("?parent=none&q=读书", "读书")

	var out listResp
	e.must(200, "GET", "/api/tasks", nil).json(t, &out)
	if out.Count != 4 || out.Revision == 0 {
		t.Errorf("list envelope: count=%d revision=%d", out.Count, out.Revision)
	}
	for _, q := range []string{"?sort=random", "?status=nope", "?priority=9", "?limit=-1", "?overdue=maybe",
		"?deleted=all", "?due_before=never", "?colour=red", "?parent=ffffffff"} {
		r := e.do("GET", "/api/tasks"+q, nil)
		if r.status != 400 && r.status != 404 {
			t.Errorf("%s: status %d", q, r.status)
		}
	}
}

func TestPatch(t *testing.T) {
	e := start(t, t.TempDir())
	task := e.create(map[string]any{"title": "a", "due_at": "2026-10-09", "tags": []string{"x", "y"}})

	if r := e.do("PATCH", "/api/tasks/"+task.ID, map[string]any{"title": "b"}); r.status != 428 || r.errorCode(t) != "version_required" {
		t.Fatalf("missing version: %d %s", r.status, r.body)
	}
	var out taskResp
	e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{
		"version": 1, "title": "b", "due_at": nil, "priority": 3, "add_tags": []string{"z"}, "remove_tags": []string{"x"},
		"notes": "补充", "status": "in_progress",
	}).json(t, &out)
	got := out.Task
	if got.Title != "b" || got.DueAt != nil || got.Priority != core.PriorityHigh || !slices.Equal(got.Tags, []string{"y", "z"}) ||
		got.Notes != "补充" || got.Status != core.StatusInProgress || got.Version != 2 {
		t.Fatalf("patched: %+v", got)
	}
	// If-Match works as well, and agrees with the body when both are sent.
	r := e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{"title": "c"}, "If-Match", `"2"`)
	if r.header.Get("ETag") != `"3"` {
		t.Errorf("etag after patch: %q", r.header.Get("ETag"))
	}
	if r := e.do("PATCH", "/api/tasks/"+task.ID, map[string]any{"title": "d", "version": 2}, "If-Match", "3"); r.status != 400 {
		t.Errorf("disagreeing versions: %d", r.status)
	}
	for _, body := range []any{
		map[string]any{"version": 3, "title": ""},
		map[string]any{"version": 3, "priority": "huge"},
		map[string]any{"version": 3, "status": "nope"},
		map[string]any{"version": 3, "nickname": "x"},
		map[string]any{"version": 0, "title": "x"},
		map[string]any{"version": 3, "parent_id": task.ID},
		`[1,2]`,
	} {
		if r := e.do("PATCH", "/api/tasks/"+task.ID, body); r.status != 400 {
			t.Errorf("%v: status %d %s", body, r.status, r.body)
		}
	}
	if r := e.do("PATCH", "/api/tasks/ffffffff", map[string]any{"version": 1, "title": "x"}); r.status != 404 {
		t.Errorf("missing task: %d", r.status)
	}
	e.must(200, "DELETE", "/api/tasks/"+task.ID, nil)
	if r := e.do("PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 4, "title": "x"}); r.status != 410 || r.errorCode(t) != "task_deleted" {
		t.Errorf("patching a deleted task: %d %s", r.status, r.body)
	}
}

func TestStatusDeleteRestoreUndo(t *testing.T) {
	e := start(t, t.TempDir())
	a := e.create(map[string]any{"title": "a"})
	b := e.create(map[string]any{"title": "b"})

	var st struct {
		Task    core.Task `json:"task"`
		Changed int       `json:"changed"`
	}
	e.must(200, "POST", "/api/tasks/"+a.ID+"/status", map[string]any{"status": "done", "version": 1}).json(t, &st)
	if st.Task.Status != core.StatusDone || st.Task.CompletedAt == nil || st.Changed != 1 {
		t.Fatalf("complete: %+v", st)
	}
	hist, _ := e.store.History(a.ID)
	if last := hist[len(hist)-1]; last.Action != "complete" || last.Actor != "web" {
		t.Fatalf("web status changes are recorded like the CLI's: %+v", last)
	}
	e.must(200, "POST", "/api/tasks/"+a.ID+"/status", map[string]any{"status": "todo"})
	e.must(200, "POST", "/api/tasks/"+a.ID+"/status", map[string]any{"status": "archived"})
	if got := e.get(a.ID); got.Status != core.StatusArchived || got.ArchivedAt == nil {
		t.Fatalf("archive: %+v", got)
	}
	if r := e.do("POST", "/api/tasks/"+a.ID+"/status", map[string]any{"status": "later"}); r.status != 400 {
		t.Errorf("bad status: %d", r.status)
	}
	// A stale status change is a conflict, kept as a recoverable edit.
	r := e.do("POST", "/api/tasks/"+b.ID+"/status", map[string]any{"status": "done", "version": 7})
	if r.status != 409 || r.errorCode(t) != "version_conflict" {
		t.Fatalf("stale status: %d %s", r.status, r.body)
	}
	if cs, _ := e.store.Conflicts(b.ID, false); len(cs) != 1 || !strings.Contains(string(cs[0].Patch), `"done"`) {
		t.Fatalf("stored conflict: %+v", cs)
	}

	if r := e.do("DELETE", "/api/tasks/"+b.ID+"?version=5", nil); r.status != 409 {
		t.Fatalf("stale delete: %d %s", r.status, r.body)
	}
	e.must(200, "DELETE", "/api/tasks/"+b.ID+"?version=1", nil)
	if got := titles(e.list("")); len(got) != 0 {
		t.Fatalf("deleted task still listed: %q", got)
	}
	if got := titles(e.list("?deleted=only")); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("deleted=only: %q", got)
	}
	if r := e.do("DELETE", "/api/tasks/"+b.ID, nil); r.status != 410 {
		t.Errorf("double delete: %d", r.status)
	}
	e.must(200, "POST", "/api/tasks/"+b.ID+"/restore", nil)
	if got := e.get(b.ID); got.DeletedAt != nil {
		t.Fatal("restore failed")
	}

	// Undo steps back through the same operation log the CLI uses.
	var undo struct {
		Undo core.UndoResult `json:"undo"`
	}
	e.must(200, "POST", "/api/undo", nil).json(t, &undo)
	if undo.Undo.Operation.Kind != "restore" || e.get(b.ID).DeletedAt == nil {
		t.Fatalf("undo restore: %+v", undo)
	}
	for i := 0; i < 10; i++ {
		e.do("POST", "/api/undo", nil)
	}
	if r := e.do("POST", "/api/undo", nil); r.status != 409 || r.errorCode(t) != "nothing_to_undo" {
		t.Fatalf("nothing to undo: %d %s", r.status, r.body)
	}
}

func TestMove(t *testing.T) {
	e := start(t, t.TempDir())
	a := e.create(map[string]any{"title": "a"})
	b := e.create(map[string]any{"title": "b"})
	c := e.create(map[string]any{"title": "c"})
	e.must(200, "POST", "/api/tasks/"+c.ID+"/move", map[string]any{"top": true, "version": 1})
	e.must(200, "POST", "/api/tasks/"+a.ID+"/move", map[string]any{"after": b.ID[:8]})
	if got := titles(e.list("?sort=manual")); !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("manual order: %q", got)
	}
	if r := e.do("POST", "/api/tasks/"+a.ID+"/move", map[string]any{"top": true, "bottom": true}); r.status != 400 {
		t.Errorf("ambiguous placement: %d", r.status)
	}
	if r := e.do("POST", "/api/tasks/"+a.ID+"/move", map[string]any{"top": true, "version": 1}); r.status != 409 {
		t.Errorf("stale move: %d", r.status)
	}
}

func TestBatch(t *testing.T) {
	e := start(t, t.TempDir())
	a := e.create(map[string]any{"title": "a"})
	b := e.create(map[string]any{"title": "b"})
	c := e.create(map[string]any{"title": "c"})

	var res struct {
		OperationID int64       `json:"operation_id"`
		Changed     int         `json:"changed"`
		Tasks       []core.Task `json:"tasks"`
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "priority", "priority": "urgent", "ids": []string{a.ID, b.ID[:8]}}).json(t, &res)
	if res.Changed != 2 || res.OperationID == 0 {
		t.Fatalf("batch priority: %+v", res)
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "move", "category": "home", "ids": []string{a.ID, c.ID}})
	if got := titles(e.list("?category=home")); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("batch move: %q", got)
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "move", "parent_id": a.ID, "ids": []string{b.ID}})
	if e.get(b.ID).ParentID != a.ID {
		t.Fatal("batch move parent")
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "move", "parent_id": "none", "ids": []string{b.ID}})
	if e.get(b.ID).ParentID != "" {
		t.Fatal("batch move to top level")
	}

	// One stale item fails the whole batch: nothing changes.
	cur := e.get(a.ID)
	r := e.do("POST", "/api/batch", map[string]any{"action": "complete",
		"items": []map[string]any{{"id": a.ID, "version": cur.Version}, {"id": b.ID, "version": 1}}})
	if r.status != 409 || r.errorCode(t) != "version_conflict" {
		t.Fatalf("stale batch: %d %s", r.status, r.body)
	}
	if e.get(a.ID).Status != core.StatusTodo {
		t.Fatal("a partially applied batch changed a task")
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "done", "ids": []string{a.ID, b.ID}})
	e.must(200, "POST", "/api/batch", map[string]any{"action": "archive", "ids": []string{a.ID, b.ID}})
	if got := titles(e.list("")); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("after batch archive: %q", got)
	}
	// The whole batch is one undoable step.
	e.must(200, "POST", "/api/undo", nil)
	if got := titles(e.list("?status=done")); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("undo batch archive: %q", got)
	}
	e.must(200, "POST", "/api/batch", map[string]any{"action": "delete", "ids": []string{a.ID, b.ID}})
	e.must(200, "POST", "/api/batch", map[string]any{"action": "restore", "ids": []string{a.ID}})

	for _, body := range []map[string]any{
		{"action": "explode", "ids": []string{a.ID}},
		{"action": "complete"},
		{"action": "priority", "ids": []string{a.ID}},
		{"action": "move", "ids": []string{a.ID}},
		{"action": "complete", "ids": []string{"ffffffff"}},
	} {
		if r := e.do("POST", "/api/batch", body); r.status != 400 && r.status != 404 {
			t.Errorf("%v: %d %s", body, r.status, r.body)
		}
	}
}

func TestVersionsAndRevert(t *testing.T) {
	e := start(t, t.TempDir())
	task := e.create(map[string]any{"title": "v1", "priority": "low"})
	e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 1, "title": "v2", "priority": "high"})
	e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 2, "title": "v3"})

	var vs struct {
		Versions []core.TaskVersion `json:"versions"`
	}
	e.must(200, "GET", "/api/tasks/"+task.ID+"/versions", nil).json(t, &vs)
	if len(vs.Versions) != 3 || vs.Versions[0].Task.Title != "v1" || vs.Versions[2].Task.Title != "v3" || vs.Versions[1].Actor != "web" {
		t.Fatalf("versions: %+v", vs)
	}
	var one taskResp
	e.must(200, "GET", "/api/tasks/"+task.ID+"/versions/1", nil).json(t, &one)
	if one.Task.Title != "v1" || one.Task.Version != 1 {
		t.Fatalf("version 1: %+v", one.Task)
	}

	if r := e.do("POST", "/api/tasks/"+task.ID+"/versions/1/revert", nil); r.status != 428 {
		t.Fatalf("revert without version: %d", r.status)
	}
	if r := e.do("POST", "/api/tasks/"+task.ID+"/versions/1/revert", map[string]any{"version": 2}); r.status != 409 {
		t.Fatalf("stale revert: %d", r.status)
	}
	var rev taskResp
	e.must(200, "POST", "/api/tasks/"+task.ID+"/versions/1/revert", map[string]any{"version": 3}).json(t, &rev)
	if rev.Task.Title != "v1" || rev.Task.Priority != core.PriorityLow || rev.Task.Version != 4 {
		t.Fatalf("reverted: %+v", rev.Task)
	}
	hist, _ := e.store.History(task.ID)
	if last := hist[len(hist)-1]; last.Action != "revert" || last.Changes["title"].To != "v1" {
		t.Fatalf("revert history: %+v", last)
	}
}

func TestHealthAndIndex(t *testing.T) {
	e := start(t, t.TempDir())
	var h struct {
		OK       bool   `json:"ok"`
		Version  string `json:"version"`
		Schema   int    `json:"schema_version"`
		Revision int64  `json:"revision"`
	}
	r := e.must(200, "GET", "/api/health", nil)
	r.json(t, &h)
	if !h.OK || h.Version != "test" || h.Schema != core.LatestSchemaVersion() || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("health: %+v %v", h, r.header)
	}
	r = e.must(200, "GET", "/", nil)
	if !strings.Contains(string(r.body), `lang="zh-CN"`) || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %s", r.body)
	}
}
