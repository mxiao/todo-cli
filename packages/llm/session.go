package llm

import (
	"time"
)

// Session kinds.
const (
	KindIntake   = "intake"   // natural language → tasks (FR-301…307)
	KindDecide   = "decide"   // decision support (FR-401…406)
	KindOptimize = "optimize" // self-optimisation suggestions (FR-407/408)
)

// Session states.
const (
	StatusNeedsInput = "needs_input" // waiting for answers to the one clarifying round
	StatusPending    = "pending"     // items wait for confirmation
	StatusApplied    = "applied"     // every item was handled
	StatusPartial    = "partial"     // some items applied, others still pending
	StatusRejected   = "rejected"    // ignored / rejected by the user
	StatusUndone     = "undone"
	StatusSuperseded = "superseded" // replaced by a re-decision
)

// Item states.
const (
	ItemPending  = "pending"
	ItemApplied  = "applied"
	ItemRejected = "rejected"
	ItemSkipped  = "skipped" // not executed: invalid or incomplete
	ItemFailed   = "failed"
	ItemUndone   = "undone"
)

// Item kinds.
const (
	ItemCreate   = "create"
	ItemSubtask  = "subtask"
	ItemUpdate   = "update"
	ItemChange   = "change"   // one decision change (priority, due, status, deps, order)
	ItemCommand  = "command"  // a command line the model wants to run
	ItemAgent    = "agent"    // start a configured agent
	ItemOptimize = "optimize" // a self-optimisation suggestion
)

// Session is one model interaction and its proposed changes. Every
// session is stored, so its proposals, decisions and results stay
// traceable (NFR-027).
type Session struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Mode     Mode     `json:"mode"`
	Input    string   `json:"input,omitempty"`
	Selected []string `json:"selected,omitempty"`
	// Questions are asked once at most (FR-303); Answers hold the reply.
	Questions []string `json:"questions,omitempty"`
	Answers   string   `json:"answers,omitempty"`
	Asked     bool     `json:"asked"`
	Feedback  string   `json:"feedback,omitempty"`

	Summary         string           `json:"summary,omitempty"`
	Risks           []Risk           `json:"risks,omitempty"`
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	Items           []Item           `json:"items"`
	Warnings        []string         `json:"warnings,omitempty"`
	// Degraded means the model was unavailable and local rules answered.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`

	Result     *Result `json:"result,omitempty"`
	Operations []int64 `json:"operations,omitempty"`
	// Refs maps the short references shown to the model (T1…) to task ids.
	Refs          map[string]string `json:"refs,omitempty"`
	Profile       string            `json:"profile,omitempty"`
	Model         string            `json:"model,omitempty"`
	ConfigVersion int               `json:"config_version,omitempty"`
	Supersedes    string            `json:"supersedes,omitempty"`
	SupersededBy  string            `json:"superseded_by,omitempty"`
	Origin        string            `json:"origin,omitempty"` // cli, web, tui …
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// Risk is a decision risk hint.
type Risk struct {
	TaskID  string `json:"task_id,omitempty"`
	Title   string `json:"title,omitempty"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Recommendation is one entry of the suggested order (FR-402).
type Recommendation struct {
	Order    int    `json:"order"`
	TaskID   string `json:"task_id"`
	Title    string `json:"title"`
	Reason   string `json:"reason"`
	Focus    string `json:"focus,omitempty"`
	Estimate string `json:"estimate,omitempty"`
}

// Item is one proposed change.
type Item struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Ref names a task created by this session (N1…) so other items can
	// depend on it or use it as parent.
	Ref       string      `json:"ref,omitempty"`
	TaskID    string      `json:"task_id,omitempty"`
	TaskTitle string      `json:"task_title,omitempty"`
	ParentRef string      `json:"parent_ref,omitempty"`
	Fields    *TaskFields `json:"fields,omitempty"`
	Change    *Change     `json:"change,omitempty"`
	Command   *Command    `json:"command,omitempty"`
	Optimize  *Suggestion `json:"optimize,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	// Diff shows the value before and after for review (FR-404).
	Diff []Diff `json:"diff,omitempty"`
	// NeedsConfirm and Categories explain why an item was not executed
	// automatically.
	NeedsConfirm  bool     `json:"needs_confirm,omitempty"`
	ConfirmReason string   `json:"confirm_reason,omitempty"`
	Categories    []string `json:"categories,omitempty"`
	Message       string   `json:"message,omitempty"`
	ResultTaskID  string   `json:"result_task_id,omitempty"`
	ActionID      int64    `json:"action_id,omitempty"`
	ConfigVersion int      `json:"config_version,omitempty"`
	Operation     int64    `json:"operation,omitempty"`
	AppliedBy     string   `json:"applied_by,omitempty"` // auto | user
}

