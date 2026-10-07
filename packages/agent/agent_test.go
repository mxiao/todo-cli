package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

func TestSuccessWritesBackFourResultTypes(t *testing.T) {
	f := newFixture(t)
	f.mock("writer", "success")
	task := f.task("写周报")
	r := f.run(task.ID, "writer")

	if r.Status != StatusSucceeded || r.StatusLabel != "成功" || r.Attempt != 1 || r.Error != "" {
		t.Fatalf("run %+v", r)
	}
	if r.StartedAt == nil || r.EndedAt == nil || r.EndedAt.Before(*r.StartedAt) || len(r.Attempts) != 1 || r.Attempts[0].EndedAt == nil {
		t.Fatalf("times %+v %+v", r, r.Attempts)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 || r.Stage != "done" || r.Progress != 100 {
		t.Fatalf("exit/progress %+v", r)
	}
	if got := resultTypes(r.Results); !slices.Equal(got, []string{ResultCommandOutput, ResultCommit, ResultFile, ResultText}) {
		t.Fatalf("result types %v", got)
	}
	for _, res := range r.Results {
		if res.Agent != "writer" || res.RunID != r.ID || res.TaskID != task.ID || res.CreatedAt.IsZero() || res.Version != 1 {
			t.Fatalf("result source %+v", res)
		}
		if res.Key != ResultKey(task.ID, r.ID, res.Type, "") {
			t.Fatalf("idempotency key %q", res.Key)
		}
		switch res.Type {
		case ResultText:
			if res.Summary != "report done" || !strings.Contains(res.Data["text"].(string), "All checks passed") {
				t.Fatalf("text %+v", res)
			}
		case ResultFile:
			if res.Data["path"] != filepath.Join(f.dir, "report.md") || res.Data["exists"] != true || res.Data["sha256"] == nil || res.Summary != "weekly report" {
				t.Fatalf("file %+v", res)
			}
		case ResultCommit:
			if res.Data["repo"] != "/tmp/repo" || res.Data["branch"] != "main" || res.Data["commit"] != "0123456789abcdef0123456789abcdef01234567" {
				t.Fatalf("commit %+v", res)
			}
		case ResultCommandOutput:
			if res.Data["command"] != "make test" || res.Data["exit_code"] != float64(0) || res.Data["stdout"] != "ok" {
				t.Fatalf("command %+v", res)
			}
		}
	}
	// The log has output, commands, progress and results (FR-503).
	if len(f.events(r.ID, EventOutput)) == 0 || len(f.events(r.ID, EventCommand)) != 1 || len(f.events(r.ID, EventProgress)) != 2 ||
		len(f.events(r.ID, EventResult)) != 4 {
		t.Fatalf("log:\n%s", f.log(r.ID))
	}
	if cmd := f.events(r.ID, EventCommand)[0]; cmd.Message != "git commit -m report → 退出码 0" {
		t.Fatalf("command event %+v", cmd)
	}
	// The agent got the protocol input on stdin with the final prompt (FR-506).
	b, err := os.ReadFile(filepath.Join(f.dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("input %s: %v", b, err)
	}
	if in.Protocol != Protocol || in.Task.ID != task.ID || in.Task.Title != "写周报" || in.Prompt != r.Prompt || in.RunID != r.ID ||
		in.Attempt != 1 || in.Workdir != f.dir || len(in.Constraints) < len(BaseConstraints) || !strings.Contains(in.OutputFormat, "JSON Lines") {
		t.Fatalf("input %+v", in)
	}
	for _, want := range []string{"# 任务", "写周报", "描述 写周报", "# 约束", "提权", "# 输出格式", "result_type"} {
		if !strings.Contains(r.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, r.Prompt)
		}
	}
	// The task is in progress, not done: success alone does not complete it.
	got, _ := f.store.Get(task.ID)
	if got.Status != core.StatusInProgress {
		t.Fatalf("task status %s", got.Status)
	}
	h := f.history(task.ID)
	for _, want := range []string{"agent_run@agent/writer", "start@agent/writer", "agent_result@agent/writer", "agent_succeeded@agent/writer"} {
		if !slices.Contains(h, want) {
			t.Fatalf("history %v lacks %s", h, want)
		}
	}
	if tr, _ := f.m.TaskResults(task.ID); len(tr) != 4 {
		t.Fatalf("task results %+v", tr)
	}
}

func TestCompleteOnSuccessOnly(t *testing.T) {
	f := newFixture(t)
	f.mock("ok", "success")
	f.mock("bad", "fail")
	a, b := f.task("a"), f.task("b")
	if r := f.run(a.ID, "ok", func(s *StartRequest) { s.CompleteOnSuccess = true }); r.Status != StatusSucceeded {
		t.Fatal(r.Status)
	}
	if r := f.run(b.ID, "bad", func(s *StartRequest) { s.CompleteOnSuccess = true }); r.Status != StatusFailed {
		t.Fatal(r.Status)
	}
	ga, _ := f.store.Get(a.ID)
	gb, _ := f.store.Get(b.ID)
	if ga.Status != core.StatusDone || gb.Status == core.StatusDone {
		t.Fatalf("a=%s b=%s: a failed run must never complete its task", ga.Status, gb.Status)
	}
}

func TestTaskStatusResultAppliesOnlyOnSuccess(t *testing.T) {
	f := newFixture(t)
	f.mock("jira", "status")
	task := f.task("同步工单")
	r := f.run(task.ID, "jira")
	if r.Status != StatusSucceeded || len(r.Results) != 1 {
		t.Fatalf("run %+v", r)
	}
	res := r.Results[0]
	if res.Type != ResultTaskStatus || res.Data["status"] != "Resolved" || res.Data["url"] != "https://jira.example/T-1" || res.Summary != "jira 状态 Resolved" {
		t.Fatalf("status result %+v", res)
	}
	if got, _ := f.store.Get(task.ID); got.Status != core.StatusDone {
		t.Fatalf("task %s", got.Status)
	}
}

func TestNonZeroExitFails(t *testing.T) {
	f := newFixture(t)
	f.mock("bad", "fail")
	task := f.task("编译")
	r := f.run(task.ID, "bad")
	if r.Status != StatusFailed || r.ErrorKind != KindExitCode || r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("run %+v", r)
	}
	if !strings.Contains(r.Error, "退出码 3") || !strings.Contains(r.Error, "missing dependency libfoo") {
		t.Fatalf("error %q", r.Error)
	}
	ev := f.events(r.ID, EventOutput)
	if len(ev) != 2 || !slices.ContainsFunc(ev, func(e Event) bool { return e.Stream == "stderr" && strings.Contains(e.Message, "boom") }) {
		t.Fatalf("output %+v", ev)
	}
	got, _ := f.store.Get(task.ID)
	if got.Status == core.StatusDone {
		t.Fatal("failed run completed the task")
	}
	if !slices.Contains(f.history(task.ID), "agent_failed@agent/bad") {
		t.Fatalf("history %v", f.history(task.ID))
	}
}

