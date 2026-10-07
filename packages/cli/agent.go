package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/agent"
	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
)

// agents opens the agent manager (with the model service as launcher).
func (a *app) agents() (*agent.Manager, error) {
	if a.agentMgr != nil {
		return a.agentMgr, nil
	}
	if _, err := a.llm(); err != nil {
		return nil, err
	}
	if a.agentMgr == nil {
		if a.agentErr != nil {
			return nil, a.agentErr
		}
		return nil, errors.New("agent runs are not available")
	}
	return a.agentMgr, nil
}

func cmdAgent(a *app, args []string) error {
	return a.runGroup("agent", []subcommand{
		{"list", "", "List the configured agents and adapters", agentList},
		{"add", "<name> [--adapter cli|http|llm|…] [flags] [-- <program> [args]]", "Add or replace an agent (generic command line, HTTP API, prompt agent or a registered adapter)", agentAdd},
		{"remove", "<name>", "Remove an agent definition (its runs and results stay)", agentRemove},
		{"run", "<task> [--agent NAME|auto] [--context T] [--constraint C] [--format F] [--template T --var k=v] [--complete] [--detach] [--dry-run]", "Start an agent on a task and follow it (auto: the model chooses)", agentRun},
		{"prompt", "<run|task> [--agent NAME|auto] [--context T]", "Show the final prompt of a run, or preview it for a task", agentPrompt},
		{"runs", "[--task ID] [--status s,…] [--limit N]", "List agent runs", agentRuns},
		{"show", "<run>", "Show a run: status, times, duration, attempts and results", agentShow},
		{"logs", "<run> [--follow] [--attempt N] [--kind output,command,…]", "Show a run's full log (output, commands, progress, errors)", agentLogs},
		{"pause", "<run>", "Pause a run", agentControl("pause")},
		{"resume", "<run> [--detach]", "Continue a paused run", agentControl("resume")},
		{"cancel", "<run>", "Cancel a run (results written so far stay)", agentControl("cancel")},
		{"retry", "<run> [--context T] [--detach]", "Run again; earlier attempts and logs are kept", agentControl("retry")},
		{"confirm", "<run> [--detach]", "Let a run that waits for confirmation start", agentControl("confirm")},
		{"reject", "<run>", "Refuse a run that waits for confirmation", agentControl("reject")},
		{"results", "<task>", "List the results written back for a task, with source agent and time", agentResults},
		{"writeback", "<task> <json|-> [--run R] [--source S]", "Write a result back manually ({\"result_type\": \"text|file|commit|command_output|data|task_status\", …})", agentWriteback},
		{"worker", "<run>", "Execute a run (used by --detach)", agentWorker},
	}, "list", args)
}

// ---- agent definitions ----

func agentList(a *app, args []string) error {
	if _, err := a.subFlags("agent", "list", "", nil, args); err != nil {
		return err
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	list, err := m.Agents()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"agents": list, "adapters": agent.Adapters()})
	}
	if len(list) == 0 {
		a.printf("没有配置智能体。示例：todo agent add coder --input prompt-stdin --output text -- claude -p\n")
	}
	for _, s := range list {
		target := strings.Join(s.Command, " ")
		switch s.AdapterName() {
		case "http":
			target = s.Method + " " + llm.RedactURL(s.URL)
		case "llm":
			target = "模型配置 " + orNone(s.Profile)
		}
		a.printf("%-12s %-5s %s", s.Name, s.AdapterName(), target)
		if s.Description != "" {
			a.printf("  — %s", s.Description)
		}
		var opts []string
		if s.Confirm {
			opts = append(opts, "启动前确认")
		}
		if s.MaxRetries > 0 {
			opts = append(opts, fmt.Sprintf("自动重试 %d 次", s.MaxRetries))
		}
		if s.TimeoutSeconds > 0 {
			opts = append(opts, "超时 "+(time.Duration(s.TimeoutSeconds)*time.Second).String())
		}
		if len(opts) > 0 {
			a.printf("  [%s]", strings.Join(opts, "，"))
		}
		a.printf("\n")
	}
	a.printf("可用适配器：%s\n", strings.Join(agent.Adapters(), ", "))
	return nil
}

