package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Actor is the history actor of model-initiated task changes (FR-307).
const Actor = "llm"

// Options configures a Service.
type Options struct {
	// Getenv reads model configuration overrides and keys; nil = os.Getenv.
	Getenv func(string) string
	// Secrets is the keychain; nil = the macOS keychain.
	Secrets SecretStore
	// Runner executes model-initiated commands; nil = real processes.
	Runner Runner
	// NewClient builds the model client for a profile; tests return a mock.
	NewClient func(*Resolved) (Client, error)
	// Now is the local clock used to read relative dates ("明天").
	Now func() time.Time
	// Origin labels sessions with the entry point (cli, web).
	Origin string
	// Profile overrides the active profile for every call.
	Profile string
}

// Service runs the model features over one task store.
type Service struct {
	store *core.Store
	llm   *core.Store // same database, history actor "llm"
	opts  Options

	mu       sync.Mutex
	keys     []string // known secrets, hidden from everything stored
	keysSet  bool
	launcher AgentLauncher
}

// Open prepares the llm tables in the store's database.
func Open(store *core.Store, opts Options) (*Service, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Secrets == nil {
		opts.Secrets = NewKeychain()
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	if opts.NewClient == nil {
		opts.NewClient = NewClient
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := store.MigrateModule("llm", moduleMigrations); err != nil {
		return nil, err
	}
	return &Service{store: store, llm: store.WithActor(Actor), opts: opts}, nil
}

// Store is the task store the service works on.
func (s *Service) Store() *core.Store { return s.store }

func (s *Service) now() time.Time { return s.opts.Now() }

// Settings loads the settings; $TODO_CLI_LLM_MODE overrides the mode.
func (s *Service) Settings() (Settings, error) {
	st, err := LoadSettings(s.store.DataDir())
	if err != nil {
		return st, err
	}
	if v := s.opts.Getenv(EnvMode); v != "" {
		m, err := ParseMode(v)
		if err != nil {
			return st, fmt.Errorf("%s: %w", EnvMode, err)
		}
		st.Mode = m
	}
	return st, nil
}

// SaveSettings validates and stores the settings file.
func (s *Service) SaveSettings(st Settings) error {
	s.mu.Lock()
	s.keysSet = false
	s.mu.Unlock()
	return st.Save(s.store.DataDir())
}

// UpdateSettings loads, changes and saves the settings file.
func (s *Service) UpdateSettings(fn func(*Settings) error) (Settings, error) {
	st, err := LoadSettings(s.store.DataDir())
	if err != nil {
		return st, err
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	return st, s.SaveSettings(st)
}

// Resolve resolves a profile ("" = override or active profile).
func (s *Service) Resolve(profile string) (*Resolved, error) {
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	if profile == "" {
		profile = s.opts.Profile
	}
	r, err := Resolve(st, profile, s.opts.Getenv, s.opts.Secrets)
	if err == nil && r.APIKey != "" {
		s.mu.Lock()
		if !slices.Contains(s.keys, r.APIKey) {
			s.keys = append(s.keys, r.APIKey)
		}
		s.mu.Unlock()
	}
	return r, err
}

// Secrets returns the known secret values so callers can hide them too.
func (s *Service) secrets() []string {
	s.mu.Lock()
	set := s.keysSet
	s.mu.Unlock()
	if !set {
		s.mu.Lock()
		s.keysSet = true
		s.mu.Unlock()
		if v := s.opts.Getenv(EnvAPIKey); v != "" {
			s.addKey(v)
		}
		if st, err := s.Settings(); err == nil {
			for _, p := range st.Profiles {
				if p.APIKeyEnv != "" {
					if v := s.opts.Getenv(p.APIKeyEnv); v != "" {
						s.addKey(v)
					}
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func (s *Service) addKey(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.keys, k) {
		s.keys = append(s.keys, k)
	}
}

// Redact hides known keys plus the generic secret patterns.
func (s *Service) Redact(text string) string { return Redact(text, s.secrets()...) }

// StatusReport describes the model configuration (FR-001, FR-704).
type StatusReport struct {
	Model          Public   `json:"model"`
	Available      bool     `json:"available"`
	Mode           Mode     `json:"mode"`
	ModeLabel      string   `json:"mode_label"`
	Profiles       []Public `json:"profiles"`
	ConfirmList    []string `json:"confirm_list"`
	DefaultConfirm []string `json:"default_confirm"`
	SendFields     []string `json:"send_fields"`
	DecisionFields []string `json:"decision_fields"`
	AllowCommands  bool     `json:"allow_commands"`
	Agents         []string `json:"agents"`
	ConfigVersion  int      `json:"assist_config_version"`
	Error          string   `json:"error,omitempty"`
}

// Status reports the active profile and policy without any secret.
func (s *Service) Status() StatusReport {
	rep := StatusReport{DefaultConfirm: DefaultConfirm, Profiles: []Public{}, Agents: []string{}}
	st, err := s.Settings()
	if err != nil {
		rep.Error = err.Error()
		rep.Mode, rep.ModeLabel = ModeConfirm, ModeConfirm.Label()
		return rep
	}
	rep.Mode, rep.ModeLabel = st.Mode, st.Mode.Label()
	rep.ConfirmList, rep.SendFields, rep.DecisionFields, rep.AllowCommands = st.ConfirmList, st.SendFields, st.DecisionFields, st.AllowCommands
	for _, a := range st.Agents {
		rep.Agents = append(rep.Agents, a.Name)
	}
	if r, err := s.Resolve(""); err == nil {
		rep.Model = r.Public()
		rep.Available = r.Configured()
	} else {
		rep.Error = err.Error()
	}
	for _, p := range st.Profiles {
		if r, err := Resolve(st, p.Name, s.opts.Getenv, s.opts.Secrets); err == nil {
			rep.Profiles = append(rep.Profiles, r.Public())
		}
	}
	_, rep.ConfigVersion, _ = s.AssistConfig()
	return rep
}

// ConnectionReport is the result of a connection test (FR-602).
type ConnectionReport struct {
	OK        bool   `json:"ok"`
	Model     Public `json:"model"`
	LatencyMS int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"`
	Error     *Error `json:"error,omitempty"`
	// Alternatives are other profiles to switch to after a failure.
	Alternatives []string `json:"alternatives,omitempty"`
}

// TestConnection sends a tiny request to the profile's service.
func (s *Service) TestConnection(ctx context.Context, profile string) (*ConnectionReport, error) {
	r, err := s.Resolve(profile)
	if err != nil {
		return nil, err
	}
	rep := &ConnectionReport{Model: r.Public()}
	if st, err := s.Settings(); err == nil {
		for _, n := range st.ProfileNames() {
			if n != r.Name {
				rep.Alternatives = append(rep.Alternatives, n)
			}
		}
	}
	var client Client
	if r.Configured() {
		client, err = s.opts.NewClient(r)
	} else {
		err = NotConfigured(r)
	}
	if err == nil {
		start := time.Now()
		var resp *Response
		resp, err = client.Complete(ctx, Request{Purpose: "test", MaxTokens: 5, Messages: []Message{
			{Role: "system", Content: "Connection test. Reply with the single word OK."}, {Role: "user", Content: "ping"}}})
		rep.LatencyMS = time.Since(start).Milliseconds()
		s.recordCall("", "test", r, time.Since(start), err)
		if err == nil {
			rep.OK, rep.Reply = true, truncate(s.Redact(strings.TrimSpace(resp.Content)), 40)
			return rep, nil
		}
	}
	e, ok := AsError(err)
	if !ok {
		e = &Error{Kind: KindUnreachable, Message: err.Error(), Retryable: true}
	}
	e = s.redactError(e)
	e.Profiles = rep.Alternatives
	rep.Error = e
	return rep, nil
}

// client builds the model client of a profile or a not-configured error.
func (s *Service) client(profile string) (Client, *Resolved, error) {
	r, err := s.Resolve(profile)
	if err != nil {
		return nil, nil, err
	}
	if !r.Configured() {
		return nil, r, s.withAlternatives(NotConfigured(r), r.Name)
	}
	c, err := s.opts.NewClient(r)
	if err != nil {
		if e, ok := AsError(err); ok {
			return nil, r, s.withAlternatives(e, r.Name)
		}
		return nil, r, err
	}
	return c, r, nil
}

func (s *Service) withAlternatives(e *Error, current string) *Error {
	if st, err := s.Settings(); err == nil {
		for _, n := range st.ProfileNames() {
			if n != current {
				e.Profiles = append(e.Profiles, n)
			}
		}
	}
	return e
}

// ask sends messages, audits the call and decodes the JSON answer.
func (s *Service) ask(ctx context.Context, c Client, r *Resolved, sessionID, purpose string, msgs []Message, v any) error {
	for i := range msgs {
		msgs[i].Content = s.Redact(msgs[i].Content)
	}
	start := time.Now()
	resp, err := c.Complete(ctx, Request{Purpose: purpose, Messages: msgs, JSON: true})
	if err == nil {
		err = decodeJSON(resp.Content, v)
	}
	s.recordCall(sessionID, purpose, r, time.Since(start), err)
	if e, ok := AsError(err); ok {
		return s.withAlternatives(s.redactError(e), r.Name)
	}
	return err
}

// redactError copies a model error with secrets hidden: clients may echo
// keys back in their error messages.
func (s *Service) redactError(e *Error) *Error {
	c := *e
	c.Message, c.Hint = s.Redact(c.Message), s.Redact(c.Hint)
	c.Profiles = slices.Clone(e.Profiles)
	return &c
}

// taskContext renders tasks for the model with only the allowed fields,
// short references instead of ids, and secrets removed (NFR-032).
func (s *Service) taskContext(tasks []core.Task, fields []string, refOf map[string]string, selected []string) []map[string]any {
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		m := map[string]any{"ref": refOf[t.ID]}
		for _, f := range fields {
			switch f {
			case "title":
				m["title"] = s.Redact(truncate(t.Title, 200))
			case "description":
				if t.Description != "" {
					m["description"] = s.Redact(truncate(t.Description, 400))
				}
			case "notes":
				if t.Notes != "" {
					m["notes"] = s.Redact(truncate(t.Notes, 400))
				}
			case "tags":
				if len(t.Tags) > 0 {
					m["tags"] = t.Tags
				}
			case "status":
				m["status"] = string(t.Status)
			case "priority":
				m["priority"] = t.Priority.String()
			case "category":
				if t.Category != "" {
					m["category"] = t.Category
				}
			case "due_at":
				if t.DueAt != nil {
					m["due_at"] = s.localTime(*t.DueAt)
				}
			case "depends_on":
				var refs []string
				for _, d := range t.DependsOn {
					if r, ok := refOf[d]; ok {
						refs = append(refs, r)
					}
				}
				if len(refs) > 0 {
					m["depends_on"] = refs
				}
			case "parent_id":
				if r, ok := refOf[t.ParentID]; ok {
					m["parent"] = r
				}
			}
		}
		if slices.Contains(selected, t.ID) {
			m["selected"] = true
		}
		out = append(out, m)
	}
	return out
}

func (s *Service) localTime(t time.Time) string {
	return t.In(s.now().Location()).Format("2006-01-02 15:04 Mon")
}

func (s *Service) nowLine() string {
	n := s.now()
	return n.Format("2006-01-02 15:04 Mon") + " (" + n.Format("MST -07:00") + ")"
}

// candidates returns open tasks (plus the selected ones) and assigns the
// short references T1… used in prompts.
func (s *Service) candidates(selected []string, limit int, order func([]core.Task) []core.Task) ([]core.Task, map[string]string, map[string]string, error) {
	tasks, err := s.store.List(core.Filter{Statuses: []core.Status{core.StatusTodo, core.StatusInProgress}, Sort: core.SortUpdated})
	if err != nil {
		return nil, nil, nil, err
	}
	if order != nil {
		tasks = order(tasks)
	}
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	for _, id := range selected {
		if !slices.ContainsFunc(tasks, func(t core.Task) bool { return t.ID == id }) {
			t, err := s.store.Get(id)
			if err != nil {
				return nil, nil, nil, err
			}
			tasks = append([]core.Task{*t}, tasks...)
		}
	}
	refOf, refs := map[string]string{}, map[string]string{}
	for i, t := range tasks {
		r := "T" + itoa(i+1)
		refOf[t.ID], refs[r] = r, t.ID
	}
	return tasks, refOf, refs, nil
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

// AskJSON sends one system/user exchange to the profile's model and decodes
// its JSON answer, with the same redaction and auditing as the built-in
// features (used by packages/prompt).
func (s *Service) AskJSON(ctx context.Context, purpose, profile, system, user string, v any) error {
	c, r, err := s.client(profile)
	if err != nil {
		return err
	}
	return s.ask(ctx, c, r, "", purpose, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, v)
}
