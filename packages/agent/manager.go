package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

// EnvConcurrency limits how many attempts run at the same time.
const EnvConcurrency = "TODO_CLI_AGENT_CONCURRENCY"

// Options configures a Manager.
type Options struct {
	// LLM provides the agent settings, redaction, the model for automatic
	// agent selection and the llm adapter. Required.
	LLM *llm.Service
	// Prompts renders prompt templates for runs; nil disables templates.
	Prompts *prompt.Library
	// Origin is the initiator of runs that do not name one (cli, web).
	Origin string
	// Getenv reads whitelisted variables and header tokens; nil = os.Getenv.
	Getenv func(string) string
	// PollInterval is how often control requests are picked up (100ms).
	PollInterval time.Duration
	// KillGrace is how long a cancelled agent gets between SIGTERM and
	// SIGKILL (3s).
	KillGrace time.Duration
	// MaxConcurrent bounds running attempts across processes (default 4,
	// or $TODO_CLI_AGENT_CONCURRENCY); more runs wait in the queue.
	MaxConcurrent int
	// Async makes LaunchAgent return at once and run in the background
	// (the web server); otherwise it waits for the run (the CLI).
	Async bool
	// Context bounds background runs; nil = context.Background().
	Context context.Context
}

// Manager starts, executes and controls agent runs over one task store.
type Manager struct {
	store *core.Store
	svc   *llm.Service
	opts  Options
	pid   int

	wg sync.WaitGroup
}

// executing holds the runs executed by this process (by any manager).
var (
	execMu    sync.Mutex
	executing = map[string]bool{}
)

func isExecuting(id string) bool {
	execMu.Lock()
	defer execMu.Unlock()
	return executing[id]
}

var (
	errCancelled = errors.New("cancelled by the user")
	errTimeout   = errors.New("timed out")
)

// Open prepares the agent tables and marks runs whose executing process
// died as "unknown".
func Open(store *core.Store, opts Options) (*Manager, error) {
	if opts.LLM == nil {
		return nil, fmt.Errorf("%w: agent.Open needs the model service", ErrInvalid)
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Origin == "" {
		opts.Origin = "cli"
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 100 * time.Millisecond
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 3 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		if n, err := strconv.Atoi(opts.Getenv(EnvConcurrency)); err == nil && n > 0 {
			opts.MaxConcurrent = n
		} else {
			opts.MaxConcurrent = 4
		}
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if err := store.MigrateModule("agent", moduleMigrations); err != nil {
		return nil, err
	}
	m := &Manager{store: store, svc: opts.LLM, opts: opts, pid: os.Getpid()}
	if err := m.Recover(); err != nil {
		return nil, err
	}
	return m, nil
}

// Wait blocks until every background run started by this manager ended.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) redact(s string, secrets []string) string {
	return llm.Redact(m.svc.Redact(s), secrets...)
}

func (m *Manager) redactMap(v map[string]any, secrets []string) map[string]any {
	out, _ := m.redactAny(v, secrets).(map[string]any)
	return out
}

func (m *Manager) redactAny(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		return m.redact(x, secrets)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = m.redactAny(e, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = m.redactAny(e, secrets)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = m.redact(e, secrets)
		}
		return out
	}
	return v
}

// ---- agent definitions ----

// Agents lists the configured agents with defaults filled in.
func (m *Manager) Agents() ([]Spec, error) {
	st, err := m.svc.Settings()
	if err != nil {
		return nil, err
	}
	out := make([]Spec, 0, len(st.Agents))
	for _, a := range st.Agents {
		_ = Normalize(&a, m.svc.KeyEnvNames())
		out = append(out, a)
	}
	return out, nil
}

// Agent returns a configured agent; an unknown name or an adapter that is
// not available is reported as such, never as a success (NFR-035).
func (m *Manager) Agent(name string) (Spec, error) {
	st, err := m.svc.Settings()
	if err != nil {
		return Spec{}, err
	}
	var names []string
	for _, a := range st.Agents {
		if strings.EqualFold(a.Name, strings.TrimSpace(name)) {
			if err := Normalize(&a, m.svc.KeyEnvNames()); err != nil {
				return Spec{}, fmt.Errorf("智能体 %s 不可用（%s）：%w", a.Name, KindAdapterUnavailable, err)
			}
			return a, nil
		}
		names = append(names, a.Name)
	}
	have := "尚未配置任何智能体（todo agent add）"
	if len(names) > 0 {
		have = "已配置：" + strings.Join(names, ", ")
	}
	return Spec{}, fmt.Errorf("%w: 智能体 %q 未配置，能力不可用；%s", ErrNotFound, name, have)
}

// PutAgent adds or replaces an agent definition.
func (m *Manager) PutAgent(sp Spec) (Spec, error) {
	if err := Normalize(&sp, m.svc.KeyEnvNames()); err != nil {
		return sp, err
	}
	_, err := m.svc.UpdateSettings(func(s *llm.Settings) error {
		s.Agents = slices.DeleteFunc(s.Agents, func(a llm.AgentProfile) bool { return strings.EqualFold(a.Name, sp.Name) })
		s.Agents = append(s.Agents, sp)
		return nil
	})
	return sp, err
}

// RemoveAgent deletes an agent definition; its runs and results stay.
func (m *Manager) RemoveAgent(name string) error {
	_, err := m.svc.UpdateSettings(func(s *llm.Settings) error {
		n := len(s.Agents)
		s.Agents = slices.DeleteFunc(s.Agents, func(a llm.AgentProfile) bool { return strings.EqualFold(a.Name, name) })
		if len(s.Agents) == n {
			return fmt.Errorf("%w: no agent %q", ErrNotFound, name)
		}
		return nil
	})
	return err
}

