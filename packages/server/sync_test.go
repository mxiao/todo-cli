package server_test

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/server"
)

type conflictBody struct {
	Error    struct{ Code string } `json:"error"`
	Current  core.Task             `json:"current"`
	Conflict struct {
		ID             int64     `json:"id"`
		TaskID         string    `json:"task_id"`
		BaseVersion    int64     `json:"base_version"`
		CurrentVersion int64     `json:"current_version"`
		Open           bool      `json:"open"`
		Base           core.Task `json:"base"`
		Current        core.Task `json:"current"`
		Yours          core.Task `json:"yours"`
		Merged         core.Task `json:"merged"`
		Fields         []struct {
			Field           string `json:"field"`
			Base            any    `json:"base"`
			Current         any    `json:"current"`
			Yours           any    `json:"yours"`
			ChangedByOthers bool   `json:"changed_by_others"`
			ChangedByYou    bool   `json:"changed_by_you"`
			Conflicting     bool   `json:"conflicting"`
		} `json:"fields"`
	} `json:"conflict"`
}

// Two independent clients (own connections) PATCH the same task from the
// same version at the same moment: exactly one wins, the other gets a 409
// that shows both sides, and its edit is kept and can still be applied.
func TestConcurrentPatchNeverSilentlyOverwrites(t *testing.T) {
	e := start(t, t.TempDir())
	task := e.create(map[string]any{"title": "整理本周项目材料", "notes": "原始备注"})

	type attempt struct {
		name string
		body map[string]any
		resp response
	}
	attempts := []*attempt{
		{name: "terminal", body: map[string]any{"version": task.Version, "title": "整理材料（CLI 版）", "priority": "high"}},
		{name: "browser", body: map[string]any{"version": task.Version, "title": "整理材料（网页版）", "notes": "网页补充"}},
	}
	var ready, wg sync.WaitGroup
	gate := make(chan struct{})
	for _, a := range attempts {
		ready.Add(1)
		wg.Add(1)
		client := &http.Client{Transport: &http.Transport{}}
		go func() {
			defer wg.Done()
			ready.Done()
			<-gate
			a.resp = e.req(client, "PATCH", "/api/tasks/"+task.ID, a.body)
		}()
	}
	ready.Wait()
	close(gate)
	wg.Wait()

	var winner, loser *attempt
	for _, a := range attempts {
		switch a.resp.status {
		case http.StatusOK:
			winner = a
		case http.StatusConflict:
			loser = a
		default:
			t.Fatalf("%s: unexpected %d %s", a.name, a.resp.status, a.resp.body)
		}
	}
	if winner == nil || loser == nil {
		t.Fatalf("want one 200 and one 409, got %d and %d", attempts[0].resp.status, attempts[1].resp.status)
	}

	var cb conflictBody
	loser.resp.json(t, &cb)
	c := cb.Conflict
	if cb.Error.Code != "version_conflict" || c.ID == 0 || !c.Open || c.TaskID != task.ID || c.BaseVersion != 1 || c.CurrentVersion != 2 {
		t.Fatalf("conflict: %s", loser.resp.body)
	}
	if c.Current.Title != winner.body["title"] || c.Base.Title != task.Title || c.Yours.Title != loser.body["title"] {
		t.Fatalf("conflict sides: base=%q current=%q yours=%q", c.Base.Title, c.Current.Title, c.Yours.Title)
	}
	var titleDiff bool
	for _, f := range c.Fields {
		if f.Field == "title" {
			titleDiff = f.Conflicting && f.ChangedByOthers && f.ChangedByYou && f.Base == task.Title
		}
	}
	if !titleDiff {
		t.Fatalf("title should be flagged as conflicting: %+v", c.Fields)
	}

	// Not silently overwritten: the stored task is exactly the winner's edit.
	got := e.get(task.ID)
	if got.Version != 2 || got.Title != winner.body["title"] {
		t.Fatalf("stored task: %+v", got)
	}
	// The winner's version stays recoverable even after the conflict is resolved.
	defer func() {
		var v taskResp
		e.must(200, "GET", "/api/tasks/"+task.ID+"/versions/2", nil).json(t, &v)
		if v.Task.Title != winner.body["title"] {
			t.Fatalf("version 2: %+v", v.Task)
		}
	}()

	// The rejected edit is kept: listed on the task and fetchable by id.
	var list struct {
		Conflicts []json.RawMessage `json:"conflicts"`
	}
	e.must(200, "GET", "/api/tasks/"+task.ID+"/conflicts", nil).json(t, &list)
	if len(list.Conflicts) != 1 {
		t.Fatalf("open conflicts: %d", len(list.Conflicts))
	}
	cid := strconv.FormatInt(c.ID, 10)
	e.must(200, "GET", "/api/conflicts/"+cid, nil)

	// Applying it after the task moved on again is itself rejected...
	e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 2, "category": "work"})
	if r := e.do("POST", "/api/conflicts/"+cid+"/resolve", map[string]any{"resolution": "mine"}); r.status != 409 {
		t.Fatalf("stale resolve: %d %s", r.status, r.body)
	}
	// ...and succeeds once the user confirms the latest version.
	var res struct {
		Task     core.Task     `json:"task"`
		Conflict core.Conflict `json:"conflict"`
	}
	e.must(200, "POST", "/api/conflicts/"+cid+"/resolve", map[string]any{"resolution": "mine", "version": 3}).json(t, &res)
	if res.Task.Title != loser.body["title"] || res.Task.Category != "work" || res.Task.Version != 4 ||
		res.Conflict.Resolution != "mine" || res.Conflict.ResolvedAt == nil {
		t.Fatalf("resolved: %+v", res)
	}
	if n, ok := loser.body["notes"]; ok && res.Task.Notes != n {
		t.Fatalf("loser's notes not applied: %+v", res.Task)
	}
	if r := e.do("POST", "/api/conflicts/"+cid+"/resolve", map[string]any{"resolution": "theirs"}); r.status != 409 || r.errorCode(t) != "conflict_resolved" {
		t.Fatalf("double resolve: %d %s", r.status, r.body)
	}
	e.must(200, "GET", "/api/tasks/"+task.ID+"/conflicts", nil).json(t, &list)
	if len(list.Conflicts) != 0 {
		t.Fatal("resolved conflict still open")
	}
	e.must(200, "GET", "/api/tasks/"+task.ID+"/conflicts?all=true", nil).json(t, &list)
	if len(list.Conflicts) != 1 {
		t.Fatal("resolved conflicts stay queryable")
	}
	// Every applied edit is in the history.
	hist, _ := e.store.History(task.ID)
	var seen []any
	for _, h := range hist {
		if ch, ok := h.Changes["title"]; ok {
			seen = append(seen, ch.To)
		}
	}
	if !slices.Equal(seen, []any{task.Title, winner.body["title"], loser.body["title"]}) {
		t.Fatalf("title history: %v", seen)
	}
}

