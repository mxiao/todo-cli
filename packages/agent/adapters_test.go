package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mxiao/todo-cli/packages/llm"
)

// crmAdapter stands for a dedicated adapter of an application that is not
// built in: it is registered at runtime and reads its settings from Options.
type crmAdapter struct {
	spec Spec
}

func (a *crmAdapter) Run(ctx context.Context, job *Job, sink Sink) Exit {
	var in Input
	if err := json.Unmarshal(job.Input, &in); err != nil {
		return Exit{Code: -1, Err: err}
	}
	sink.Emit(EventLog, "", "creating ticket in "+a.spec.Options["board"], nil)
	sink.Message(map[string]any{"type": "result", "result_type": "task_status", "system": "crm", "status": "open",
		"url": "https://crm.example/" + a.spec.Options["board"] + "/1", "external_id": "CRM-1"})
	sink.Message(map[string]any{"type": "result", "result_type": "data", "data": map[string]any{"title": in.Task.Title}})
	return Exit{Code: 0}
}

func TestRegisteredDedicatedAdapter(t *testing.T) {
	Register("test-crm", func(sp Spec, _ Deps) (Adapter, error) { return &crmAdapter{spec: sp}, nil })
	f := newFixture(t)
	if _, err := f.m.PutAgent(Spec{Name: "crm", Adapter: "test-crm", Options: map[string]string{"board": "sales"}}); err != nil {
		t.Fatal(err)
	}
	task := f.task("跟进客户")
	r := f.run(task.ID, "crm")
	if r.Status != StatusSucceeded || r.Adapter != "test-crm" || len(r.Results) != 2 {
		t.Fatalf("run %+v", r)
	}
	st := r.Results[0]
	if st.Type != ResultTaskStatus || st.Data["url"] != "https://crm.example/sales/1" || st.Data["external_id"] != "CRM-1" {
		t.Fatalf("status result %+v", st)
	}
	if r.Results[1].Data["data"].(map[string]any)["title"] != "跟进客户" {
		t.Fatalf("data result %+v", r.Results[1])
	}
	// Unknown adapters are refused, and a configured agent whose adapter is
	// missing reports "not available" instead of pretending to work.
	if _, err := f.m.PutAgent(Spec{Name: "x", Adapter: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown adapter accepted: %v", err)
	}
	if _, err := f.svc.UpdateSettings(func(s *llm.Settings) error {
		s.Agents = append(s.Agents, llm.AgentProfile{Name: "gone", Adapter: "uninstalled"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Start(context.Background(), StartRequest{TaskID: task.ID, Agent: "gone"}); err == nil ||
		!strings.Contains(err.Error(), KindAdapterUnavailable) {
		t.Fatalf("missing adapter: %v", err)
	}
}

func TestHTTPAdapter(t *testing.T) {
	var mu sync.Mutex
	var got struct {
		auth, key, ctype string
		body             Input
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got.auth, got.key, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		mu.Unlock()
		switch r.URL.Path {
		case "/stream":
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintln(w, `{"type":"progress","stage":"build","percent":50}`)
			fmt.Fprintln(w, `{"type":"log","message":"token Bearer `+got.auth[7:]+`"}`)
			fmt.Fprintln(w, `{"type":"result","result_type":"commit","repo":"git@example:app","branch":"feat/x","commit":"89abcdef01"}`)
		case "/doc":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"succeeded","results":[{"type":"text","text":"from the API"}]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"database down"}`)
		}
	}))
	defer srv.Close()

	f := newFixture(t)
	f.env["CI_TOKEN"] = "ci-token-abcdef123456"
	for _, a := range []Spec{
		{Name: "stream", Adapter: "http", URL: srv.URL + "/stream", HeaderEnv: map[string]string{"Authorization": "CI_TOKEN"}},
		{Name: "doc", Adapter: "http", URL: srv.URL + "/doc"},
		{Name: "broken", Adapter: "http", URL: srv.URL + "/fail"},
		{Name: "missing-env", Adapter: "http", URL: srv.URL + "/doc", HeaderEnv: map[string]string{"X-Token": "UNSET_VAR"}},
	} {
		if _, err := f.m.PutAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	task := f.task("部署")
	r := f.run(task.ID, "stream")
	if r.Status != StatusSucceeded || len(r.Results) != 1 || r.Results[0].Data["branch"] != "feat/x" || r.Progress != 50 {
		t.Fatalf("stream run %+v", r)
	}
	mu.Lock()
	if got.auth != "ci-token-abcdef123456" || got.key != task.ID+":"+r.ID || got.ctype != "application/json" || got.body.Task.Title != "部署" {
		t.Fatalf("request %+v", got)
	}
	mu.Unlock()
	if strings.Contains(f.log(r.ID), "ci-token-abcdef123456") {
		t.Fatalf("header token logged:\n%s", f.log(r.ID))
	}
	if r := f.run(task.ID, "doc"); r.Status != StatusSucceeded || len(r.Results) != 1 || r.Results[0].Data["text"] != "from the API" {
		t.Fatalf("doc run %+v", r)
	}
	r = f.run(task.ID, "broken")
	if r.Status != StatusFailed || r.ExitCode == nil || *r.ExitCode != 500 || !strings.Contains(r.Error, "database down") {
		t.Fatalf("broken run %+v", r)
	}
	r = f.run(task.ID, "missing-env")
	if r.Status != StatusFailed || !strings.Contains(r.Error, "UNSET_VAR") {
		t.Fatalf("missing env %+v", r)
	}
	if _, err := f.m.Pause(f.start(StartRequest{TaskID: task.ID, Agent: "doc"}).ID); err != nil {
		t.Fatalf("pausing a queued http run: %v", err)
	}
}

func TestPromptAgentAndAutoSelection(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.PutAgent(Spec{Name: "writer", Adapter: "llm", Description: "写文档"}); err != nil {
		t.Fatal(err)
	}
	f.mock("coder", "success", func(s *Spec) { s.Description = "写代码" })
	task := f.task("写一份发布说明")
	f.model.Push(map[string]any{"agent": "writer", "reason": "文档类任务"}, "# 发布说明\n- 新功能 A")
	r := f.run(task.ID, "auto")
	if r.Agent != "writer" || r.Selection.By != "llm" || r.Selection.Reason != "文档类任务" {
		t.Fatalf("selection %+v", r)
	}
	if r.Status != StatusSucceeded || len(r.Results) != 1 || r.Results[0].Data["text"] != "# 发布说明\n- 新功能 A" {
		t.Fatalf("prompt agent %+v", r)
	}
	req := f.model.LastRequest()
	if req.Purpose != "agent" || !strings.Contains(req.Messages[1].Content, "写一份发布说明") {
		t.Fatalf("model request %+v", req)
	}
	// Without a usable model, routing rules decide.
	if _, err := f.svc.SetAssistValue("routing", json.RawMessage(`[{"match":"tag:agent","agent":"coder"}]`)); err != nil {
		t.Fatal(err)
	}
	delete(f.env, llm.EnvAPIKey)
	name, sel, err := f.m.SelectAgent(context.Background(), task)
	if err != nil || name != "coder" || sel.By != "routing" || !sel.Degraded {
		t.Fatalf("routing fallback %s %+v %v", name, sel, err)
	}
}

func TestLaunchFromModelDecision(t *testing.T) {
	f := newFixture(t)
	f.svc.SetAgentLauncher(f.m)
	f.mock("coder", "success", func(s *Spec) { s.Description = "写代码" })
	task := f.task("修复登录 bug")
	f.model.Push(map[string]any{"summary": "先修 bug", "actions": []map[string]any{
		{"type": "agent", "agent": "coder", "task": "T1", "reason": "这是代码任务"}}})
	sess, err := f.svc.Decide(context.Background(), llm.DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Items) != 1 || sess.Items[0].Kind != llm.ItemAgent || sess.Items[0].Status != llm.ItemPending {
		t.Fatalf("items %+v", sess.Items)
	}
	shown := sess.Items[0].Command.Prompt
	if !strings.Contains(shown, "修复登录 bug") || !strings.Contains(shown, "大模型建议：这是代码任务") || !strings.Contains(shown, "# 输出格式") {
		t.Fatalf("previewed prompt:\n%s", shown)
	}
	if runs, _ := f.m.List(ListFilter{}); len(runs) != 0 {
		t.Fatalf("the preview created runs: %+v", runs)
	}
	sess, err = f.svc.Apply(context.Background(), sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	it := sess.Items[0]
	if it.Status != llm.ItemApplied || it.Command.RunID == "" || !strings.Contains(it.Message, "已启动智能体运行") {
		t.Fatalf("item %+v", it)
	}
	r := f.get(it.Command.RunID)
	if r.Status != StatusSucceeded || r.Initiator != llm.Actor || r.SessionID != sess.ID || r.Item != 1 || r.Prompt != shown || r.TaskID != task.ID ||
		r.Selection.By != "llm" {
		t.Fatalf("run %+v", r)
	}
	acts, err := f.svc.Actions(5)
	if err != nil || len(acts) != 1 || acts[0].RunID != r.ID || acts[0].Status != "started" {
		t.Fatalf("actions %+v %v", acts, err)
	}
}