// workdir is the agent's working directory: its own, the model command
// directory, or the home directory.
func (m *Manager) workdir(sp Spec) (string, error) {
	dir := sp.Dir
	if dir == "" {
		if st, err := m.svc.Settings(); err == nil {
			dir = st.CommandDir
		}
	}
	home, _ := os.UserHomeDir()
	if dir == "" {
		dir = home
	}
	if dir == "~" {
		dir = home
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, dir[2:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// ---- starting runs ----

// StartRequest starts an agent on a task (FR-501).
type StartRequest struct {
	TaskID string
	// Agent is an agent name; "" or "auto" lets the model choose.
	Agent string
	// Context, Constraints and OutputFormat are added to the prompt.
	Context      string
	Constraints  []string
	OutputFormat string
	// Template and Vars render a saved prompt template as instructions.
	Template string
	Vars     map[string]string
	// Prompt replaces the generated prompt (e.g. the prompt shown in a
	// model session before it was accepted).
	Prompt            string
	CompleteOnSuccess bool
	// MaxRetries overrides the agent's automatic retries.
	MaxRetries *int
	Initiator  string
	SessionID  string
	Item       int
	// Confirmed skips the agent's confirm step (the user already accepted).
	Confirmed bool
	// Selection records who chose a named agent (default: the user).
	Selection *Selection
	// DryRun only builds the run and its prompt, nothing is stored.
	DryRun bool
}

// Start creates a run: queued, or waiting for confirmation when the agent
// requires it. Execute (or a background executor) runs it.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*Run, error) {
	id, err := m.store.ResolveID(req.TaskID)
	if err != nil {
		return nil, err
	}
	t, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Deleted() {
		return nil, fmt.Errorf("%w: task %s is deleted", core.ErrDeleted, t.ID)
	}
	name := strings.TrimSpace(req.Agent)
	sel := Selection{By: "user"}
	if req.Selection != nil {
		sel = *req.Selection
	}
	if name == "" || strings.EqualFold(name, "auto") {
		if name, sel, err = m.SelectAgent(ctx, t); err != nil {
			return nil, err
		}
	}
	sp, err := m.Agent(name)
	if err != nil {
		return nil, err
	}
	dir, err := m.workdir(sp)
	if err != nil {
		return nil, err
	}
	runID := newRunID()
	in, err := m.buildInput(t, sp, req, runID, dir)
	if err != nil {
		return nil, err
	}
	in = m.redactInput(in)
	initiator := req.Initiator
	if initiator == "" {
		initiator = m.opts.Origin
	}
	opts := RunOpts{CompleteOnSuccess: req.CompleteOnSuccess, MaxRetries: sp.MaxRetries}
	if req.MaxRetries != nil {
		if *req.MaxRetries < 0 || *req.MaxRetries > maxRetries {
			return nil, fmt.Errorf("%w: max_retries must be 0-%d", ErrInvalid, maxRetries)
		}
		opts.MaxRetries = *req.MaxRetries
	}
	status := StatusQueued
	if sp.Confirm && !req.Confirmed {
		status = StatusWaitingConfirmation
	}
	now := m.store.Now()
	run := &Run{ID: runID, TaskID: t.ID, Agent: sp.Name, Adapter: sp.AdapterName(), Status: status, StatusLabel: StatusLabel(status),
		Initiator: initiator, Selection: sel, SessionID: req.SessionID, Item: req.Item, Prompt: in.Prompt, Input: in, Spec: sp,
		Options: opts, CreatedAt: now, UpdatedAt: now, Attempts: []Attempt{}, Results: []Result{}}
	if req.DryRun {
		return run, nil
	}
	inJSON, _ := json.Marshal(in)
	selJSON, _ := json.Marshal(sel)
	specJSON, _ := json.Marshal(sp)
	optJSON, _ := json.Marshal(opts)
	if _, err := m.db().Exec(`INSERT INTO agent_runs(id, task_id, agent, adapter, status, initiator, selection, session_id, item, prompt, input,
		spec, options, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, t.ID, sp.Name, run.Adapter, status, initiator, string(selJSON), req.SessionID, req.Item, in.Prompt, string(inJSON),
		string(specJSON), string(optJSON), fmtTime(now), fmtTime(now)); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("创建运行：智能体 %s（%s 适配器，由 %s 发起", sp.Name, run.Adapter, initiator)
	if sel.By != "user" {
		msg += "，" + selectionLabel(sel)
	}
	msg += "）"
	m.addEvent(runID, 0, EventState, "", msg, map[string]any{"status": status, "selection": sel}, nil)
	if status == StatusWaitingConfirmation {
		m.addEvent(runID, 0, EventState, "", "等待确认：该智能体配置为启动前需要确认（todo agent confirm "+runID+"）", map[string]any{"status": status}, nil)
	}
	_ = m.store.WithActor(agentActor(sp.Name)).RecordEvent(t.ID, "agent_run",
		map[string]core.Change{"run": {To: runID}, "agent": {To: sp.Name}, "status": {To: status}})
	return m.Get(runID)
}

func selectionLabel(s Selection) string {
	switch s.By {
	case "llm":
		return "由大模型选择：" + s.Reason
	case "routing":
		return "按路由规则选择：" + s.Reason
	case "only":
		return "唯一配置的智能体"
	}
	return "用户指定"
}

func (m *Manager) redactInput(in *Input) *Input {
	b, _ := json.Marshal(in)
	var out Input
	if err := json.Unmarshal([]byte(m.redact(string(b), nil)), &out); err != nil {
		// Redaction never breaks JSON strings; keep the original if it did.
		return in
	}
	return &out
}

// ---- execution ----

// Execute runs a queued run in this process until it ends, waiting for a
// free slot, for resumption and for automatic retries. It returns when the
// run is in a final state, or with ErrState when it waits for confirmation
// or is executed elsewhere.
func (m *Manager) Execute(ctx context.Context, id string) error {
	id, err := m.ResolveRun(id)
	if err != nil {
		return err
	}
	execMu.Lock()
	if executing[id] {
		execMu.Unlock()
		return nil
	}
	executing[id] = true
	execMu.Unlock()
	defer func() {
		execMu.Lock()
		delete(executing, id)
		execMu.Unlock()
	}()
	for {
		r, err := m.loadRow(id)
		if err != nil {
			return err
		}
		switch {
		case Terminal(r.Status):
			return nil
		case r.Status == StatusWaitingConfirmation:
			return fmt.Errorf("%w: run %s is waiting for confirmation (todo agent confirm %s)", ErrState, id, id)
		case r.active:
			return fmt.Errorf("%w: run %s is being executed by process %d", ErrState, id, r.OwnerPID)
		case r.Status == StatusQueued:
			if ok, err := m.claim(id); err != nil {
				return err
			} else if ok {
				m.runAttempt(ctx, id)
				continue
			}
		case r.Status == StatusWaitingRetry:
			if r.NextAt == nil || !m.store.Now().Before(*r.NextAt) {
				_, _ = m.db().Exec(`UPDATE agent_runs SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
					StatusQueued, fmtTime(m.store.Now()), id, StatusWaitingRetry)
				continue
			}
		}
		_, _ = m.db().Exec(`UPDATE agent_runs SET owner_pid = ? WHERE id = ? AND active = 0`, m.pid, id)
		select {
		case <-ctx.Done():
			m.interruptIdle(id)
			return ctx.Err()
		case <-time.After(m.opts.PollInterval):
		}
	}
}