func TestStartFailureAndMissingAgent(t *testing.T) {
	f := newFixture(t)
	if _, err := f.m.PutAgent(Spec{Name: "ghost", Command: []string{"/nonexistent/agent-binary"}, Dir: f.dir}); err != nil {
		t.Fatal(err)
	}
	task := f.task("x")
	r := f.run(task.ID, "ghost")
	if r.Status != StatusFailed || r.ErrorKind != KindStartFailed || !strings.Contains(r.Error, "无法启动") {
		t.Fatalf("run %+v", r)
	}
	if _, err := f.m.Start(context.Background(), StartRequest{TaskID: task.ID, Agent: "nobody"}); !errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "未配置") {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestTimeoutIsUnknownAndNotRetried(t *testing.T) {
	f := newFixture(t)
	f.mock("slow", "slow", func(s *Spec) { s.TimeoutSeconds = 1; s.MaxRetries = 2 })
	task := f.task("慢任务")
	start := time.Now()
	r := f.run(task.ID, "slow")
	if r.Status != StatusUnknown || r.ErrorKind != KindTimeout || r.Attempt != 1 || len(r.Attempts) != 1 {
		t.Fatalf("run %+v", r)
	}
	if !strings.Contains(r.Error, "超时") || time.Since(start) > 5*time.Second {
		t.Fatalf("error %q after %s", r.Error, time.Since(start))
	}
	if got, _ := f.store.Get(task.ID); got.Status == core.StatusDone {
		t.Fatal("timed out run completed the task")
	}
}

func TestCancelKeepsPartialResults(t *testing.T) {
	f := newFixture(t)
	f.mock("loop", "trap")
	task := f.task("长任务")
	r := f.start(StartRequest{TaskID: task.ID, Agent: "loop"})
	done := make(chan error, 1)
	go func() { done <- f.m.Execute(context.Background(), r.ID) }()
	f.waitFor("first result", func() bool { return len(f.events(r.ID, EventResult)) == 1 })
	if _, err := f.m.Cancel(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r = f.get(r.ID)
	if r.Status != StatusCancelled || r.ErrorKind != KindCancelled || len(r.Results) != 1 || r.EndedAt == nil {
		t.Fatalf("run %+v", r)
	}
	if !strings.Contains(f.log(r.ID), "got TERM") {
		t.Fatalf("the agent did not get SIGTERM:\n%s", f.log(r.ID))
	}
	if _, err := f.m.Cancel(r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("cancel twice: %v", err)
	}
	// Cancelling a queued run ends it at once.
	q := f.start(StartRequest{TaskID: task.ID, Agent: "loop"})
	if c, err := f.m.Cancel(q.ID); err != nil || c.Status != StatusCancelled {
		t.Fatalf("queued cancel %+v %v", c, err)
	}
}

func TestPauseAndResume(t *testing.T) {
	f := newFixture(t)
	f.mock("slow", "slow")
	task := f.task("可暂停")
	r := f.start(StartRequest{TaskID: task.ID, Agent: "slow"})
	done := make(chan error, 1)
	go func() { done <- f.m.Execute(context.Background(), r.ID) }()
	f.waitFor("output", func() bool { return len(f.events(r.ID, EventOutput)) >= 3 })
	if _, err := f.m.Pause(r.ID); err != nil {
		t.Fatal(err)
	}
	f.waitFor("paused", func() bool { return f.get(r.ID).Status == StatusPaused })
	time.Sleep(100 * time.Millisecond)
	n := len(f.events(r.ID, EventOutput))
	time.Sleep(300 * time.Millisecond)
	if m := len(f.events(r.ID, EventOutput)); m != n {
		t.Fatalf("output continued while paused: %d → %d", n, m)
	}
	if _, err := f.m.Pause(r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("pause twice: %v", err)
	}
	if _, err := f.m.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	f.waitFor("running", func() bool { return f.get(r.ID).Status == StatusRunning })
	f.waitFor("more output", func() bool { return len(f.events(r.ID, EventOutput)) > n+2 })
	if _, err := f.m.Cancel(r.ID); err != nil {
		t.Fatal(err)
	}
	<-done
	got := f.get(r.ID)
	if got.Status != StatusCancelled || got.DurationMS <= 0 {
		t.Fatalf("run %+v", got)
	}
	log := f.log(r.ID)
	for _, want := range []string{"已暂停", "已继续", "已取消"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %s:\n%s", want, log)
		}
	}
	// A queued run can be held and released before it starts.
	q := f.start(StartRequest{TaskID: task.ID, Agent: "slow"})
	if p, err := f.m.Pause(q.ID); err != nil || p.Status != StatusPaused {
		t.Fatalf("pause queued %+v %v", p, err)
	}
	if p, err := f.m.Resume(q.ID); err != nil || p.Status != StatusQueued {
		t.Fatalf("resume queued %+v %v", p, err)
	}
}

func TestAutomaticRetry(t *testing.T) {
	f := newFixture(t)
	f.mock("flaky", "flaky", func(s *Spec) { s.MaxRetries = 1 })
	task := f.task("偶发失败")
	r := f.run(task.ID, "flaky")
	if r.Status != StatusSucceeded || r.Attempt != 2 || len(r.Attempts) != 2 {
		t.Fatalf("run %+v", r)
	}
	if a := r.Attempts[0]; a.Status != StatusFailed || a.ErrorKind != KindExitCode || a.ExitCode == nil || *a.ExitCode != 1 {
		t.Fatalf("first attempt %+v", a)
	}
	if len(f.events(r.ID)) == 0 || !strings.Contains(f.log(r.ID), "自动重试") {
		t.Fatalf("log:\n%s", f.log(r.ID))
	}
	ev, _ := f.m.Events(r.ID, EventFilter{Attempt: 1, Kinds: []string{EventOutput}})
	if len(ev) != 1 || !strings.Contains(ev[0].Message, "transient failure 1") {
		t.Fatalf("first attempt log %+v", ev)
	}
}

func TestManualRetryKeepsEarlierAttempts(t *testing.T) {
	f := newFixture(t)
	f.mock("bad", "fail")
	task := f.task("需要修参数")
	r := f.run(task.ID, "bad")
	if r.Status != StatusFailed {
		t.Fatal(r.Status)
	}
	if _, err := f.m.Retry(r.ID, RetryRequest{Context: "改用 --fast 参数"}); err != nil {
		t.Fatal(err)
	}
	if got := f.get(r.ID); got.Status != StatusQueued || !strings.Contains(got.Prompt, "改用 --fast 参数") {
		t.Fatalf("queued %+v", got)
	}
	if err := f.m.Execute(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(r.ID)
	if got.Attempt != 2 || len(got.Attempts) != 2 || got.Attempts[0].Status != StatusFailed || got.Attempts[1].Status != StatusFailed {
		t.Fatalf("attempts %+v", got.Attempts)
	}
	for _, a := range []int{1, 2} {
		if ev, _ := f.m.Events(r.ID, EventFilter{Attempt: a, Kinds: []string{EventOutput}}); len(ev) != 2 {
			t.Fatalf("attempt %d log %+v", a, ev)
		}
	}
	var in Input
	b, _ := os.ReadFile(filepath.Join(f.dir, "input.json"))
	if err := json.Unmarshal(b, &in); err != nil || in.Attempt != 2 || !strings.Contains(in.Context.User, "--fast") {
		t.Fatalf("retry input %+v %v", in, err)
	}
	// Succeeded runs cannot be retried.
	f.mock("ok", "success")
	ok := f.run(task.ID, "ok")
	if _, err := f.m.Retry(ok.ID, RetryRequest{}); !errors.Is(err, ErrState) {
		t.Fatalf("retry succeeded run: %v", err)
	}
}

func TestPartialSuccess(t *testing.T) {
	f := newFixture(t)
	f.mock("half", "partial", func(s *Spec) { s.MaxRetries = 3 })
	f.mock("declared", "declared-partial")
	f.mock("invalid", "invalid")
	task := f.task("部分完成")

	r := f.run(task.ID, "half")
	if r.Status != StatusPartial || r.ErrorKind != KindExitCode || len(r.Results) != 1 || r.Attempt != 1 {
		t.Fatalf("exit 2 with a result: %+v", r)
	}
	if !strings.Contains(r.Error, "已写回 1 个结果") || !strings.Contains(r.Error, "no network") {
		t.Fatalf("error %q", r.Error)
	}
	if r := f.run(task.ID, "declared"); r.Status != StatusPartial || r.Error != "1/2 files" {
		t.Fatalf("declared partial: %+v", r)
	}
	r = f.run(task.ID, "invalid")
	if r.Status != StatusPartial || len(r.Results) != 1 || !strings.Contains(r.Error, "1 个结果无效") {
		t.Fatalf("invalid result: %+v", r)
	}
	if !strings.Contains(f.log(r.ID), "结果未写回") {
		t.Fatalf("log:\n%s", f.log(r.ID))
	}
	if got, _ := f.store.Get(task.ID); got.Status == core.StatusDone {
		t.Fatal("partial run completed the task")
	}
}

func TestLogRedaction(t *testing.T) {
	f := newFixture(t)
	f.env["MY_SERVICE_TOKEN"] = "tok-custom-value-778899"
	f.mock("leaky", "secret", func(s *Spec) { s.Env = []string{"MY_SERVICE_TOKEN"} })
	task := f.task("泄露检查 password=topsecret123")
	r := f.run(task.ID, "leaky")
	if r.Status != StatusSucceeded {
		t.Fatalf("run %+v", r)
	}
	everything := f.log(r.ID) + r.Prompt
	for _, res := range r.Results {
		b, _ := json.Marshal(res)
		everything += string(b)
	}
	b, _ := json.Marshal(f.get(r.ID).Input)
	everything += string(b)
	for _, secret := range []string{"sk-abcdefghijklmnop1234567890", "abcdefghijklmnopqrstuvwxyz0123", "hunter2hunter2", "tok-custom-value-778899", "topsecret123"} {
		if strings.Contains(everything, secret) {
			t.Fatalf("secret %q stored:\n%s", secret, everything)
		}
	}
	if !strings.Contains(everything, "[REDACTED]") {
		t.Fatal("nothing redacted")
	}
}

func TestEnvironmentWhitelist(t *testing.T) {
	f := newFixture(t)
	f.env["ALLOWED_VAR"] = "yes"
	f.mock("env", "env", func(s *Spec) { s.Env = []string{"ALLOWED_VAR"} })
	r := f.run(f.task("env").ID, "env")
	out := f.events(r.ID, EventOutput)[0].Message
	for _, want := range []string{"ALLOWED_VAR", "PATH", "TODO_AGENT_RUN_ID", "TODO_AGENT_TASK_ID", "TODO_AGENT_INPUT_FILE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("env lacks %s: %s", want, out)
		}
	}
	for _, banned := range []string{"OTHER_VAR", "TODO_CLI_MODEL_API_KEY"} {
		if strings.Contains(out, banned) {
			t.Fatalf("env leaks %s: %s", banned, out)
		}
	}
	if _, err := f.m.PutAgent(Spec{Name: "keys", Command: []string{"x"}, Env: []string{"TODO_CLI_MODEL_API_KEY"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("model key whitelisted: %v", err)
	}
	if _, err := f.m.PutAgent(Spec{Name: "root", Command: []string{"sudo", "agent"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sudo agent: %v", err)
	}
}

func TestArgumentsAndInputModes(t *testing.T) {
	f := newFixture(t)
	f.mock("args", "args", func(s *Spec) {
		s.Command = append(s.Command, "--task", "{{task_id}}", "--run={{run_id}}")
		s.Input = InputNone
	})
	f.mock("text", "text", func(s *Spec) { s.Output = OutputText; s.Input = InputPromptStdin })
	task := f.task("参数; rm -rf / $(whoami)")
	r := f.run(task.ID, "args")
	out := f.events(r.ID, EventOutput)[0].Message
	if out != "args: args --task "+task.ID+" --run="+r.ID {
		t.Fatalf("args %q", out)
	}
	r = f.run(task.ID, "text")
	b, _ := os.ReadFile(filepath.Join(f.dir, "input.json"))
	if string(b) != r.Prompt {
		t.Fatalf("prompt-stdin got %q", b)
	}
	if len(r.Results) != 1 || r.Results[0].Type != ResultText || r.Results[0].Data["text"] != "Line one of the answer\nLine two" {
		t.Fatalf("text output %+v", r.Results)
	}
}

func TestIdempotencyKeyAndVersions(t *testing.T) {
	f := newFixture(t)
	f.env["DUP_EXIT"] = "5"
	f.mock("dup", "dup", func(s *Spec) { s.Env = []string{"DUP_EXIT"} })
	task := f.task("幂等")
	r := f.run(task.ID, "dup")
	if r.Status != StatusPartial {
		t.Fatalf("run %+v", r)
	}
	if len(r.Results) != 3 {
		t.Fatalf("duplicate commit stored twice: %+v", r.Results)
	}
	if !strings.Contains(f.log(r.ID), "未重复写入") {
		t.Fatalf("log:\n%s", f.log(r.ID))
	}
	if _, err := f.m.Retry(r.ID, RetryRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Execute(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	rs := f.get(r.ID).Results
	byKey := map[string][]Result{}
	for _, x := range rs {
		byKey[x.Key] = append(byKey[x.Key], x)
	}
	commit := byKey[ResultKey(task.ID, r.ID, ResultCommit, "")]
	a := byKey[ResultKey(task.ID, r.ID, ResultText, "a")]
	b := byKey[ResultKey(task.ID, r.ID, ResultText, "b")]
	if len(commit) != 1 || len(b) != 1 {
		t.Fatalf("retry repeated identical results: %+v", rs)
	}
	if len(a) != 2 || a[0].Version != 1 || a[1].Version != 2 || a[0].Data["text"] != "attempt 1" || a[1].Data["text"] != "attempt 2" {
		t.Fatalf("conflicting result must keep both versions: %+v", a)
	}
	// Manual write-back is idempotent too.
	raw := map[string]any{"result_type": "text", "text": "人工补充"}
	first, err := f.m.SaveResult(ResultInput{TaskID: task.ID, Agent: "cli", Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.m.SaveResult(ResultInput{TaskID: task.ID, Agent: "cli", Raw: raw})
	if err != nil || !again.Duplicate || again.ID != first.ID || first.RunID != ManualRun {
		t.Fatalf("manual %+v %+v %v", first, again, err)
	}
}

func TestResultValidation(t *testing.T) {
	cases := []map[string]any{
		{"result_type": "commit", "repo": "r", "branch": "b", "commit": "not-a-hash"},
		{"result_type": "commit", "repo": "r", "commit": "abcdef1"},
		{"result_type": "file"},
		{"result_type": "command_output", "argv": []any{"ls"}},
		{"result_type": "task_status"},
		{"result_type": "video"},
		{"result_type": "text", "text": "x", "key": "bad key with spaces"},
	}
	for _, c := range cases {
		if _, _, _, _, err := NormalizeResult(c, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v accepted: %v", c, err)
		}
	}
	_, _, _, data, err := NormalizeResult(map[string]any{"result_type": "file", "path": "/etc/hosts"}, "/tmp/work")
	if err != nil || data["outside_workdir"] != true {
		t.Fatalf("file outside the working directory %v %v", data, err)
	}
	if _, _, _, data, _ = NormalizeResult(map[string]any{"result_type": "file", "path": "out/a.md"}, "/tmp/work"); data["outside_workdir"] != nil ||
		data["path"] != "/tmp/work/out/a.md" {
		t.Fatalf("relative file %v", data)
	}
	typ, _, sum, data, err := NormalizeResult(map[string]any{"result_type": "data", "data": map[string]any{"n": 1.0}}, "")
	if err != nil || typ != ResultData || data["data"] == nil || !strings.Contains(sum, `"n":1`) {
		t.Fatalf("data %s %s %v %v", typ, sum, data, err)
	}
}

func TestWaitingConfirmation(t *testing.T) {
	f := newFixture(t)
	f.mock("careful", "success", func(s *Spec) { s.Confirm = true })
	task := f.task("需确认")
	r := f.start(StartRequest{TaskID: task.ID, Agent: "careful"})
	if r.Status != StatusWaitingConfirmation || r.StatusLabel != "等待确认" {
		t.Fatalf("run %+v", r)
	}
	if err := f.m.Execute(context.Background(), r.ID); !errors.Is(err, ErrState) {
		t.Fatalf("executed without confirmation: %v", err)
	}
	if _, err := f.m.Confirm(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Execute(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.get(r.ID); got.Status != StatusSucceeded {
		t.Fatalf("confirmed run %+v", got)
	}
	r2 := f.start(StartRequest{TaskID: task.ID, Agent: "careful"})
	if got, err := f.m.Reject(r2.ID); err != nil || got.Status != StatusCancelled || got.ErrorKind != KindRejected {
		t.Fatalf("reject %+v %v", got, err)
	}
	if r3 := f.start(StartRequest{TaskID: task.ID, Agent: "careful", Confirmed: true}); r3.Status != StatusQueued {
		t.Fatalf("pre-confirmed %+v", r3)
	}
}

func TestRecoverOrphanedRun(t *testing.T) {
	f := newFixture(t)
	f.mock("ok", "success")
	task := f.task("崩溃恢复")
	r := f.start(StartRequest{TaskID: task.ID, Agent: "ok"})
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	// The run was claimed by a process that has exited since.
	if _, err := f.store.DB().Exec(`UPDATE agent_runs SET status = 'running', active = 1, attempt = 1, owner_pid = ? WHERE id = ?`,
		dead.Process.Pid, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO agent_attempts(run_id, attempt, status, started_at) VALUES (?, 1, 'running', ?)`, r.ID, fmtTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Recover(); err != nil {
		t.Fatal(err)
	}
	got := f.get(r.ID)
	if got.Status != StatusUnknown || got.ErrorKind != KindOrphaned || got.Attempts[0].Status != StatusUnknown {
		t.Fatalf("orphan %+v", got)
	}
	if retried, err := f.m.Retry(r.ID, RetryRequest{}); err != nil || retried.Status != StatusQueued {
		t.Fatalf("manual retry of unknown run %+v %v", retried, err)
	}
}

func TestConcurrencyLimitQueues(t *testing.T) {
	f := newFixture(t)
	f.m.opts.MaxConcurrent = 1
	f.mock("loop", "trap")
	f.mock("ok", "success")
	a := f.start(StartRequest{TaskID: f.task("a").ID, Agent: "loop"})
	b := f.start(StartRequest{TaskID: f.task("b").ID, Agent: "ok"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = f.m.Execute(context.Background(), a.ID) }()
	f.waitFor("a running", func() bool { return f.get(a.ID).Status == StatusRunning })
	go func() { defer wg.Done(); _ = f.m.Execute(context.Background(), b.ID) }()
	time.Sleep(200 * time.Millisecond)
	if got := f.get(b.ID); got.Status != StatusQueued {
		t.Fatalf("b should wait for a free slot: %s", got.Status)
	}
	if _, err := f.m.Cancel(a.ID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := f.get(b.ID); got.Status != StatusSucceeded {
		t.Fatalf("b %+v", got)
	}
}

func TestInterruptedExecutorCancelsRun(t *testing.T) {
	f := newFixture(t)
	f.mock("loop", "trap")
	r := f.start(StartRequest{TaskID: f.task("x").ID, Agent: "loop"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.m.Execute(ctx, r.ID) }()
	f.waitFor("running", func() bool { return len(f.events(r.ID, EventResult)) == 1 })
	cancel()
	<-done
	if got := f.get(r.ID); got.Status != StatusCancelled || got.ErrorKind != KindInterrupted {
		t.Fatalf("run %+v", got)
	}
}
