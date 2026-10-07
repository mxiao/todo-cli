package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// webCall sends one request the way the web UI's API client does.
func webCall(t *testing.T, base, method, path string, body any) map[string]json.RawMessage {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return out
}

type historyStep struct {
	Action  string
	Changes map[string]core.Change
}

// historyShape drops what legitimately differs between two tasks that went
// through the same operations: ids, titles, timestamps and positions.
func historyShape(hist []core.HistoryEntry) []historyStep {
	out := make([]historyStep, len(hist))
	for i, e := range hist {
		ch := map[string]core.Change{}
		for k, v := range e.Changes {
			switch k {
			case "title", "position", "updated_at", "completed_at", "archived_at", "deleted_at":
				continue
			}
			ch[k] = v
		}
		out[i] = historyStep{Action: e.Action, Changes: ch}
	}
	return out
}

func changedFields(hist []core.HistoryEntry) [][]string {
	out := make([][]string, len(hist))
	for i, e := range hist {
		for k := range e.Changes {
			out[i] = append(out[i], k)
		}
		slices.Sort(out[i])
	}
	return out
}

// FR-706: each task operation the web page performs through the REST API
// yields the same state transition and history entry as the CLI command of
// the same name; only the recorded actor differs ("web" vs "cli").
func TestWebOperationsMatchCLIHistory(t *testing.T) {
	h := newHarness(t)
	out, _ := h.serve("--json", "--port", "0")
	var info serveInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &info); err != nil {
		t.Fatal(err)
	}
	api := strings.TrimSuffix(info.URL, "/")

	// Web: the request bodies apps/web/js/app.js sends for each action.
	var created struct{ Task core.Task }
	raw := webCall(t, api, "POST", "/api/tasks", map[string]any{"title": "网页任务", "status": "todo", "priority": "high", "tags": []string{"x"}})
	_ = json.Unmarshal(raw["task"], &created.Task)
	web := created.Task.ID
	version := func() int64 {
		var tk core.Task
		_ = json.Unmarshal(webCall(t, api, "GET", "/api/tasks/"+web, nil)["task"], &tk)
		return tk.Version
	}
	webCall(t, api, "PATCH", "/api/tasks/"+web, map[string]any{"version": version(), "title": "网页任务2", "priority": "medium", "notes": "补充", "due_at": "2030-01-02T10:00:00Z"})
	for _, st := range []string{"done", "todo", "in_progress", "archived", "todo"} {
		webCall(t, api, "POST", "/api/tasks/"+web+"/status", map[string]any{"status": st, "version": version()})
	}
	webCall(t, api, "POST", "/api/batch", map[string]any{"action": "priority", "priority": "urgent", "items": []map[string]any{{"id": web, "version": version()}}})
	webCall(t, api, "POST", "/api/batch", map[string]any{"action": "move", "category": "proj", "items": []map[string]any{{"id": web, "version": version()}}})
	webCall(t, api, "POST", "/api/batch", map[string]any{"action": "complete", "items": []map[string]any{{"id": web, "version": version()}}})
	webCall(t, api, "POST", "/api/batch", map[string]any{"action": "reopen", "items": []map[string]any{{"id": web, "version": version()}}})
	webCall(t, api, "DELETE", "/api/tasks/"+web, map[string]any{"version": version()})
	webCall(t, api, "POST", "/api/tasks/"+web+"/restore", map[string]any{"version": version()})

	// CLI: the commands of the same name.
	cli := h.add("命令行任务", "-p", "high", "-t", "x").ID
	h.ok("edit", cli, "--title", "命令行任务2", "-p", "medium", "-n", "补充", "--due", "2030-01-02T10:00:00Z")
	for _, cmd := range []string{"done", "reopen", "start", "archive", "reopen"} {
		h.ok(cmd, cli)
	}
	h.ok("priority", "urgent", cli)
	h.ok("move", cli, "--category", "proj")
	h.ok("batch", "done", cli)
	h.ok("batch", "reopen", cli)
	h.ok("delete", cli)
	h.ok("restore", cli)

	var w, c taskOut
	h.json(&w, "show", web)
	h.json(&c, "show", cli)
	wantActions := []string{"create", "update", "complete", "reopen", "start", "archive", "reopen", "priority", "move", "complete", "reopen", "delete", "restore"}
	var got []string
	for _, e := range w.History {
		got = append(got, e.Action)
		if e.Actor != "web" {
			t.Errorf("web history actor %q", e.Actor)
		}
	}
	for _, e := range c.History {
		if e.Actor != "cli" {
			t.Errorf("cli history actor %q", e.Actor)
		}
	}
	if !slices.Equal(got, wantActions) {
		t.Fatalf("web actions %v, want %v", got, wantActions)
	}
	if !reflect.DeepEqual(changedFields(w.History), changedFields(c.History)) {
		t.Errorf("changed fields differ:\nweb %v\ncli %v", changedFields(w.History), changedFields(c.History))
	}
	if ws, cs := historyShape(w.History), historyShape(c.History); !reflect.DeepEqual(ws, cs) {
		t.Errorf("history differs:\nweb %+v\ncli %+v", ws, cs)
	}
	norm := func(t core.Task) core.Task {
		t.ID, t.Title, t.Position = "", "", 0
		t.CreatedAt, t.UpdatedAt = time.Time{}, time.Time{}
		t.CompletedAt, t.ArchivedAt, t.DeletedAt = nil, nil, nil
		return t
	}
	if wt, ct := norm(w.Task), norm(c.Task); !reflect.DeepEqual(wt, ct) {
		t.Errorf("final state differs:\nweb %+v\ncli %+v", wt, ct)
	}
}
