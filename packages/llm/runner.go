package llm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CommandSpec is a command run without a shell: argv is passed as is, so
// task text can never be interpreted as shell syntax.
type CommandSpec struct {
	Argv    []string      `json:"argv"`
	Dir     string        `json:"dir,omitempty"`
	Stdin   string        `json:"-"`
	Timeout time.Duration `json:"-"`
	// HideEnv lists environment variables (API keys) the command must not
	// inherit.
	HideEnv []string `json:"-"`
}

// CommandResult keeps stdout, stderr and the exit code (NFR-036).
type CommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Runner executes commands.
type Runner interface {
	Run(ctx context.Context, spec CommandSpec) CommandResult
}

// ExecRunner runs real processes as the current user.
type ExecRunner struct{}

const maxOutput = 64 << 10

func (ExecRunner) Run(ctx context.Context, spec CommandSpec) CommandResult {
	if spec.Timeout <= 0 {
		spec.Timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Stdin = strings.NewReader(spec.Stdin)
	cmd.Env = filterEnv(os.Environ(), spec.HideEnv)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{b: &out}, &limited{b: &errb}
	err := cmd.Run()
	res := CommandResult{Stdout: out.String(), Stderr: errb.String(), DurationMS: time.Since(start).Milliseconds()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode, res.Error = true, -1, "timed out after "+spec.Timeout.String()
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode, res.Error = -1, err.Error()
	}
	return res
}

func filterEnv(env, hide []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		keep := true
		for _, h := range hide {
			if name == h {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

type limited struct{ b *bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if room := maxOutput - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
			l.b.WriteString("\n…[truncated]")
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