// TaskFields are the task fields of a create/update item. Nil means
// unchanged; Due is RFC 3339 ("" clears it).
type TaskFields struct {
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
	Due         *string   `json:"due,omitempty"`
	DueText     string    `json:"due_text,omitempty"`
	Priority    *string   `json:"priority,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	Category    *string   `json:"category,omitempty"`
	// DependsOn holds task ids or session refs (N1).
	DependsOn *[]string `json:"depends_on,omitempty"`
}

// Change is a decision change of one task field (FR-403).
type Change struct {
	Field string `json:"field"` // priority | due_at | status | depends_on | order
	To    any    `json:"to"`
}

// Command is a model-proposed command line or agent start (FR-603).
type Command struct {
	Argv  []string `json:"argv,omitempty"`
	Agent string   `json:"agent,omitempty"`
	Dir   string   `json:"dir,omitempty"`
	// Prompt is what an agent receives on standard input, shown for
	// review before it starts (FR-506).
	Prompt string         `json:"prompt,omitempty"`
	Result *CommandResult `json:"result,omitempty"`
	// RunID is the agent run started for this item (packages/agent).
	RunID string `json:"run_id,omitempty"`
}

// Diff is one field before and after.
type Diff struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// Result summarises what happened (FR-306).
type Result struct {
	Created     int      `json:"created"`
	Updated     int      `json:"updated"`
	NotExecuted int      `json:"not_executed"`
	Failed      int      `json:"failed"`
	Messages    []string `json:"messages"`
}

func (s *Session) item(n int) *Item {
	for i := range s.Items {
		if s.Items[i].N == n {
			return &s.Items[i]
		}
	}
	return nil
}

// refreshStatus derives the session state from its items.
func (s *Session) refreshStatus() {
	if s.Status == StatusNeedsInput || s.Status == StatusSuperseded || s.Status == StatusUndone {
		return
	}
	pending, applied, rejected := 0, 0, 0
	for _, it := range s.Items {
		switch it.Status {
		case ItemPending:
			pending++
		case ItemApplied:
			applied++
		case ItemRejected:
			rejected++
		}
	}
	switch {
	case pending > 0 && applied > 0:
		s.Status = StatusPartial
	case pending > 0:
		s.Status = StatusPending
	case applied > 0:
		s.Status = StatusApplied
	case rejected > 0:
		s.Status = StatusRejected
	default:
		s.Status = StatusApplied
	}
}

// tally counts the items into a result (FR-306).
func (s *Session) tally() *Result {
	r := &Result{Messages: []string{}}
	for _, it := range s.Items {
		switch it.Status {
		case ItemApplied:
			if it.Kind == ItemCreate || it.Kind == ItemSubtask {
				r.Created++
			} else {
				r.Updated++
			}
		case ItemFailed:
			r.Failed++
		default:
			r.NotExecuted++
		}
		if it.Message != "" {
			r.Messages = append(r.Messages, itemLabel(&it)+"："+it.Message)
		}
	}
	return r
}

func itemLabel(it *Item) string {
	title := it.TaskTitle
	if it.Fields != nil && it.Fields.Title != nil {
		title = *it.Fields.Title
	}
	if title == "" && it.Optimize != nil {
		title = it.Optimize.Target
	}
	if title == "" && it.Command != nil {
		if it.Command.Agent != "" {
			title = "agent " + it.Command.Agent
		} else if len(it.Command.Argv) > 0 {
			title = it.Command.Argv[0]
		}
	}
	return "#" + itoa(it.N) + " " + title
}
