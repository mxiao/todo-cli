package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
	"github.com/mxiao/todo-cli/packages/server"
)

const apiKey = "sk-api-test-secret-5555555"

// startLLM runs the service with a scripted model.
func startLLM(t *testing.T, env map[string]string, responses ...any) (*testEnv, *llm.Scripted) {
	t.Helper()
	st, err := core.Open(core.Options{DataDir: t.TempDir(), Actor: "web"})
	if err != nil {
		t.Fatal(err)
	}
	model := llm.NewScripted(responses...)
	svc, err := llm.Open(st, llm.Options{Getenv: func(k string) string { return env[k] }, Secrets: &llm.MemorySecrets{},
		NewClient: func(*llm.Resolved) (llm.Client, error) { return model, nil }, Origin: "web"})
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(st, server.Options{PollInterval: 50 * time.Millisecond, LLM: svc})
	ts := httptest.NewServer(srv)
	e := &testEnv{t: t, store: st, srv: srv, ts: ts, client: &http.Client{Transport: &http.Transport{}}}
	t.Cleanup(e.stop)
	return e, model
}

type sessionResp struct {
	Session  llm.Session `json:"session"`
	Revision int64       `json:"revision"`
}

func TestLLMIntakeDecideOverHTTP(t *testing.T) {
	e, _ := startLLM(t, map[string]string{llm.EnvAPIKey: apiKey},
		map[string]any{"items": []map[string]any{{"action": "create", "title": "写周报", "priority": "high"},
			{"action": "create", "title": "发邮件"}}})
	var st llm.StatusReport
	r := e.must(200, "GET", "/api/llm/status", nil)
	r.json(t, &st)
	if !st.Available || st.Mode != llm.ModeConfirm || strings.Contains(string(r.body), apiKey) {
		t.Fatalf("status %s", r.body)
	}

	var res sessionResp
	e.must(200, "POST", "/api/llm/intake", map[string]any{"text": "写周报（高优先级），发邮件 " + apiKey}).json(t, &res)
	sid := res.Session.ID
	if res.Session.Status != llm.StatusPending || len(e.list("")) != 0 || len(res.Session.Items[0].Diff) == 0 {
		t.Fatalf("intake %+v", res.Session)
	}
	if strings.Contains(res.Session.Input, apiKey) {
		t.Fatal("key in session")
	}
	e.must(200, "PATCH", "/api/llm/sessions/"+sid+"/items/2", map[string]any{"title": "发周报邮件"})
	e.must(200, "POST", "/api/llm/sessions/"+sid+"/apply", map[string]any{"items": []int{1}})
	e.must(200, "POST", "/api/llm/sessions/"+sid+"/reject", map[string]any{})
	e.must(200, "GET", "/api/llm/sessions/"+sid, nil).json(t, &res.Session)
	if r := res.Session.Result; r.Created != 1 || r.NotExecuted != 1 {
		t.Fatalf("result %+v", r)
	}
	tasks := e.list("")
	if len(tasks) != 1 || tasks[0].Title != "写周报" {
		t.Fatalf("tasks %v", titles(tasks))
	}
	var hist struct {
		History []core.HistoryEntry `json:"history"`
	}
	e.must(200, "GET", "/api/tasks/"+tasks[0].ID+"/history", nil).json(t, &hist)
	if hist.History[0].Actor != llm.Actor {
		t.Fatalf("history %+v", hist.History)
	}
	if code := e.must(409, "POST", "/api/llm/sessions/"+sid+"/apply", map[string]any{}).errorCode(t); code != "invalid_state" {
		t.Fatalf("code %s", code)
	}
	e.must(200, "POST", "/api/llm/sessions/"+sid+"/undo", map[string]any{})
	if len(e.list("")) != 0 {
		t.Fatal("undo over HTTP")
	}
	var list struct {
		Sessions []llm.SessionSummary `json:"sessions"`
	}
	e.must(200, "GET", "/api/llm/sessions?kind=intake", nil).json(t, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].Status != llm.StatusUndone {
		t.Fatalf("sessions %+v", list.Sessions)
	}

	// Mode switch and the confirm list.
	e.must(200, "POST", "/api/llm/mode", map[string]any{"mode": "自动执行"}).json(t, &st)
	if st.Mode != llm.ModeAuto {
		t.Fatalf("mode %s", st.Mode)
	}
	e.must(200, "PUT", "/api/llm/confirm-list", map[string]any{"entries": []string{"git push"}}).json(t, &st)
	if len(st.ConfirmList) != 1 {
		t.Fatalf("confirm list %v", st.ConfirmList)
	}
	e.must(400, "POST", "/api/llm/mode", map[string]any{"mode": "yolo"})
	// Cross-site writes are refused as for tasks.
	e.must(403, "POST", "/api/llm/intake", map[string]any{"text": "x"}, "Origin", "https://evil.example")
}

