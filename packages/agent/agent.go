// Package agent runs agents on tasks and writes their results back
// (FR-501…FR-511). An agent is an llm.AgentProfile reached through an
// adapter: the generic command-line adapter ("cli"), an HTTP adapter for
// applications with their own interface ("http"), a prompt agent backed by
// a model profile ("llm"), or any adapter registered with Register.
//
// Every run is stored in the task database with its attempts, a complete
// redacted event log (output, commands, progress, errors, state changes)
// and its results. Runs are controlled through the database, so any entry
// point (CLI, web) can pause, resume, cancel or retry a run executed by
// another process.
package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
)

// Protocol is the version of the adapter protocol: the JSON input an agent
// receives and the JSON Lines it writes on stdout.
const Protocol = "todo-agent/v1"

// Run states (FR-502).
const (
	StatusQueued              = "queued"               // 排队中
	StatusWaitingConfirmation = "waiting_confirmation" // 等待确认
	StatusRunning             = "running"              // 运行中
	StatusPaused              = "paused"               // 已暂停
	StatusWaitingRetry        = "waiting_retry"        // 等待重试
	StatusSucceeded           = "succeeded"            // 成功
	StatusPartial             = "partial"              // 部分成功
	StatusFailed              = "failed"               // 失败
	StatusCancelled           = "cancelled"            // 已取消
	StatusUnknown             = "unknown"              // 结果未知: never retried automatically
)

// Statuses lists every run state.
var Statuses = []string{StatusQueued, StatusWaitingConfirmation, StatusRunning, StatusPaused, StatusWaitingRetry,
	StatusSucceeded, StatusPartial, StatusFailed, StatusCancelled, StatusUnknown}

// Terminal reports whether a run in this state has ended.
func Terminal(status string) bool {
	return slices.Contains([]string{StatusSucceeded, StatusPartial, StatusFailed, StatusCancelled, StatusUnknown}, status)
}

// StatusLabel is the user-facing name of a state.
func StatusLabel(status string) string {
	switch status {
	case StatusQueued:
		return "排队中"
	case StatusWaitingConfirmation:
		return "等待确认"
	case StatusRunning:
		return "运行中"
	case StatusPaused:
		return "已暂停"
	case StatusWaitingRetry:
		return "等待重试"
	case StatusSucceeded:
		return "成功"
	case StatusPartial:
		return "部分成功"
	case StatusFailed:
		return "失败"
	case StatusCancelled:
		return "已取消"
	case StatusUnknown:
		return "结果未知"
	}
	return status
}

// Error kinds of failed, cancelled and unknown runs.
const (
	KindExitCode           = "exit_code"           // the agent exited with a code not in success_codes
	KindDeclared           = "declared"            // the agent reported failure itself
	KindTimeout            = "timeout"             // killed after the timeout: outcome unknown
	KindCancelled          = "cancelled"           // cancelled by the user
	KindInterrupted        = "interrupted"         // the executing process stopped
	KindStartFailed        = "start_failed"        // the program could not be started
	KindAdapterUnavailable = "adapter_unavailable" // no such adapter or agent (NFR-035)
	KindUnreachable        = "unreachable"         // the http adapter could not connect
	KindRejected           = "rejected"            // the start was not confirmed
	KindOrphaned           = "orphaned"            // the executing process died: outcome unknown
)

var (
	// ErrNotFound means no such run, agent or result (same as core's).
	ErrNotFound = core.ErrNotFound
	// ErrInvalid reports bad input or agent configuration (same as core's).
	ErrInvalid = core.ErrInvalid
	// ErrState rejects an operation the run's state does not allow (same
	// as the model sessions').
	ErrState = llm.ErrState
	// ErrUnsupported means the adapter cannot do this (e.g. pause an HTTP
	// call); it matches ErrState.
	ErrUnsupported = fmt.Errorf("%w: not supported by this adapter", llm.ErrState)
)

// Run is one agent job on a task. Retrying it starts a new attempt; earlier
// attempts and their logs are kept (FR-504).
type Run struct {
	ID      string `json:"id"`
	TaskID  string `json:"task_id"`
	Agent   string `json:"agent"`
	Adapter string `json:"adapter"`
	Status  string `json:"status"`
	// StatusLabel is the Chinese name of Status.
	StatusLabel string `json:"status_label"`
	Attempt     int    `json:"attempt"`
	// Initiator started the run: cli, web, llm …
	Initiator string    `json:"initiator"`
	Selection Selection `json:"selection"`
	SessionID string    `json:"session_id,omitempty"`
	Item      int       `json:"item,omitempty"`
	// Prompt is the final prompt the agent receives (FR-506).
	Prompt string `json:"prompt"`
	// Input is the JSON document passed to the agent.
	Input   *Input  `json:"input,omitempty"`
	Spec    Spec    `json:"spec"`
	Options RunOpts `json:"options"`
	// Control is a pending pause/resume/cancel request.
	Control   string     `json:"control,omitempty"`
	Stage     string     `json:"stage,omitempty"`
	Progress  float64    `json:"progress,omitempty"`
	Error     string     `json:"error,omitempty"`
	ErrorKind string     `json:"error_kind,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	OwnerPID  int        `json:"owner_pid,omitempty"`
	NextAt    *time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
	// DurationMS is the running time so far or in total, without pauses.
	DurationMS int64     `json:"duration_ms"`
	PausedMS   int64     `json:"paused_ms,omitempty"`
	Attempts   []Attempt `json:"attempts,omitempty"`
	Results    []Result  `json:"results,omitempty"`
}

// RunOpts are per-run choices.
type RunOpts struct {
	// CompleteOnSuccess marks the task done when the run succeeds; it is
	// never applied to a failed, partial or unknown run (FR-509).
	CompleteOnSuccess bool `json:"complete_on_success,omitempty"`
	MaxRetries        int  `json:"max_retries,omitempty"`
}

// Attempt is one execution of a run.
type Attempt struct {
	Attempt    int        `json:"attempt"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	ErrorKind  string     `json:"error_kind,omitempty"`
	Results    int        `json:"results"`
}

// Selection records how the agent was chosen (FR-501).
type Selection struct {
	// By is user, llm (the model chose), routing (a routing rule matched)
	// or only (the only configured agent).
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
	// Degraded is set when the model was asked but unavailable.
	Degraded bool `json:"degraded,omitempty"`
}

// Event kinds of the run log (FR-503).
const (
	EventState    = "state"    // status transitions
	EventSystem   = "system"   // what the framework did (start command, env names, signals)
	EventOutput   = "output"   // raw stdout/stderr lines
	EventLog      = "log"      // agent log messages
	EventProgress = "progress" // stage and percent
	EventCommand  = "command"  // commands the agent ran
	EventError    = "error"    // errors reported by the agent or the framework
	EventResult   = "result"   // results written back
)

// Event is one entry of a run's log. Everything is redacted before it is
// stored.
type Event struct {
	ID      int64          `json:"id"`
	RunID   string         `json:"run_id"`
	Attempt int            `json:"attempt"`
	Kind    string         `json:"kind"`
	Stream  string         `json:"stream,omitempty"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
	At      time.Time      `json:"at"`
}