func agentAdd(a *app, args []string) error {
	var adapter, desc, dir, timeout, input, output, url, method, profile *string
	var maxRetries, retryDelay *int
	var confirm *bool
	var env, codes, constraints, headers, options *stringList
	pos, err := a.subFlags("agent", "add", "<name> [flags] [-- <program> [args]]", func(f *fset) {
		adapter = f.String("adapter", "cli", "cli, http, llm or a registered adapter")
		desc = f.String("desc", "", "what the agent is good at (used for automatic selection)")
		dir = f.String("dir", "", "working directory")
		env = f.List("env", "environment variables passed through (the rest is withheld)")
		timeout = f.String("timeout", "", "attempt timeout, e.g. 90s, 10m (default 30m)")
		codes = f.List("success-codes", "exit codes meaning success (default 0)")
		input = f.String("input", "", "json-stdin (default), json-arg, prompt-stdin, prompt-arg, none")
		output = f.String("output", "", "jsonl (default), json, text")
		maxRetries = f.Int("max-retries", 0, "automatic retries after a failure (0-5)")
		retryDelay = f.Int("retry-delay", 0, "seconds before an automatic retry")
		confirm = f.Bool("confirm", false, "every start waits for confirmation")
		constraints = f.List("constraint", "constraint added to every prompt of this agent")
		url = f.String("url", "", "http adapter: endpoint")
		method = f.String("method", "", "http adapter: POST (default), PUT or PATCH")
		headers = f.List("header-env", "http adapter: Header=ENV_VAR (the value is read from the environment)")
		profile = f.String("profile", "", "llm adapter: model profile")
		options = f.List("option", "settings of a registered adapter: key=value")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usagef("usage: todo agent add <name> [flags] [-- <program> [args]]")
	}
	sp := agent.Spec{Name: pos[0], Command: pos[1:], Adapter: *adapter, Description: *desc, Dir: *dir, Env: *env,
		Input: *input, Output: *output, MaxRetries: *maxRetries, RetryDelaySeconds: *retryDelay, Confirm: *confirm,
		Constraints: *constraints, URL: *url, Method: *method, Profile: *profile}
	if sp.Adapter == "cli" {
		sp.Adapter = ""
	}
	if *timeout != "" {
		d, err := time.ParseDuration(*timeout)
		if err != nil {
			n, nerr := strconv.Atoi(*timeout)
			if nerr != nil {
				return usagef("--timeout %q: want a duration such as 90s or 10m", *timeout)
			}
			d = time.Duration(n) * time.Second
		}
		if d < time.Second {
			return usagef("--timeout must be at least 1s")
		}
		sp.TimeoutSeconds = int(d / time.Second)
	}
	for _, c := range *codes {
		n, err := strconv.Atoi(c)
		if err != nil {
			return usagef("--success-codes: %q is not a number", c)
		}
		sp.SuccessCodes = append(sp.SuccessCodes, n)
	}
	for _, h := range *headers {
		k, v, ok := strings.Cut(h, "=")
		if !ok {
			return usagef("--header-env %q: want Header=ENV_VAR", h)
		}
		if sp.HeaderEnv == nil {
			sp.HeaderEnv = map[string]string{}
		}
		sp.HeaderEnv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	for _, o := range *options {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			return usagef("--option %q: want key=value", o)
		}
		if sp.Options == nil {
			sp.Options = map[string]string{}
		}
		sp.Options[strings.TrimSpace(k)] = v
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	sp, err = m.PutAgent(sp)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"agent": sp})
	}
	a.printf("已保存智能体 %s（%s 适配器）\n", sp.Name, sp.AdapterName())
	return nil
}

func agentRemove(a *app, args []string) error {
	pos, err := a.subFlags("agent", "remove", "<name>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent remove <name>")
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	if err := m.RemoveAgent(pos[0]); err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"removed": pos[0]})
	}
	a.printf("已删除智能体 %s\n", pos[0])
	return nil
}

// ---- runs ----

type startFlags struct {
	agentName, context, format, template *string
	constraints, vars                    *stringList
	complete                             *bool
	maxRetries                           *int
}