// ExecuteIfIdle executes a run unless an executor (in this or another
// live process) already handles it.
func (m *Manager) ExecuteIfIdle(ctx context.Context, id string) error {
	if !m.needsExecutor(id) {
		return nil
	}
	return m.Execute(ctx, id)
}

// Background executes a run in a goroutine bounded by Options.Context.
func (m *Manager) Background(id string) {
	if !m.needsExecutor(id) {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = m.Execute(m.opts.Context, id)
	}()
}

func (m *Manager) needsExecutor(id string) bool {
	r, err := m.loadRow(id)
	if err != nil || Terminal(r.Status) || r.active || r.Status == StatusWaitingConfirmation {
		return false
	}
	if isExecuting(id) {
		return false
	}
	return r.OwnerPID == 0 || r.OwnerPID == m.pid || !processAlive(r.OwnerPID)
}

// claim starts the next attempt if a slot is free.
func (m *Manager) claim(id string) (bool, error) {
	now := fmtTime(m.store.Now())
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, active = 1, attempt = attempt + 1, owner_pid = ?, control = '', stage = '',
		progress = 0, error = '', error_kind = '', exit_code = NULL, next_attempt_at = NULL, paused_at = NULL, paused_ms = 0,
		ended_at = NULL, started_at = coalesce(started_at, ?), updated_at = ?
		WHERE id = ? AND status = ? AND active = 0 AND (SELECT count(*) FROM agent_runs WHERE active = 1) < ?`,
		StatusRunning, m.pid, now, now, id, StatusQueued, m.opts.MaxConcurrent)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// interruptIdle cancels a run that was waiting in this process when the
// process stopped, so it does not stay queued without an executor.
func (m *Manager) interruptIdle(id string) {
	now := fmtTime(m.store.Now())
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, error = ?, error_kind = ?, ended_at = ?, updated_at = ?, owner_pid = 0
		WHERE id = ? AND active = 0 AND status IN (?, ?, ?)`, StatusCancelled, "执行进程已停止，运行未继续", KindInterrupted, now, now,
		id, StatusQueued, StatusWaitingRetry, StatusPaused)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if r, err := m.loadRow(id); err == nil {
			m.addEvent(id, r.Attempt, EventState, "", "已取消：执行进程已停止", map[string]any{"status": StatusCancelled}, nil)
			m.recordEnd(r)
		}
	}
}

type outcome struct {
	status, kind, msg string
	code              *int
}

func (m *Manager) runAttempt(ctx context.Context, id string) {
	r, err := m.loadRow(id)
	if err != nil {
		return
	}
	attempt, sp := r.Attempt, r.Spec
	startedAt := m.store.Now()
	started := time.Now()
	_, _ = m.db().Exec(`INSERT OR REPLACE INTO agent_attempts(run_id, attempt, status, started_at) VALUES (?, ?, ?, ?)`,
		id, attempt, StatusRunning, fmtTime(startedAt))
	m.addEvent(id, attempt, EventState, "", fmt.Sprintf("第 %d 次执行开始", attempt), map[string]any{"status": StatusRunning}, nil)
	if attempt == 1 {
		m.markTaskStarted(r)
	}
	sink := &runSink{m: m, runID: id, taskID: r.TaskID, agent: r.Agent, attempt: attempt, dir: r.Input.Workdir}
	finish := func(o outcome) { m.finishAttempt(r, attempt, started, o, sink) }

	in := r.Input
	in.Attempt = attempt
	if attempt > 1 {
		in.Context.PreviousResults = nil
		if rs, err := m.TaskResults(r.TaskID); err == nil {
			for _, x := range rs {
				in.Context.PreviousResults = append(in.Context.PreviousResults, ResultRef{Type: x.Type, Key: x.Key, Version: x.Version, Agent: x.Agent, Summary: x.Summary})
			}
		}
	}
	inJSON, _ := json.Marshal(in)
	_, _ = m.db().Exec(`UPDATE agent_runs SET input = ? WHERE id = ?`, string(inJSON), id)

	if fi, err := os.Stat(in.Workdir); err != nil || !fi.IsDir() {
		finish(outcome{status: StatusFailed, kind: KindStartFailed, msg: fmt.Sprintf("工作目录 %s 不存在或不可用", in.Workdir)})
		return
	}
	f, ok := factory(sp.AdapterName())
	if !ok {
		finish(outcome{status: StatusFailed, kind: KindAdapterUnavailable, msg: fmt.Sprintf("适配器 %q 不可用（已注册：%s）", sp.AdapterName(), strings.Join(Adapters(), ", "))})
		return
	}
	adapter, err := f(sp, Deps{LLM: m.svc, Getenv: m.opts.Getenv})
	if err != nil {
		finish(outcome{status: StatusFailed, kind: KindAdapterUnavailable, msg: err.Error()})
		return
	}
	job := &Job{RunID: id, TaskID: r.TaskID, Attempt: attempt, Agent: r.Agent, Spec: sp, Input: inJSON, Prompt: r.Prompt,
		Dir: in.Workdir, Env: m.childEnv(sp, r, attempt, in.Workdir, sink), KillGrace: m.opts.KillGrace}
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	limit := timeout(sp)
	stop := m.watch(actx, cancel, id, adapter, limit, sink)
	exit := adapter.Run(actx, job, sink)
	paused := stop()
	sink.pausedMS = paused.Milliseconds()
	finish(m.outcome(ctx, actx, exit, sink, sp, limit))
}

