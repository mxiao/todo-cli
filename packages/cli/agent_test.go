package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/agent"
)

func agentHarness(t *testing.T) (*harness, string, string) {
	h := newHarness(t)
	h.env["PATH"] = "/usr/bin:/bin"
	script, err := filepath.Abs("../agent/testdata/mock-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	return h, script, t.TempDir()
}

func TestAgentRunShowLogsAndResults(t *testing.T) {
	h, script, dir := agentHarness(t)
	h.ok("agent", "add", "writer", "--desc", "写报告", "--dir", dir, "--timeout", "2m", "--", "/bin/sh", script, "success")
	var list struct {
		Agents   []agent.Spec `json:"agents"`
		Adapters []string     `json:"adapters"`
	}
	h.json(&list, "agent", "list")
	if len(list.Agents) != 1 || list.Agents[0].Name != "writer" || list.Agents[0].TimeoutSeconds != 120 || list.Agents[0].Input != agent.InputJSONStdin ||
		!strings.Contains(strings.Join(list.Adapters, ","), "http") {
		t.Fatalf("agents %+v", list)
	}
	id := h.add("写周报").ID

	preview := h.ok("agent", "run", id, "--agent", "writer", "--dry-run", "--context", "本周重点：发布")
	if !strings.Contains(preview, "# 任务") || !strings.Contains(preview, "本周重点：发布") {
		t.Fatalf("preview:\n%s", preview)
	}
	out := h.ok("agent", "run", id, "--agent", "writer", "--complete")
	for _, want := range []string{"运行 r", "智能体 writer", "▸", "prepare 10%", "$ git commit -m report", "│ plain output line",
		"✔", "结果 [commit]", "结果：成功", "写回 4 个结果"} {
		if !strings.Contains(out, want) {
			t.Errorf("run output lacks %q:\n%s", want, out)
		}
	}
	var runs struct {
		Runs []agent.Run `json:"runs"`
	}
	h.json(&runs, "agent", "runs", "--task", id)
	if len(runs.Runs) != 1 || runs.Runs[0].Status != agent.StatusSucceeded {
		t.Fatalf("runs %+v", runs)
	}
	rid := runs.Runs[0].ID
	if task := h.showTask(id); task.Task.Status != "done" {
		t.Fatalf("--complete: task %s", task.Task.Status)
	}
	show := h.ok("agent", "show", rid[:6])
	for _, want := range []string{"成功", "写周报", "writer（cli 适配器；用户指定）", "执行记录：", "#1 成功", "[file] weekly report — 来源 writer", "report.md", "0123456789abcdef"} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}
	if logs := h.ok("agent", "logs", rid, "--kind", "command"); strings.Count(logs, "\n") != 1 || !strings.Contains(logs, "git commit") {
		t.Fatalf("logs:\n%s", logs)
	}
	if p := h.ok("agent", "prompt", rid); !strings.Contains(p, "写周报") || !strings.Contains(p, "# 输出格式") {
		t.Fatalf("prompt:\n%s", p)
	}
	res := h.ok("agent", "results", id)
	for _, typ := range []string{"[text]", "[file]", "[commit]", "[command_output]"} {
		if !strings.Contains(res, typ) {
			t.Errorf("results lack %s:\n%s", typ, res)
		}
	}
	if out := h.ok("agent", "writeback", id, `{"result_type":"text","text":"人工补充"}`); !strings.Contains(out, "已写回结果 v1") {
		t.Fatalf("writeback: %s", out)
	}
	if out := h.ok("agent", "writeback", id, `{"result_type":"text","text":"人工补充"}`); !strings.Contains(out, "未重复写入") {
		t.Fatalf("writeback twice: %s", out)
	}
	// The task history shows the run and its results (FR-108, FR-511).
	hist := h.ok("history", id)
	for _, want := range []string{"agent_run", "agent_result", "agent_succeeded", "agent/writer"} {
		if !strings.Contains(hist, want) {
			t.Errorf("history lacks %q:\n%s", want, hist)
		}
	}
}