func TestLLMUnavailableOverHTTP(t *testing.T) {
	e, model := startLLM(t, map[string]string{})
	r := e.must(503, "POST", "/api/llm/intake", map[string]any{"text": "写周报"})
	var body struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	r.json(t, &body)
	if body.Error.Code != llm.KindNotConfigured || body.Error.Hint == "" || len(model.Requests) != 0 {
		t.Fatalf("unavailable %s", r.body)
	}
	// Tasks keep working, and decisions fall back to local rules.
	e.create(map[string]any{"title": "手动任务"})
	var res sessionResp
	e.must(200, "POST", "/api/llm/decide", map[string]any{}).json(t, &res)
	if !res.Session.Degraded || len(res.Session.Recommendations) != 1 {
		t.Fatalf("degraded decide %+v", res.Session)
	}
	var rep llm.ConnectionReport
	e.must(200, "POST", "/api/llm/test", map[string]any{}).json(t, &rep)
	if rep.OK || rep.Error == nil || rep.Error.Kind != llm.KindNotConfigured {
		t.Fatalf("test %+v", rep)
	}
}

func TestPromptsOverHTTP(t *testing.T) {
	e, _ := startLLM(t, map[string]string{llm.EnvAPIKey: apiKey}, map[string]any{
		"goal": "总结 {{主题}} 的会议", "steps": []string{"列要点", "列待办"}, "output_format": "列表",
	})
	var sum struct {
		Body     string           `json:"body"`
		Template *prompt.Template `json:"template"`
	}
	e.must(200, "POST", "/api/prompts/summarize", map[string]any{"text": "请总结这次会议……很长", "save": true, "name": "会议纪要"}).json(t, &sum)
	if sum.Template == nil || !strings.Contains(sum.Body, "{{主题}}") {
		t.Fatalf("summarize %+v", sum)
	}
	id := sum.Template.ID
	var tpl prompt.Template
	e.must(200, "PUT", "/api/prompts/"+id, map[string]any{"body": "总结 {{主题}}，不超过 {{字数}} 字", "note": "精简"}).json(t, &tpl)
	if tpl.Current != 2 {
		t.Fatalf("edit %+v", tpl)
	}
	r := e.must(400, "POST", "/api/prompts/"+id+"/render", map[string]any{"values": map[string]string{"主题": "周会"}})
	if !strings.Contains(string(r.body), "missing_variables") || !strings.Contains(string(r.body), "字数") {
		t.Fatalf("missing vars %s", r.body)
	}
	var out struct {
		Prompt string    `json:"prompt"`
		Task   core.Task `json:"task"`
	}
	e.must(200, "POST", "/api/prompts/"+id+"/render", map[string]any{"values": map[string]string{"主题": "周会", "字数": "200"}}).json(t, &out)
	if out.Prompt != "总结 周会，不超过 200 字" {
		t.Fatalf("render %q", out.Prompt)
	}
	e.must(201, "POST", "/api/prompts/"+id+"/task", map[string]any{"values": map[string]string{"主题": "周会", "字数": "200"}}).json(t, &out)
	if out.Task.Description != out.Prompt || len(e.list("")) != 1 {
		t.Fatalf("task %+v", out.Task)
	}
	e.must(200, "POST", "/api/prompts/"+id+"/rollback", map[string]any{}).json(t, &tpl)
	if tpl.Current != 3 || !strings.Contains(tpl.Latest.Body, "## 执行步骤") {
		t.Fatalf("rollback %+v", tpl.Latest)
	}
	e.must(201, "POST", "/api/prompts/"+id+"/copy", map[string]any{"name": "会议纪要 2"})
	r = e.must(200, "GET", "/api/prompts/"+id+"/export?format=json", nil)
	if !strings.Contains(r.header.Get("Content-Type"), "json") || !strings.Contains(string(r.body), "很长") {
		t.Fatalf("export %s", r.body)
	}
	var vs struct {
		Versions []prompt.Version `json:"versions"`
	}
	e.must(200, "GET", "/api/prompts/"+id+"/versions", nil).json(t, &vs)
	if len(vs.Versions) != 3 {
		t.Fatalf("versions %d", len(vs.Versions))
	}
	e.must(200, "DELETE", "/api/prompts/"+id, nil)
	e.must(404, "GET", "/api/prompts/"+id, nil)
	var all struct {
		Templates []prompt.Template `json:"templates"`
	}
	e.must(200, "GET", "/api/prompts", nil).json(t, &all)
	if len(all.Templates) != 1 {
		t.Fatalf("templates %d", len(all.Templates))
	}
}