func (sf *startFlags) register(f *fset) {
	sf.agentName = f.String("agent", "auto", "agent name, or auto to let the model choose")
	sf.context = f.String("context", "", "extra context for the agent")
	sf.format = f.String("format", "", "extra output requirements")
	sf.template = f.String("template", "", "prompt template whose rendered text becomes the instructions")
	sf.constraints = f.List("constraint", "extra constraint")
	sf.vars = f.List("var", "template variable name=value")
	sf.complete = f.Bool("complete", false, "mark the task done when the run succeeds")
	sf.maxRetries = f.Int("max-retries", -1, "automatic retries after a failure (default: the agent's setting)")
}

func (sf *startFlags) request(a *app, taskID string) (agent.StartRequest, error) {
	req := agent.StartRequest{TaskID: taskID, Agent: *sf.agentName, Context: *sf.context, OutputFormat: *sf.format,
		Template: *sf.template, Constraints: *sf.constraints, CompleteOnSuccess: *sf.complete, Initiator: a.origin()}
	if *sf.maxRetries >= 0 {
		req.MaxRetries = sf.maxRetries
	}
	if len(*sf.vars) > 0 {
		req.Vars = map[string]string{}
		for _, v := range *sf.vars {
			k, val, ok := strings.Cut(v, "=")
			if !ok {
				return req, usagef("--var %q: want name=value", v)
			}
			req.Vars[strings.TrimSpace(k)] = val
		}
	}
	return req, nil
}

func (a *app) origin() string {
	if a.actor != "" {
		return a.actor
	}
	return "cli"
}