func TestAgentFailureRetryAndControl(t *testing.T) {
	h, script, dir := agentHarness(t)
	h.ok("agent", "add", "bad", "--dir", dir, "--", "/bin/sh", script, "fail")
	id := h.add("会失败").ID
	out, _, code := h.run("agent", "run", id, "--agent", "bad")
	if code != ExitError || !strings.Contains(out, "结果：失败") || !strings.Contains(out, "missing dependency") || !strings.Contains(out, "todo agent retry") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if task := h.showTask(id); task.Task.Status == "done" {
		t.Fatal("failed run completed the task")
	}
	var runs struct {
		Runs []agent.Run `json:"runs"`
	}
	h.json(&runs, "agent", "runs")
	rid := runs.Runs[0].ID
	out, _, code = h.run("agent", "retry", rid, "--context", "安装 libfoo 后再试")
	if code != ExitError || !strings.Contains(out, "第 2 次执行") {
		t.Fatalf("retry exit %d:\n%s", code, out)
	}
	var got struct {
		Run agent.Run `json:"run"`
	}
	h.json(&got, "agent", "show", rid)
	if len(got.Run.Attempts) != 2 || !strings.Contains(got.Run.Prompt, "安装 libfoo") {
		t.Fatalf("attempts %+v", got.Run.Attempts)
	}
	if errOut, code := h.fail("agent", "pause", rid); code != ExitError || !strings.Contains(errOut, "invalid_state") && !strings.Contains(errOut, "cannot be paused") {
		t.Fatalf("pause ended run: %d %s", code, errOut)
	}
	if errOut, code := h.fail("agent", "run", id, "--agent", "nobody"); code != ExitNotFound || !strings.Contains(errOut, "未配置") {
		t.Fatalf("unknown agent: %d %s", code, errOut)
	}

	h.ok("agent", "add", "careful", "--confirm", "--dir", dir, "--", "/bin/sh", script, "success")
	if out := h.ok("agent", "run", id, "--agent", "careful"); !strings.Contains(out, "等待确认") {
		t.Fatalf("confirm agent:\n%s", out)
	}
	h.json(&runs, "agent", "runs", "--status", "waiting_confirmation")
	if len(runs.Runs) != 1 {
		t.Fatalf("waiting runs %+v", runs)
	}
	if out := h.ok("agent", "confirm", runs.Runs[0].ID); !strings.Contains(out, "结果：成功") {
		t.Fatalf("confirm:\n%s", out)
	}
	h.ok("agent", "remove", "careful")
	if errOut, _ := h.fail("agent", "add", "root", "--", "sudo", "rm"); !strings.Contains(errOut, "privileges") {
		t.Fatalf("sudo agent: %s", errOut)
	}
}

func (h *harness) showTask(id string) taskOut {
	h.t.Helper()
	var o taskOut
	h.json(&o, "show", id)
	return o
}

// todoProc runs this test binary as the real `todo` command.
func todoProc(t *testing.T, dataDir string, args ...string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"--data-dir", dataDir}, args...)...)
	cmd.Env = append(os.Environ(), "TODO_CLI_E2E_CHILD=1")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func runStatus(t *testing.T, dataDir, id string) agent.Run {
	t.Helper()
	out, code := todoProc(t, dataDir, "--json", "agent", "show", id)
	var got struct {
		Run agent.Run `json:"run"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("show %s exited %d:\n%s", id, code, out)
	}
	return got.Run
}

func waitStatus(t *testing.T, dataDir, id string, want ...string) agent.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := runStatus(t, dataDir, id)
		if slices.Contains(want, r.Status) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %s, want %v", id, r.Status, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAgentDetachedWorkerAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	script, _ := filepath.Abs("../agent/testdata/mock-agent.sh")
	data, work := t.TempDir(), t.TempDir()
	if out, code := todoProc(t, data, "agent", "add", "slow", "--dir", work, "--", "/bin/sh", script, "slow"); code != 0 {
		t.Fatalf("add: %s", out)
	}
	out, code := todoProc(t, data, "--json", "add", "后台任务")
	var task taskOut
	if code != 0 || json.Unmarshal([]byte(out), &task) != nil {
		t.Fatalf("add task: %s", out)
	}
	start := func() (string, int) {
		out, code := todoProc(t, data, "--json", "agent", "run", task.Task.ID, "--agent", "slow", "--detach")
		var res struct {
			Run       agent.Run `json:"run"`
			WorkerPID int       `json:"worker_pid"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.WorkerPID == 0 {
			t.Fatalf("detach exited %d:\n%s", code, out)
		}
		return res.Run.ID, res.WorkerPID
	}

	// Another process cancels the run the background worker executes.
	id, _ := start()
	waitStatus(t, data, id, agent.StatusRunning)
	if out, code := todoProc(t, data, "agent", "pause", id); code != 0 {
		t.Fatalf("pause: %s", out)
	}
	waitStatus(t, data, id, agent.StatusPaused)
	if out, code := todoProc(t, data, "agent", "resume", id); code != 0 {
		t.Fatalf("resume: %s", out)
	}
	waitStatus(t, data, id, agent.StatusRunning)
	if out, code := todoProc(t, data, "agent", "cancel", id); code != 0 {
		t.Fatalf("cancel: %s", out)
	}
	if r := waitStatus(t, data, id, agent.StatusCancelled); r.ErrorKind != agent.KindCancelled {
		t.Fatalf("cancelled run %+v", r)
	}

	// A worker that dies leaves a run whose outcome is unknown.
	id, pid := start()
	waitStatus(t, data, id, agent.StatusRunning)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if r := waitStatus(t, data, id, agent.StatusUnknown); r.ErrorKind != agent.KindOrphaned {
		t.Fatalf("orphaned run %+v", r)
	}
}
