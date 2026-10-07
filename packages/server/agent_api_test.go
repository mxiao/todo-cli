package server_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/agent"
	"github.com/mxiao/todo-cli/packages/llm"
)

type runResp struct {
	Run agent.Run `json:"run"`
}

func (e *testEnv) run(id string) agent.Run {
	e.t.Helper()
	var r runResp
	e.must(200, "GET", "/api/agent-runs/"+id, nil).json(e.t, &r)
	return r.Run
}

func (e *testEnv) waitRun(id string, status ...string) agent.Run {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := e.run(id)
		for _, s := range status {
			if r.Status == s {
				return r
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("run %s is %s, want %v", id, r.Status, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAgentRunsOverHTTP(t *testing.T) {
	e, _ := startLLM(t, map[string]string{llm.EnvAPIKey: apiKey})
	script, _ := filepath.Abs("../agent/testdata/mock-agent.sh")
	dir := t.TempDir()
	e.must(200, "PUT", "/api/agents/writer", map[string]any{"command": []string{"/bin/sh", script, "success"}, "dir": dir, "description": "写报告"})
	e.must(200, "PUT", "/api/agents/slow", map[string]any{"command": []string{"/bin/sh", script, "slow"}, "dir": dir})
	var agents struct {
		Agents   []agent.Spec `json:"agents"`
		Adapters []string     `json:"adapters"`
	}
	e.must(200, "GET", "/api/agents", nil).json(t, &agents)
	if len(agents.Agents) != 2 || agents.Agents[0].Name != "writer" || len(agents.Adapters) < 3 {
		t.Fatalf("agents %+v", agents)
	}
	if r := e.do("PUT", "/api/agents/root", map[string]any{"command": []string{"sudo", "x"}}); r.status != 400 {
		t.Fatalf("sudo agent: %d %s", r.status, r.body)
	}
	task := e.create(map[string]any{"title": "网页启动智能体"})

	var preview runResp
	e.must(200, "POST", "/api/tasks/"+task.ID+"/agent-runs", map[string]any{"agent": "writer", "dry_run": true, "context": "补充"}).json(t, &preview)
	if !strings.Contains(preview.Run.Prompt, "网页启动智能体") || !strings.Contains(preview.Run.Prompt, "补充") {
		t.Fatalf("preview %+v", preview.Run)
	}
	var started runResp
	e.must(201, "POST", "/api/tasks/"+task.ID+"/agent-runs", map[string]any{"agent": "writer", "complete_on_success": true}).json(t, &started)
	if started.Run.Initiator != "web" {
		t.Fatalf("started %+v", started.Run)
	}
	r := e.waitRun(started.Run.ID, agent.StatusSucceeded)
	if len(r.Results) != 4 || r.StartedAt == nil || r.EndedAt == nil {
		t.Fatalf("run %+v", r)
	}
	if got := e.get(task.ID); got.Status != "done" {
		t.Fatalf("task %s", got.Status)
	}
	var evs struct {
		Events []agent.Event `json:"events"`
		Done   bool          `json:"done"`
	}
	e.must(200, "GET", "/api/agent-runs/"+r.ID+"/events?kind=command,result", nil).json(t, &evs)
	if len(evs.Events) != 5 || !evs.Done {
		t.Fatalf("events %+v", evs)
	}
	e.must(200, "GET", "/api/agent-runs/"+r.ID+"/events?after="+strconv.FormatInt(evs.Events[4].ID, 10), nil).json(t, &evs)
	for _, ev := range evs.Events {
		if ev.Kind == agent.EventResult || ev.Kind == agent.EventCommand {
			t.Fatalf("after filter returned old event %+v", ev)
		}
	}
	var prompt struct {
		Prompt string `json:"prompt"`
	}
	if e.must(200, "GET", "/api/agent-runs/"+r.ID+"/prompt", nil).json(t, &prompt); !strings.Contains(prompt.Prompt, "# 约束") {
		t.Fatalf("prompt %q", prompt.Prompt)
	}
	if res := e.do("POST", "/api/agent-runs/"+r.ID+"/retry", nil); res.status != 409 || res.errorCode(t) != "invalid_state" {
		t.Fatalf("retry succeeded run: %d %s", res.status, res.body)
	}

	// External systems write results back idempotently.
	commit := map[string]any{"result_type": "commit", "repo": "app", "branch": "main", "commit": "deadbeef42", "source": "ci"}
	var wb struct {
		Result agent.Result `json:"result"`
	}
	e.must(201, "POST", "/api/tasks/"+task.ID+"/results", commit).json(t, &wb)
	if wb.Result.Agent != "ci" || wb.Result.RunID != agent.ManualRun {
		t.Fatalf("writeback %+v", wb.Result)
	}
	e.must(200, "POST", "/api/tasks/"+task.ID+"/results", commit).json(t, &wb)
	if !wb.Result.Duplicate {
		t.Fatalf("second writeback %+v", wb.Result)
	}
	if res := e.do("POST", "/api/tasks/"+task.ID+"/results", map[string]any{"result_type": "commit", "repo": "x"}); res.status != 400 {
		t.Fatalf("invalid result: %d %s", res.status, res.body)
	}
	var results struct {
		Results []agent.Result `json:"results"`
	}
	if e.must(200, "GET", "/api/tasks/"+task.ID+"/results", nil).json(t, &results); len(results.Results) != 5 {
		t.Fatalf("results %+v", results)
	}

	// Pause, resume and cancel a background run.
	e.must(201, "POST", "/api/tasks/"+task.ID+"/agent-runs", map[string]any{"agent": "slow"}).json(t, &started)
	id := started.Run.ID
	e.waitRun(id, agent.StatusRunning)
	e.must(200, "POST", "/api/agent-runs/"+id+"/pause", nil)
	e.waitRun(id, agent.StatusPaused)
	e.must(200, "POST", "/api/agent-runs/"+id+"/resume", nil)
	e.waitRun(id, agent.StatusRunning)
	e.must(200, "POST", "/api/agent-runs/"+id+"/cancel", nil)
	if r := e.waitRun(id, agent.StatusCancelled); r.ErrorKind != agent.KindCancelled {
		t.Fatalf("cancelled %+v", r)
	}
	// Retrying keeps the earlier attempt; stopping the server ends the run.
	e.must(200, "POST", "/api/agent-runs/"+id+"/retry", map[string]any{"context": "再试一次"})
	e.waitRun(id, agent.StatusRunning)
	var list struct {
		Runs []agent.Run `json:"runs"`
	}
	e.must(200, "GET", "/api/agent-runs?task="+task.ID+"&status=running", nil).json(t, &list)
	if len(list.Runs) != 1 || list.Runs[0].ID != id || list.Runs[0].Attempt != 2 {
		t.Fatalf("running runs %+v", list.Runs)
	}
	if res := e.do("POST", "/api/tasks/"+task.ID+"/agent-runs", map[string]any{"agent": "nobody"}); res.status != 404 {
		t.Fatalf("unknown agent: %d %s", res.status, res.body)
	}
	e.srv.Close()
	got, err := e.store.DB().Query(`SELECT status, error_kind FROM agent_runs WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	var status, kind string
	for got.Next() {
		_ = got.Scan(&status, &kind)
	}
	if status != agent.StatusCancelled || kind != agent.KindInterrupted {
		t.Fatalf("after shutdown: %s %s", status, kind)
	}
}