func agentRun(a *app, args []string) error {
	var sf startFlags
	var detach, dry, quiet *bool
	pos, err := a.subFlags("agent", "run", "<task> [flags]", func(f *fset) {
		sf.register(f)
		detach = f.Bool("detach", false, "run in the background and return at once")
		dry = f.Bool("dry-run", false, "only show the agent and the final prompt")
		quiet = f.Bool("quiet", false, "do not stream the log")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent run <task> [--agent NAME|auto] [flags]")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	req, err := sf.request(a, id)
	if err != nil {
		return err
	}
	req.DryRun = *dry
	ctx, cancel := a.ctx()
	defer cancel()
	r, err := m.Start(ctx, req)
	if err != nil {
		return err
	}
	if *dry {
		if a.json {
			return a.emitJSON(map[string]any{"run": r})
		}
		a.printf("智能体：%s（%s 适配器，%s）\n\n%s", r.Agent, r.Adapter, selectionText(r.Selection), r.Prompt)
		return nil
	}
	if !a.json {
		a.printf("运行 %s：智能体 %s（%s），%s\n", r.ID, r.Agent, selectionText(r.Selection), r.StatusLabel)
	}
	return a.execute(ctx, m, r, *detach, *quiet)
}

// execute runs a queued run in the foreground (streaming its log) or
// hands it to a background worker process.
func (a *app) execute(ctx context.Context, m *agent.Manager, r *agent.Run, detach, quiet bool) error {
	if r.Status == agent.StatusWaitingConfirmation {
		if a.json {
			return a.emitJSON(map[string]any{"run": r})
		}
		a.printf("等待确认：todo agent confirm %s（或 todo agent reject %s）\n", r.ID, r.ID)
		return nil
	}
	if detach {
		pid, err := a.spawnWorker(r.ID)
		if err != nil {
			return err
		}
		if a.json {
			return a.emitJSON(map[string]any{"run": r, "worker_pid": pid})
		}
		a.printf("已在后台运行（进程 %d）。查看：todo agent logs %s --follow\n", pid, r.ID)
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- m.ExecuteIfIdle(ctx, r.ID) }()
	var last int64
	stream := !a.json && !quiet
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for running := true; running; {
		select {
		case err := <-done:
			running = false
			if err != nil && !errors.Is(err, context.Canceled) {
				if !errors.Is(err, agent.ErrState) {
					return err
				}
				if !a.json {
					a.printf("%v\n", err)
				}
			}
		case <-tick.C:
		}
		if stream {
			last = a.printEvents(m, r.ID, last)
		}
	}
	got, err := m.Get(r.ID)
	if err != nil {
		return err
	}
	if a.json {
		err = a.emitJSON(map[string]any{"run": got})
	} else {
		a.printRunSummary(got)
	}
	if err == nil && agent.Terminal(got.Status) && got.Status != agent.StatusSucceeded {
		// Scripts see a run that did not succeed as a failed command.
		return statusFailed{fmt.Errorf("agent run %s: %s", got.ID, got.Status)}
	}
	return err
}

func (a *app) printEvents(m *agent.Manager, id string, after int64) int64 {
	evs, err := m.Events(id, agent.EventFilter{After: after})
	if err != nil {
		return after
	}
	for _, e := range evs {
		a.printf("%s\n", eventLine(e))
		after = e.ID
	}
	return after
}

func eventLine(e agent.Event) string {
	mark := "·"
	switch e.Kind {
	case agent.EventState:
		mark = "●"
	case agent.EventOutput:
		mark = "│"
		if e.Stream == "stderr" {
			mark = "!"
		}
	case agent.EventProgress:
		mark = "▸"
	case agent.EventCommand:
		mark = "$"
	case agent.EventResult:
		mark = "✔"
	case agent.EventError:
		mark = "✖"
	}
	return fmt.Sprintf("%s #%d %s %s", e.At.Local().Format("15:04:05"), e.Attempt, mark, e.Message)
}

func selectionText(s agent.Selection) string {
	switch s.By {
	case "llm":
		return "大模型选择：" + s.Reason
	case "routing":
		t := "路由规则：" + s.Reason
		if s.Degraded {
			t += "（大模型不可用）"
		}
		return t
	case "only":
		return "唯一的智能体"
	}
	return "用户指定"
}

func durationText(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", ms)
	}
	return d.Round(100 * time.Millisecond).String()
}

func (a *app) printRunSummary(r *agent.Run) {
	a.printf("结果：%s，耗时 %s，第 %d 次执行，写回 %d 个结果", r.StatusLabel, durationText(r.DurationMS), r.Attempt, len(r.Results))
	if r.Error != "" {
		a.printf("\n原因：%s", r.Error)
	}
	a.printf("\n")
	switch r.Status {
	case agent.StatusFailed, agent.StatusPartial, agent.StatusUnknown, agent.StatusCancelled:
		a.printf("可重试：todo agent retry %s [--context 补充说明]\n", r.ID)
	}
}

func agentPrompt(a *app, args []string) error {
	var sf startFlags
	pos, err := a.subFlags("agent", "prompt", "<run|task>", sf.register, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent prompt <run|task> [--agent NAME]")
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	r, err := m.Get(pos[0])
	if errors.Is(err, core.ErrNotFound) {
		var id string
		if id, err = a.resolveOne(pos[0]); err != nil {
			return err
		}
		req, rerr := sf.request(a, id)
		if rerr != nil {
			return rerr
		}
		req.DryRun = true
		ctx, cancel := a.ctx()
		defer cancel()
		r, err = m.Start(ctx, req)
	}
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"run_id": r.ID, "agent": r.Agent, "prompt": r.Prompt, "input": r.Input})
	}
	a.printf("%s", r.Prompt)
	return nil
}

func agentRuns(a *app, args []string) error {
	var task *string
	var statuses *stringList
	var limit *int
	if _, err := a.subFlags("agent", "runs", "", func(f *fset) {
		task = f.String("task", "", "only runs of this task")
		statuses = f.List("status", "only these states")
		limit = f.Int("limit", 20, "how many runs")
	}, args); err != nil {
		return err
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	filter := agent.ListFilter{Statuses: *statuses, Limit: *limit}
	if *task != "" {
		if filter.TaskID, err = a.resolveOne(*task); err != nil {
			return err
		}
	}
	runs, err := m.List(filter)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"runs": runs})
	}
	if len(runs) == 0 {
		a.printf("没有智能体运行记录\n")
	}
	store, _ := a.open()
	for _, r := range runs {
		title := r.TaskID
		if t, err := store.Get(r.TaskID); err == nil {
			title = t.Title
		}
		a.printf("%s  %s  %-8s %-10s %-8s 第%d次 %7s  %s\n", r.ID, a.localTime(r.CreatedAt), r.StatusLabel, r.Agent, r.Initiator,
			r.Attempt, durationText(r.DurationMS), title)
	}
	return nil
}