// childEnv is the environment of agent processes: a minimal base, the
// agent's whitelist and the run identifiers; model keys never.
func (m *Manager) childEnv(sp Spec, r *runRow, attempt int, dir string, sink *runSink) []string {
	base := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ"}
	hidden := m.svc.KeyEnvNames()
	var env []string
	seen := map[string]bool{}
	for _, name := range append(base, sp.Env...) {
		if seen[name] || slices.Contains(hidden, name) {
			continue
		}
		seen[name] = true
		if v := m.opts.Getenv(name); v != "" {
			env = append(env, name+"="+v)
			if looksSecretName(name) {
				sink.Secret(v)
			}
		}
	}
	return append(env, "TODO_AGENT_PROTOCOL="+Protocol, "TODO_AGENT_RUN_ID="+r.ID, "TODO_AGENT_TASK_ID="+r.TaskID,
		"TODO_AGENT_ATTEMPT="+strconv.Itoa(attempt), "TODO_AGENT_WORKDIR="+dir, "TODO_AGENT_IDEMPOTENCY_KEY="+r.TaskID+":"+r.ID)
}

func looksSecretName(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// watch applies pause, resume and cancel requests and the timeout, which
// does not count paused time. stop returns the total paused time.
func (m *Manager) watch(ctx context.Context, cancel context.CancelCauseFunc, id string, a Adapter, limit time.Duration, sink *runSink) (stop func() time.Duration) {
	done, stopped := make(chan struct{}), make(chan struct{})
	var paused time.Duration
	go func() {
		defer close(stopped)
		t := time.NewTicker(m.opts.PollInterval)
		defer t.Stop()
		start := time.Now()
		var since time.Time // when the current pause began
		clear := func(c string) {
			_, _ = m.db().Exec(`UPDATE agent_runs SET control = '' WHERE id = ? AND control = ?`, id, c)
		}
		for {
			select {
			case <-done:
				if !since.IsZero() {
					paused += time.Since(since)
				}
				return
			case <-ctx.Done():
				if !since.IsZero() {
					paused += time.Since(since)
				}
				return
			case <-t.C:
			}
			if since.IsZero() && time.Since(start)-paused >= limit {
				sink.Emit(EventError, "", fmt.Sprintf("执行超时（%s），正在终止", limit), nil)
				cancel(errTimeout)
				continue
			}
			var control string
			if err := m.db().QueryRow(`SELECT control FROM agent_runs WHERE id = ?`, id).Scan(&control); err != nil || control == "" {
				continue
			}
			clear(control)
			now := m.store.Now()
			switch control {
			case "cancel":
				sink.Emit(EventState, "", "收到取消请求，正在终止", nil)
				cancel(errCancelled)
			case "pause":
				p, ok := a.(Pauser)
				if !ok || !since.IsZero() {
					continue
				}
				if err := p.Pause(); err != nil {
					sink.Emit(EventError, "", "暂停失败："+err.Error(), nil)
					continue
				}
				since = time.Now()
				_, _ = m.db().Exec(`UPDATE agent_runs SET status = ?, paused_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
					StatusPaused, fmtTime(now), fmtTime(now), id, StatusRunning)
				sink.Emit(EventState, "", "已暂停", map[string]any{"status": StatusPaused})
			case "resume":
				p, ok := a.(Pauser)
				if !ok || since.IsZero() {
					continue
				}
				if err := p.Resume(); err != nil {
					sink.Emit(EventError, "", "继续失败："+err.Error(), nil)
					continue
				}
				d := time.Since(since)
				paused += d
				since = time.Time{}
				_, _ = m.db().Exec(`UPDATE agent_runs SET status = ?, paused_at = NULL, paused_ms = paused_ms + ?, updated_at = ? WHERE id = ? AND status = ?`,
					StatusRunning, d.Milliseconds(), fmtTime(now), id, StatusPaused)
				sink.Emit(EventState, "", "已继续", map[string]any{"status": StatusRunning})
			}
		}
	}()
	return func() time.Duration {
		close(done)
		<-stopped
		return paused
	}
}

// outcome maps how the attempt ended to a run state. Exit codes in
// success_codes mean success unless the agent reported otherwise; a
// failure that already wrote results back is a partial success; an
// attempt killed by its timeout has an unknown outcome (NFR-019, R-13).
func (m *Manager) outcome(parent, actx context.Context, exit Exit, sink *runSink, sp Spec, limit time.Duration) outcome {
	cause := context.Cause(actx)
	sink.mu.Lock()
	results, errs, invalid, declared, declaredMsg, lastErr := sink.results, sink.errors, sink.invalid, sink.declared, sink.declaredMsg, sink.lastErr
	sink.mu.Unlock()
	code := exit.Code
	o := outcome{code: &code}
	switch {
	case errors.Is(cause, errCancelled):
		return outcome{status: StatusCancelled, kind: KindCancelled, msg: "已被用户取消", code: o.code}
	case errors.Is(cause, errTimeout):
		return outcome{status: StatusUnknown, kind: KindTimeout, code: o.code,
			msg: fmt.Sprintf("执行超时（%s）已终止；结果未知，不会自动重试，请核对后手动重试", limit)}
	case parent.Err() != nil:
		return outcome{status: StatusCancelled, kind: KindInterrupted, msg: "执行进程已停止，运行被中断", code: o.code}
	case exit.Err != nil:
		kind := exit.ErrKind
		if kind == "" {
			kind = KindStartFailed
		}
		return outcome{status: StatusFailed, kind: kind, msg: exit.Err.Error()}
	}
	detail := declaredMsg
	if detail == "" {
		detail = lastErr
	}
	ok := slices.Contains(successCodes(sp), code)
	switch {
	case declared == StatusUnknown:
		o.status, o.kind, o.msg = StatusUnknown, KindDeclared, orDefault(detail, "智能体报告结果未知")
	case ok && declared == StatusFailed:
		o.status, o.kind, o.msg = StatusFailed, KindDeclared, orDefault(detail, "智能体报告失败")
	case ok && (declared == StatusPartial || invalid > 0 || (declared == "" && errs > 0)):
		o.status, o.kind, o.msg = StatusPartial, KindDeclared, orDefault(detail, "部分完成")
		if invalid > 0 {
			o.msg = fmt.Sprintf("%s；%d 个结果无效未写回", o.msg, invalid)
		}
	case ok:
		o.status = StatusSucceeded
	case results > 0 || declared == StatusPartial:
		o.status, o.kind = StatusPartial, KindExitCode
		o.msg = fmt.Sprintf("退出码 %d；已写回 %d 个结果", code, results)
		if detail != "" {
			o.msg += "：" + detail
		}
	default:
		o.status, o.kind = StatusFailed, KindExitCode
		o.msg = fmt.Sprintf("退出码 %d", code)
		if detail != "" {
			o.msg += "：" + detail
		}
	}
	return o
}

func retryableKind(kind string) bool {
	return kind == KindExitCode || kind == KindDeclared || kind == KindUnreachable
}

func (m *Manager) finishAttempt(r *runRow, attempt int, started time.Time, o outcome, sink *runSink) {
	now := m.store.Now()
	o.msg = clip(m.redact(o.msg, sink.secretList()), 2000)
	sink.mu.Lock()
	results, retryable := sink.results, sink.retryable
	sink.mu.Unlock()
	dur := max(time.Since(started).Milliseconds()-sink.pausedMS, 0)
	var exit any
	if o.code != nil {
		exit = *o.code
	}
	attemptStatus := o.status
	retry := o.status == StatusFailed && retryableKind(o.kind) && results == 0 && (retryable == nil || *retryable) &&
		attempt <= r.Options.MaxRetries
	_, _ = m.db().Exec(`UPDATE agent_attempts SET status = ?, ended_at = ?, duration_ms = ?, exit_code = ?, error = ?, error_kind = ?, results = ?
		WHERE run_id = ? AND attempt = ?`, attemptStatus, fmtTime(now), dur, exit, o.msg, o.kind, results, r.ID, attempt)
	if retry {
		delay := time.Duration(r.Spec.RetryDelaySeconds) * time.Second
		next := now.Add(delay)
		_, _ = m.db().Exec(`UPDATE agent_runs SET status = ?, active = 0, error = ?, error_kind = ?, exit_code = ?, next_attempt_at = ?,
			paused_at = NULL, control = '', updated_at = ? WHERE id = ?`, StatusWaitingRetry, o.msg, o.kind, exit, fmtTime(next), fmtTime(now), r.ID)
		m.addEvent(r.ID, attempt, EventState, "", fmt.Sprintf("第 %d 次执行失败（%s）；%s后自动重试（%d/%d）", attempt, o.msg,
			delay, attempt, r.Options.MaxRetries), map[string]any{"status": StatusWaitingRetry, "error_kind": o.kind}, nil)
		return
	}
	_, _ = m.db().Exec(`UPDATE agent_runs SET status = ?, active = 0, error = ?, error_kind = ?, exit_code = ?, ended_at = ?, paused_at = NULL,
		control = '', next_attempt_at = NULL, updated_at = ? WHERE id = ?`, o.status, o.msg, o.kind, exit, fmtTime(now), fmtTime(now), r.ID)
	msg := fmt.Sprintf("运行结束：%s", StatusLabel(o.status))
	if o.msg != "" {
		msg += "（" + o.msg + "）"
	}
	m.addEvent(r.ID, attempt, EventState, "", msg, map[string]any{"status": o.status, "error_kind": o.kind, "exit_code": exit, "results": results}, nil)
	r.Status, r.Error, r.ErrorKind = o.status, o.msg, o.kind
	m.recordEnd(r)
	if o.status == StatusSucceeded {
		m.completeTask(r, sink.localStatusList())
	}
}

// markTaskStarted moves a todo task to in progress when its first attempt
// starts.
func (m *Manager) markTaskStarted(r *runRow) {
	t, err := m.store.Get(r.TaskID)
	if err != nil || t.Deleted() || t.Status != core.StatusTodo {
		return
	}
	_, _ = m.store.WithActor(agentActor(r.Agent)).Start(r.TaskID)
}

// recordEnd adds the run's final state to the task history (FR-511).
func (m *Manager) recordEnd(r *runRow) {
	changes := map[string]core.Change{"run": {To: r.ID}, "agent": {To: r.Agent}, "status": {To: r.Status}}
	if r.Error != "" {
		changes["error"] = core.Change{To: r.Error}
	}
	_ = m.store.WithActor(agentActor(r.Agent)).RecordEvent(r.TaskID, "agent_"+r.Status, changes)
}

// completeTask applies task statuses after a successful run: the ones the
// agent requested (task_status results with local_status) and "done" when
// the run was started with CompleteOnSuccess. Failed, partial, cancelled
// and unknown runs never change the task to done (FR-509).
func (m *Manager) completeTask(r *runRow, statuses []string) {
	if r.Options.CompleteOnSuccess && !slices.Contains(statuses, string(core.StatusDone)) {
		statuses = append(statuses, string(core.StatusDone))
	}
	if len(statuses) == 0 {
		return
	}
	target := core.Status(statuses[len(statuses)-1])
	if slices.Contains(statuses, string(core.StatusDone)) {
		target = core.StatusDone
	}
	t, err := m.store.Get(r.TaskID)
	if err != nil || t.Deleted() || t.Status == target {
		return
	}
	if _, err := m.store.WithActor(agentActor(r.Agent)).Update(r.TaskID, core.TaskPatch{Status: &target}); err != nil {
		m.addEvent(r.ID, r.Attempt, EventError, "", "更新任务状态失败："+err.Error(), nil, nil)
		return
	}
	m.addEvent(r.ID, r.Attempt, EventSystem, "", "任务状态已更新为 "+string(target), map[string]any{"task_status": string(target)}, nil)
}

// ---- control ----

// Pause suspends a running attempt (or holds a queued run) (FR-504).
func (m *Manager) Pause(id string) (*Run, error) {
	r, err := m.row(id)
	if err != nil {
		return nil, err
	}
	switch {
	case r.active && r.Status == StatusRunning:
		f, _ := factory(r.Spec.AdapterName())
		if f != nil {
			if a, err := f(r.Spec, Deps{LLM: m.svc, Getenv: m.opts.Getenv}); err == nil {
				if _, ok := a.(Pauser); !ok {
					return nil, fmt.Errorf("%w: the %s adapter cannot pause a running attempt; cancel it instead", ErrUnsupported, r.Spec.AdapterName())
				}
			}
		}
		if err := m.setControl(r.ID, "pause"); err != nil {
			return nil, err
		}
		m.addEvent(r.ID, r.Attempt, EventState, "", "请求暂停", nil, nil)
	case !r.active && (r.Status == StatusQueued || r.Status == StatusWaitingRetry):
		if err := m.transition(r.ID, StatusPaused, r.Status); err != nil {
			return nil, err
		}
		m.addEvent(r.ID, r.Attempt, EventState, "", "已暂停（尚未开始执行）", map[string]any{"status": StatusPaused}, nil)
	default:
		return nil, fmt.Errorf("%w: run %s is %s and cannot be paused", ErrState, r.ID, StatusLabel(r.Status))
	}
	return m.Get(r.ID)
}

// Resume continues a paused run.
func (m *Manager) Resume(id string) (*Run, error) {
	r, err := m.row(id)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Status == StatusPaused && r.active:
		if err := m.setControl(r.ID, "resume"); err != nil {
			return nil, err
		}
		m.addEvent(r.ID, r.Attempt, EventState, "", "请求继续", nil, nil)
	case r.Status == StatusPaused:
		if err := m.transition(r.ID, StatusQueued, StatusPaused); err != nil {
			return nil, err
		}
		m.addEvent(r.ID, r.Attempt, EventState, "", "已继续，重新排队", map[string]any{"status": StatusQueued}, nil)
	case r.Status == StatusRunning && r.Control == "pause":
		if err := m.setControl(r.ID, ""); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: run %s is %s, not paused", ErrState, r.ID, StatusLabel(r.Status))
	}
	return m.Get(r.ID)
}

// Cancel stops a run: a running attempt is terminated (its process group
// gets SIGTERM, then SIGKILL), a waiting run ends at once. Results written
// so far are kept.
func (m *Manager) Cancel(id string) (*Run, error) {
	for range 3 {
		r, err := m.row(id)
		if err != nil {
			return nil, err
		}
		if Terminal(r.Status) {
			return nil, fmt.Errorf("%w: run %s already ended (%s)", ErrState, r.ID, StatusLabel(r.Status))
		}
		if r.active {
			if err := m.setControl(r.ID, "cancel"); err != nil {
				return nil, err
			}
			m.addEvent(r.ID, r.Attempt, EventState, "", "请求取消", nil, nil)
			return m.Get(r.ID)
		}
		now := fmtTime(m.store.Now())
		res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, error = ?, error_kind = ?, ended_at = ?, updated_at = ?, control = ''
			WHERE id = ? AND active = 0 AND status = ?`, StatusCancelled, "已被用户取消", KindCancelled, now, now, r.ID, r.Status)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			m.addEvent(r.ID, r.Attempt, EventState, "", "已取消", map[string]any{"status": StatusCancelled}, nil)
			r.Status, r.Error = StatusCancelled, "已被用户取消"
			m.recordEnd(r)
			return m.Get(r.ID)
		}
	}
	return nil, fmt.Errorf("%w: run %s changed state; try again", ErrState, id)
}

// RetryRequest retries a run.
type RetryRequest struct {
	// Context is added to the prompt (e.g. corrected parameters).
	Context string
}

// Retry queues a new attempt of an ended run (or of one waiting for an
// automatic retry). Earlier attempts, logs and results are kept (FR-504).
func (m *Manager) Retry(id string, req RetryRequest) (*Run, error) {
	r, err := m.row(id)
	if err != nil {
		return nil, err
	}
	allowed := []string{StatusFailed, StatusPartial, StatusCancelled, StatusUnknown, StatusWaitingRetry}
	if r.active || !slices.Contains(allowed, r.Status) {
		return nil, fmt.Errorf("%w: run %s is %s; only failed, partial, cancelled, unknown or waiting runs can be retried",
			ErrState, r.ID, StatusLabel(r.Status))
	}
	if extra := strings.TrimSpace(req.Context); extra != "" {
		extra = m.redact(extra, nil)
		in := r.Input
		in.Context.User = strings.TrimSpace(in.Context.User + "\n" + extra)
		in.Prompt = r.Prompt + "\n# 重试补充说明\n" + extra + "\n"
		b, _ := json.Marshal(in)
		if _, err := m.db().Exec(`UPDATE agent_runs SET prompt = ?, input = ? WHERE id = ?`, in.Prompt, string(b), r.ID); err != nil {
			return nil, err
		}
		m.addEvent(r.ID, r.Attempt, EventSystem, "", "提示词已补充："+extra, nil, nil)
	}
	now := fmtTime(m.store.Now())
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, next_attempt_at = NULL, ended_at = NULL, owner_pid = 0, control = '', updated_at = ?
		WHERE id = ? AND active = 0 AND status = ?`, StatusQueued, now, r.ID, r.Status)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%w: run %s changed state; try again", ErrState, r.ID)
	}
	m.addEvent(r.ID, r.Attempt, EventState, "", fmt.Sprintf("手动重试（保留之前 %d 次执行的记录）", r.Attempt), map[string]any{"status": StatusQueued}, nil)
	return m.Get(r.ID)
}

// Confirm lets a run that waits for confirmation start.
func (m *Manager) Confirm(id string) (*Run, error) {
	r, err := m.row(id)
	if err != nil {
		return nil, err
	}
	if err := m.transition(r.ID, StatusQueued, StatusWaitingConfirmation); err != nil {
		return nil, err
	}
	m.addEvent(r.ID, 0, EventState, "", "已确认，开始排队", map[string]any{"status": StatusQueued}, nil)
	return m.Get(r.ID)
}

// Reject cancels a run that waits for confirmation.
func (m *Manager) Reject(id string) (*Run, error) {
	r, err := m.row(id)
	if err != nil {
		return nil, err
	}
	now := fmtTime(m.store.Now())
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, error = ?, error_kind = ?, ended_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusCancelled, "启动未被确认", KindRejected, now, now, r.ID, StatusWaitingConfirmation)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("%w: run %s is %s, not waiting for confirmation", ErrState, r.ID, StatusLabel(r.Status))
	}
	m.addEvent(r.ID, 0, EventState, "", "已拒绝启动", map[string]any{"status": StatusCancelled}, nil)
	r.Status, r.Error = StatusCancelled, "启动未被确认"
	m.recordEnd(r)
	return m.Get(r.ID)
}

func (m *Manager) row(id string) (*runRow, error) {
	id, err := m.ResolveRun(id)
	if err != nil {
		return nil, err
	}
	r, err := m.loadRow(id)
	if err != nil {
		return nil, err
	}
	if r.active && m.orphaned(r) {
		m.recoverRun(r)
		return m.loadRow(id)
	}
	return r, nil
}

func (m *Manager) setControl(id, control string) error {
	_, err := m.db().Exec(`UPDATE agent_runs SET control = ?, updated_at = ? WHERE id = ?`, control, fmtTime(m.store.Now()), id)
	return err
}

func (m *Manager) transition(id, to, from string) error {
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, updated_at = ? WHERE id = ? AND status = ? AND active = 0`,
		to, fmtTime(m.store.Now()), id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: run %s is not %s", ErrState, id, StatusLabel(from))
	}
	return nil
}

// ---- recovery ----

func (m *Manager) orphaned(r *runRow) bool {
	if r.OwnerPID == m.pid {
		return !isExecuting(r.ID)
	}
	return !processAlive(r.OwnerPID)
}

// Recover marks runs whose executing process died (crash, kill, reboot) as
// "unknown": they may have had side effects, so they are not retried
// automatically.
func (m *Manager) Recover() error {
	rows, err := m.db().Query(`SELECT ` + runColumns + ` FROM agent_runs WHERE active = 1`)
	if err != nil {
		return err
	}
	var list []*runRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		if m.orphaned(r) {
			m.recoverRun(r)
		}
	}
	return rows.Err()
}