func TestResolveTheirsAndErrors(t *testing.T) {
	e := start(t, t.TempDir())
	task := e.create(map[string]any{"title": "a"})
	e.must(200, "PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 1, "title": "b"})
	var cb conflictBody
	e.must(409, "PATCH", "/api/tasks/"+task.ID, map[string]any{"version": 1, "title": "c"}).json(t, &cb)
	cid := strconv.FormatInt(cb.Conflict.ID, 10)
	if cb.Current.Title != "b" || cb.Conflict.Merged.Title != "c" {
		t.Fatalf("409 body: %+v", cb)
	}
	if r := e.do("POST", "/api/conflicts/"+cid+"/resolve", map[string]any{"resolution": "both"}); r.status != 400 {
		t.Fatalf("bad resolution: %d", r.status)
	}
	if r := e.do("GET", "/api/conflicts/999", nil); r.status != 404 {
		t.Fatalf("missing conflict: %d", r.status)
	}
	if r := e.do("GET", "/api/conflicts/x", nil); r.status != 400 {
		t.Fatalf("bad conflict id: %d", r.status)
	}
	e.must(200, "POST", "/api/conflicts/"+cid+"/resolve", map[string]any{"resolution": "theirs"})
	if got := e.get(task.ID); got.Title != "b" || got.Version != 2 {
		t.Fatalf("theirs must keep the current task: %+v", got)
	}
}

// Changes made by another process on the same database (the CLI) and by
// the web API reach an open event stream within 2 seconds.
func TestEventsDeliverChangesWithinTwoSeconds(t *testing.T) {
	e := start(t, t.TempDir())
	cli, err := core.Open(core.Options{DataDir: e.dir, Actor: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	es := e.events()
	ready := es.next(t, 2*time.Second, func(ev sseEvent) bool { return ev.Name == "ready" })
	isTask := func(ev sseEvent) bool { return ev.Name == "task" }

	// CLI → web
	began := time.Now()
	created, err := cli.Create(core.NewTask{Title: "CLI 新建"})
	if err != nil {
		t.Fatal(err)
	}
	ce := taskEvent(t, es.next(t, 2*time.Second, isTask))
	if ce.TaskID != created.ID || ce.Action != "create" || ce.Actor != "cli" || ce.Task == nil || ce.Task.Title != "CLI 新建" {
		t.Fatalf("create event: %+v", ce)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("CLI change took %v to arrive", d)
	}
	for _, step := range []struct {
		action string
		do     func() error
	}{
		{"update", func() error {
			_, err := cli.Update(created.ID, core.TaskPatch{Title: new("CLI 修改")})
			return err
		}},
		{"complete", func() error { _, err := cli.Complete(created.ID); return err }},
		{"delete", func() error { _, err := cli.Delete(created.ID); return err }},
	} {
		began := time.Now()
		if err := step.do(); err != nil {
			t.Fatal(err)
		}
		ce := taskEvent(t, es.next(t, 2*time.Second, isTask))
		if ce.Action != step.action || ce.Actor != "cli" || ce.TaskID != created.ID {
			t.Fatalf("%s event: %+v", step.action, ce)
		}
		if time.Since(began) > 2*time.Second {
			t.Fatalf("%s took too long", step.action)
		}
	}

	// Web → stream (and the CLI store reads it immediately).
	web := e.create(map[string]any{"title": "网页新建"})
	ce = taskEvent(t, es.next(t, 2*time.Second, isTask))
	if ce.TaskID != web.ID || ce.Actor != "web" || ce.Task.Version != 1 {
		t.Fatalf("web create event: %+v", ce)
	}
	if got, err := cli.Get(web.ID); err != nil || got.Title != "网页新建" {
		t.Fatalf("CLI sees web task: %+v %v", got, err)
	}
	e.must(200, "PATCH", "/api/tasks/"+web.ID, map[string]any{"version": 1, "title": "网页修改"})
	ce = taskEvent(t, es.next(t, 2*time.Second, isTask))
	if ce.Action != "update" || ce.Changes["title"].To != "网页修改" || ce.Task.Version != 2 {
		t.Fatalf("web update event: %+v", ce)
	}
	es.close()

	// A reconnecting client gets the changes it missed, in order.
	if _, err := cli.Create(core.NewTask{Title: "断线期间新建"}); err != nil {
		t.Fatal(err)
	}
	re := e.events("Last-Event-ID", ready.ID)
	var actions []string
	for len(actions) < 7 {
		ev := re.next(t, 2*time.Second, func(ev sseEvent) bool { return ev.Name == "task" || ev.Name == "ready" })
		if ev.Name == "ready" {
			break
		}
		actions = append(actions, taskEvent(t, ev).Action)
	}
	want := []string{"create", "update", "complete", "delete", "create", "update", "create"}
	if !slices.Equal(actions, want) {
		t.Fatalf("replayed %v, want %v", actions, want)
	}
}

func TestEventsStopOnShutdown(t *testing.T) {
	e := start(t, t.TempDir())
	es := e.events()
	es.next(t, 2*time.Second, func(ev sseEvent) bool { return ev.Name == "ready" })
	done := make(chan struct{})
	go func() { e.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung on an open event stream")
	}
}

// Everything written through the API survives a restart of the service.
func TestRestartRestoresTasksStatusAndHistory(t *testing.T) {
	dir := t.TempDir()
	e := start(t, dir)
	a := e.create(map[string]any{"title": "a", "tags": []string{"x"}, "due_at": "2026-10-09"})
	b := e.create(map[string]any{"title": "b", "parent_id": a.ID})
	c := e.create(map[string]any{"title": "c"})
	e.must(200, "PATCH", "/api/tasks/"+a.ID, map[string]any{"version": 1, "notes": "补充", "priority": "urgent"})
	e.must(200, "POST", "/api/tasks/"+b.ID+"/status", map[string]any{"status": "done"})
	e.must(200, "DELETE", "/api/tasks/"+c.ID, nil)
	e.must(409, "PATCH", "/api/tasks/"+a.ID, map[string]any{"version": 1, "title": "stale"})

	snapshot := func(e *testEnv) map[string]any {
		out := map[string]any{}
		for _, path := range []string{
			"/api/tasks?status=all&deleted=include",
			"/api/tasks/" + a.ID, "/api/tasks/" + b.ID, "/api/tasks/" + c.ID,
			"/api/tasks/" + a.ID + "/versions", "/api/tasks/" + a.ID + "/conflicts?all=1",
		} {
			var v map[string]any
			e.must(200, "GET", path, nil).json(t, &v)
			delete(v, "revision")
			out[path] = v
		}
		return out
	}
	before := snapshot(e)
	e.stop()

	e2 := start(t, dir)
	after := snapshot(e2)
	if !reflect.DeepEqual(before, after) {
		b1, _ := json.MarshalIndent(before, "", " ")
		b2, _ := json.MarshalIndent(after, "", " ")
		t.Fatalf("state changed across restart:\nbefore %s\nafter %s", b1, b2)
	}
	if got := e2.get(b.ID); got.Status != core.StatusDone || got.CompletedAt == nil {
		t.Fatalf("status after restart: %+v", got)
	}
	hist, _ := e2.store.History(a.ID)
	if len(hist) != 2 || hist[1].Changes["notes"].To != "补充" {
		t.Fatalf("history after restart: %+v", hist)
	}
	// Editing keeps working on the restored version numbers.
	e2.must(200, "PATCH", "/api/tasks/"+a.ID, map[string]any{"version": 2, "title": "a2"})
}

func TestLocalOnlyGuard(t *testing.T) {
	e := start(t, t.TempDir())
	if r := e.do("GET", "/api/tasks", nil, "Host", "evil.example:3210"); r.status != 403 || r.errorCode(t) != "forbidden_host" {
		t.Fatalf("foreign Host (DNS rebinding): %d %s", r.status, r.body)
	}
	for _, h := range []string{"localhost:3210", "127.0.0.1", "[::1]:3210"} {
		if r := e.do("GET", "/api/health", nil, "Host", h); r.status != 200 {
			t.Errorf("Host %s: %d", h, r.status)
		}
	}
	host := e.ts.Listener.Addr().String()
	if r := e.do("POST", "/api/tasks", map[string]any{"title": "x"}, "Origin", "http://evil.example"); r.status != 403 || r.errorCode(t) != "forbidden_origin" {
		t.Fatalf("cross-origin write: %d %s", r.status, r.body)
	}
	if r := e.do("POST", "/api/tasks", map[string]any{"title": "x"}, "Origin", "http://localhost:9999"); r.status != 403 {
		t.Fatalf("other local origin: %d", r.status)
	}
	if r := e.do("POST", "/api/tasks", map[string]any{"title": "x"}, "Sec-Fetch-Site", "cross-site"); r.status != 403 {
		t.Fatalf("cross-site fetch: %d", r.status)
	}
	e.must(201, "POST", "/api/tasks", map[string]any{"title": "same origin"}, "Origin", "http://"+host)
	if n := len(e.list("")); n != 1 {
		t.Fatalf("blocked writes must not be applied: %d tasks", n)
	}
}

func TestListenLoopbackAndPortFallback(t *testing.T) {
	if server.DefaultHost != "127.0.0.1" || server.DefaultPort != 3210 {
		t.Fatal("defaults changed")
	}
	for _, h := range []string{"0.0.0.0", "192.168.1.10", "::", "example.com"} {
		if _, err := server.Listen(h, 0, 1); !errors.Is(err, server.ErrNotLoopback) {
			t.Errorf("%s: %v", h, err)
		}
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	ln, err := server.Listen("localhost", port, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := ln.Addr().(*net.TCPAddr)
	if !got.IP.IsLoopback() || got.Port <= port || got.Port >= port+10 {
		t.Fatalf("fallback listener %v (busy port %d)", got, port)
	}
	if u := server.URL(ln); u != "http://127.0.0.1:"+strconv.Itoa(got.Port)+"/" {
		t.Fatalf("url %q", u)
	}
	if _, err := server.Listen("127.0.0.1", port, 1); !errors.Is(err, server.ErrPortsInUse) {
		t.Fatalf("single busy port: %v", err)
	}
}

func TestEventsResyncWhenClientIsAhead(t *testing.T) {
	e := start(t, t.TempDir())
	e.create(map[string]any{"title": "a"})
	es := e.events("Last-Event-ID", "999999")
	ev := es.next(t, 2*time.Second, func(sseEvent) bool { return true })
	if ev.Name != "resync" {
		t.Fatalf("want resync first, got %+v", ev)
	}
	if r := e.do("GET", "/api/events?since=-4", nil); r.status != 400 {
		t.Fatalf("bad since: %d", r.status)
	}
}