func agentShow(a *app, args []string) error {
	pos, err := a.subFlags("agent", "show", "<run>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent show <run>")
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	r, err := m.Get(pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"run": r})
	}
	store, _ := a.open()
	title := ""
	if t, err := store.Get(r.TaskID); err == nil {
		title = t.Title
	}
	a.printf("运行 %s  %s\n", r.ID, r.StatusLabel)
	a.printf("任务：      %s (%s)\n", title, r.TaskID)
	a.printf("智能体：    %s（%s 适配器；%s）\n", r.Agent, r.Adapter, selectionText(r.Selection))
	a.printf("发起：      %s", r.Initiator)
	if r.SessionID != "" {
		a.printf("（模型会话 %s #%d）", r.SessionID, r.Item)
	}
	a.printf("\n")
	if r.StartedAt != nil {
		a.printf("开始：      %s\n", a.localTime(*r.StartedAt))
	}
	if r.EndedAt != nil {
		a.printf("结束：      %s\n", a.localTime(*r.EndedAt))
	}
	a.printf("耗时：      %s（第 %d 次执行）\n", durationText(r.DurationMS), r.Attempt)
	if r.Stage != "" {
		a.printf("阶段：      %s %.0f%%\n", r.Stage, r.Progress)
	}
	if r.Error != "" {
		a.printf("原因：      %s\n", r.Error)
	}
	if r.NextAt != nil {
		a.printf("下次重试：  %s\n", a.localTime(*r.NextAt))
	}
	if len(r.Attempts) > 0 {
		a.printf("执行记录：\n")
		for _, at := range r.Attempts {
			exit := ""
			if at.ExitCode != nil {
				exit = fmt.Sprintf(" 退出码 %d", *at.ExitCode)
			}
			a.printf("  #%d %-6s %s %s%s 结果 %d %s\n", at.Attempt, agent.StatusLabel(at.Status), a.localTime(at.StartedAt),
				durationText(at.DurationMS), exit, at.Results, at.Error)
		}
	}
	if len(r.Results) > 0 {
		a.printf("结果：\n")
		for _, x := range r.Results {
			a.printAgentResult(x)
		}
	}
	a.printf("提示词：todo agent prompt %s；日志：todo agent logs %s\n", r.ID, r.ID)
	return nil
}

func (a *app) printAgentResult(x agent.Result) {
	a.printf("  [%s] %s — 来源 %s，%s，v%d\n", x.Type, x.Summary, x.Agent, a.localTime(x.CreatedAt), x.Version)
	switch x.Type {
	case agent.ResultFile:
		a.printf("        %v\n", x.Data["path"])
	case agent.ResultCommit:
		a.printf("        %v %v %v\n", x.Data["repo"], x.Data["branch"], x.Data["commit"])
	case agent.ResultTaskStatus:
		if u, ok := x.Data["url"]; ok {
			a.printf("        %v\n", u)
		}
	}
}