func (m *Manager) recoverRun(r *runRow) {
	now := fmtTime(m.store.Now())
	msg := fmt.Sprintf("执行进程 %d 已退出（崩溃或被终止）；结果未知，请核对后手动重试", r.OwnerPID)
	res, err := m.db().Exec(`UPDATE agent_runs SET status = ?, active = 0, error = ?, error_kind = ?, ended_at = ?, updated_at = ?, paused_at = NULL, control = ''
		WHERE id = ? AND active = 1 AND owner_pid = ?`, StatusUnknown, msg, KindOrphaned, now, now, r.ID, r.OwnerPID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	_, _ = m.db().Exec(`UPDATE agent_attempts SET status = ?, ended_at = ?, error = ?, error_kind = ? WHERE run_id = ? AND attempt = ? AND ended_at IS NULL`,
		StatusUnknown, now, msg, KindOrphaned, r.ID, r.Attempt)
	m.addEvent(r.ID, r.Attempt, EventState, "", "结果未知："+msg, map[string]any{"status": StatusUnknown}, nil)
	r.Status, r.Error = StatusUnknown, msg
	m.recordEnd(r)
}

// ---- sink ----

// runSink stores what an attempt's adapter reports.
type runSink struct {
	m       *Manager
	runID   string
	taskID  string
	agent   string
	attempt int
	dir     string

	mu          sync.Mutex
	secrets     []string
	events      int
	dropped     bool
	results     int
	invalid     int
	errors      int
	declared    string
	declaredMsg string
	lastErr     string
	retryable   *bool
	local       []string
	pausedMS    int64
}

const maxEvents = 20000

func (s *runSink) secretList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.secrets)
}

