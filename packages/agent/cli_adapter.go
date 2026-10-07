package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cliAdapter is the generic command-line adapter (FR-505): it runs the
// configured program without a shell, so task text is never interpreted
// as shell syntax, in its own process group so pause, resume and cancel
// reach every process it starts.
type cliAdapter struct {
	spec Spec

	mu   sync.Mutex
	proc *os.Process
}

func newCLIAdapter(spec Spec, _ Deps) (Adapter, error) { return &cliAdapter{spec: spec}, nil }

// placeholders may appear in the configured arguments.
func expandArgs(args []string, job *Job, inputFile string) []string {
	r := strings.NewReplacer("{{prompt}}", job.Prompt, "{{input}}", string(job.Input), "{{input_file}}", inputFile,
		"{{task_id}}", job.TaskID, "{{run_id}}", job.RunID, "{{attempt}}", strconv.Itoa(job.Attempt), "{{workdir}}", job.Dir)
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = r.Replace(a)
	}
	return out
}

func (a *cliAdapter) Run(ctx context.Context, job *Job, sink Sink) Exit {
	f, err := os.CreateTemp("", "todo-agent-*.json")
	if err != nil {
		return Exit{Code: -1, Err: err, ErrKind: KindStartFailed}
	}
	inputFile := f.Name()
	defer os.Remove(inputFile)
	_ = f.Chmod(0o600)
	_, err = f.Write(job.Input)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Exit{Code: -1, Err: err, ErrKind: KindStartFailed}
	}

	argv := expandArgs(a.spec.Command, job, inputFile)
	stdin := ""
	switch a.spec.Input {
	case InputJSONStdin:
		stdin = string(job.Input)
	case InputPromptStdin:
		stdin = job.Prompt
	case InputJSONArg:
		argv = append(argv, string(job.Input))
	case InputPromptArg:
		argv = append(argv, job.Prompt)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = job.Dir
	cmd.Env = append(append([]string{}, job.Env...), "TODO_AGENT_INPUT_FILE="+inputFile)
	cmd.Stdin = strings.NewReader(stdin)
	setProcessGroup(cmd)
	parser := NewParser(a.spec.Output, sink)
	var outMu sync.Mutex // stdout and stderr lines are logged in order
	stdout := &lineWriter{fn: func(l string) { outMu.Lock(); parser.Line("stdout", l); outMu.Unlock() }}
	stderr := &lineWriter{fn: func(l string) { outMu.Lock(); sink.Emit(EventOutput, "stderr", l, nil); outMu.Unlock() }}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Output pipes held open by orphaned grandchildren must not block Wait.
	cmd.WaitDelay = job.KillGrace + 2*time.Second

	shown := make([]string, len(argv))
	for i, s := range argv {
		shown[i] = clip(s, 200)
	}
	names := make([]string, 0, len(cmd.Env))
	for _, kv := range cmd.Env {
		n, _, _ := strings.Cut(kv, "=")
		names = append(names, n)
	}
	sink.Emit(EventSystem, "", "启动命令："+strings.Join(quoteArgs(shown), " "),
		map[string]any{"argv": shown, "dir": job.Dir, "env": names, "input": a.spec.Input, "output": a.spec.Output})
	if err := cmd.Start(); err != nil {
		return Exit{Code: -1, Err: fmt.Errorf("无法启动 %s：%w", argv[0], err), ErrKind: KindStartFailed}
	}
	a.mu.Lock()
	a.proc = cmd.Process
	a.mu.Unlock()
	sink.Emit(EventSystem, "", fmt.Sprintf("进程已启动 pid=%d", cmd.Process.Pid), map[string]any{"pid": cmd.Process.Pid})

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		sink.Emit(EventSystem, "", "正在终止进程组（SIGTERM）", nil)
		_ = signalGroup(cmd.Process, sigCont)
		_ = signalGroup(cmd.Process, sigTerm)
		select {
		case waitErr = <-done:
		case <-time.After(job.KillGrace):
			sink.Emit(EventSystem, "", "进程未在宽限期内退出，强制结束（SIGKILL）", nil)
			_ = signalGroup(cmd.Process, sigKill)
			waitErr = <-done
		}
	}
	// Leftover processes of the group must not outlive the attempt.
	_ = signalGroup(cmd.Process, sigKill)
	outMu.Lock()
	stdout.flush()
	stderr.flush()
	outMu.Unlock()
	parser.Finish()
	a.mu.Lock()
	a.proc = nil
	a.mu.Unlock()

	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &ee) && !errors.Is(waitErr, exec.ErrWaitDelay) {
		return Exit{Code: -1, Err: waitErr, ErrKind: KindStartFailed}
	}
	return Exit{Code: code}
}

func (a *cliAdapter) signal(sig signal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc == nil {
		return fmt.Errorf("%w: the process is not running", ErrState)
	}
	return signalGroup(a.proc, sig)
}

// Pause stops every process of the attempt (SIGSTOP).
func (a *cliAdapter) Pause() error { return a.signal(sigStop) }

// Resume continues a paused attempt (SIGCONT).
func (a *cliAdapter) Resume() error { return a.signal(sigCont) }

func quoteArgs(argv []string) []string {
	out := make([]string, len(argv))
	for i, s := range argv {
		if s == "" || strings.ContainsAny(s, " \t\n\"'\\$`|&;<>(){}*?[]#~") {
			s = strconv.Quote(s)
		}
		out[i] = s
	}
	return out
}