func agentLogs(a *app, args []string) error {
	var follow *bool
	var attempt *int
	var kinds *stringList
	pos, err := a.subFlags("agent", "logs", "<run>", func(f *fset) {
		follow = f.Bool("follow", false, "keep printing until the run ends")
		attempt = f.Int("attempt", 0, "only this attempt")
		kinds = f.List("kind", "only these kinds: state, system, output, log, progress, command, error, result")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent logs <run> [--follow]")
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	id, err := m.ResolveRun(pos[0])
	if err != nil {
		return err
	}
	filter := agent.EventFilter{Attempt: *attempt, Kinds: *kinds}
	if a.json {
		evs, err := m.Events(id, filter)
		if err != nil {
			return err
		}
		return a.emitJSON(map[string]any{"events": evs})
	}
	ctx, cancel := a.ctx()
	defer cancel()
	for {
		evs, err := m.Events(id, filter)
		if err != nil {
			return err
		}
		for _, e := range evs {
			a.printf("%s\n", eventLine(e))
			filter.After = e.ID
		}
		r, err := m.Get(id)
		if err != nil {
			return err
		}
		if !*follow || agent.Terminal(r.Status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func agentControl(verb string) func(a *app, args []string) error {
	return func(a *app, args []string) error {
		var detach *bool
		var extra *string
		pos, err := a.subFlags("agent", verb, "<run>", func(f *fset) {
			if verb == "retry" || verb == "resume" || verb == "confirm" {
				detach = f.Bool("detach", false, "run in the background")
			}
			if verb == "retry" {
				extra = f.String("context", "", "extra context added to the prompt (e.g. corrected parameters)")
			}
		}, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("usage: todo agent %s <run>", verb)
		}
		m, err := a.agents()
		if err != nil {
			return err
		}
		var r *agent.Run
		switch verb {
		case "pause":
			r, err = m.Pause(pos[0])
		case "resume":
			r, err = m.Resume(pos[0])
		case "cancel":
			r, err = m.Cancel(pos[0])
		case "retry":
			r, err = m.Retry(pos[0], agent.RetryRequest{Context: *extra})
		case "confirm":
			r, err = m.Confirm(pos[0])
		case "reject":
			r, err = m.Reject(pos[0])
		}
		if err != nil {
			return err
		}
		if !a.json {
			a.printf("运行 %s：%s\n", r.ID, r.StatusLabel)
		}
		// A run that is queued again needs an executor.
		if r.Status == agent.StatusQueued && detach != nil {
			ctx, cancel := a.ctx()
			defer cancel()
			return a.execute(ctx, m, r, *detach, false)
		}
		if a.json {
			return a.emitJSON(map[string]any{"run": r})
		}
		return nil
	}
}

func agentResults(a *app, args []string) error {
	pos, err := a.subFlags("agent", "results", "<task>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent results <task>")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	rs, err := m.TaskResults(id)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"results": rs})
	}
	if len(rs) == 0 {
		a.printf("该任务还没有写回的结果\n")
	}
	for _, x := range rs {
		a.printAgentResult(x)
	}
	return nil
}

func agentWriteback(a *app, args []string) error {
	var run, source *string
	pos, err := a.subFlags("agent", "writeback", "<task> <json|->", func(f *fset) {
		run = f.String("run", "", "the run the result belongs to (default: manual)")
		source = f.String("source", "", "source shown with the result (default: the actor)")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("usage: todo agent writeback <task> '{\"result_type\":\"text\",\"text\":\"…\"}'")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	text := pos[1]
	if text == "-" {
		b, err := io.ReadAll(a.env.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return fmt.Errorf("%w: the result must be a JSON object: %v", core.ErrInvalid, err)
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	runID := *run
	if runID != "" {
		if runID, err = m.ResolveRun(runID); err != nil {
			return err
		}
	}
	src := *source
	if src == "" {
		src = a.origin()
	}
	wd, _ := os.Getwd()
	r, err := m.SaveResult(agent.ResultInput{TaskID: id, RunID: runID, Agent: src, Raw: raw, Dir: wd})
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"result": r})
	}
	switch {
	case r.Duplicate:
		a.printf("结果已存在（v%d），未重复写入\n", r.Version)
	default:
		a.printf("已写回结果 v%d：[%s] %s\n", r.Version, r.Type, r.Summary)
	}
	return nil
}

func agentWorker(a *app, args []string) error {
	pos, err := a.subFlags("agent", "worker", "<run>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo agent worker <run>")
	}
	m, err := a.agents()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	return m.Execute(ctx, pos[0])
}

// spawnWorker starts `todo agent worker <run>` detached from this process.
func (a *app) spawnWorker(runID string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	dir, err := a.resolveDataDir()
	if err != nil {
		return 0, err
	}
	args := []string{"--data-dir", dir}
	if a.actor != "" {
		args = append(args, "--actor", a.actor)
	}
	cmd := exec.Command(exe, append(args, "agent", "worker", runID)...)
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}