func (s *runSink) localStatusList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.local)
}

func (s *runSink) Secret(v string) {
	if len(v) < 4 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.secrets, v) {
		s.secrets = append(s.secrets, v)
	}
}

func (s *runSink) Emit(kind, stream, message string, data map[string]any) {
	s.mu.Lock()
	if kind == EventError {
		s.errors++
		s.lastErr = message
	}
	if kind == EventOutput && stream == "stderr" && strings.TrimSpace(message) != "" {
		s.lastErr = message
	}
	s.events++
	if s.events > maxEvents && kind == EventOutput {
		dropped := s.dropped
		s.dropped = true
		s.mu.Unlock()
		if !dropped {
			s.m.addEvent(s.runID, s.attempt, EventSystem, "", fmt.Sprintf("日志超过 %d 条，后续普通输出不再记录", maxEvents), nil, nil)
		}
		return
	}
	secrets := slices.Clone(s.secrets)
	s.mu.Unlock()
	s.m.addEvent(s.runID, s.attempt, kind, stream, message, data, secrets)
}

func (s *runSink) Message(msg map[string]any) {
	typ, _ := msg["type"].(string)
	text := str(msg, "message", "text")
	switch strings.ToLower(typ) {
	case "log":
		s.Emit(EventLog, "", text, pick(msg, "level"))
	case "progress":
		stage := str(msg, "stage")
		pct, hasPct := msg["percent"].(float64)
		label := stage
		if hasPct {
			pct = min(max(pct, 0), 100)
			label = strings.TrimSpace(fmt.Sprintf("%s %.0f%%", stage, pct))
		}
		if text != "" {
			label = strings.TrimSpace(label + " " + text)
		}
		now := fmtTime(s.m.store.Now())
		if hasPct {
			_, _ = s.m.db().Exec(`UPDATE agent_runs SET stage = ?, progress = ?, updated_at = ? WHERE id = ?`, s.m.redact(stage, s.secretList()), pct, now, s.runID)
		} else {
			_, _ = s.m.db().Exec(`UPDATE agent_runs SET stage = ?, updated_at = ? WHERE id = ?`, s.m.redact(stage, s.secretList()), now, s.runID)
		}
		s.Emit(EventProgress, "", label, pick(msg, "stage", "percent"))
	case "command":
		cmd := str(msg, "command")
		if argv := strList(msg["argv"]); len(argv) > 0 {
			cmd = strings.Join(argv, " ")
		}
		line := cmd
		if c, ok := msg["exit_code"].(float64); ok {
			line = fmt.Sprintf("%s → 退出码 %d", cmd, int(c))
		}
		s.Emit(EventCommand, "", line, pick(msg, "argv", "command", "exit_code", "dir"))
	case "error":
		if r, ok := msg["retryable"].(bool); ok {
			s.mu.Lock()
			s.retryable = &r
			s.mu.Unlock()
		}
		s.Emit(EventError, "", orDefault(text, "智能体报告错误"), pick(msg, "code", "retryable"))
	case "status":
		st := normDeclared(str(msg, "status"))
		s.mu.Lock()
		s.declared, s.declaredMsg = st, text
		s.mu.Unlock()
		s.Emit(EventLog, "", "智能体报告状态："+StatusLabel(orDefault(st, str(msg, "status")))+" "+text, pick(msg, "status"))
	case "result":
		r, err := s.m.SaveResult(ResultInput{TaskID: s.taskID, RunID: s.runID, Attempt: s.attempt, Agent: s.agent, Raw: s.m.redactMap(msg, s.secretList()), Dir: s.dir})
		if err != nil {
			s.mu.Lock()
			s.invalid++
			s.mu.Unlock()
			s.Emit(EventError, "", "结果未写回："+err.Error(), nil)
			return
		}
		s.mu.Lock()
		s.results++
		if ls, ok := r.Data["local_status"].(string); ok && !slices.Contains(s.local, ls) {
			s.local = append(s.local, ls)
		}
		s.mu.Unlock()
		label := fmt.Sprintf("结果 [%s] %s", r.Type, r.Summary)
		switch {
		case r.Duplicate:
			label += "（与已有版本相同，未重复写入）"
		case r.Version > 1:
			label += fmt.Sprintf("（新版本 v%d，旧版本保留）", r.Version)
		}
		s.Emit(EventResult, "", label, map[string]any{"result_id": r.ID, "type": r.Type, "key": r.Key, "version": r.Version, "duplicate": r.Duplicate})
	default:
		b, _ := json.Marshal(msg)
		s.Emit(EventLog, "", string(b), nil)
	}
}

func normDeclared(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "succeeded", "success", "ok", "done", "completed":
		return StatusSucceeded
	case "partial", "partially_succeeded", "partial_success":
		return StatusPartial
	case "failed", "failure", "error":
		return StatusFailed
	case "unknown":
		return StatusUnknown
	}
	return ""
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}
