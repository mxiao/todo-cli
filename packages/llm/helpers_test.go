package llm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Tuesday 2026-10-06 10:00 in UTC+8.
var cst = time.FixedZone("CST", 8*3600)
var testNow = time.Date(2026, 10, 6, 10, 0, 0, 0, cst)

const testKey = "sk-test-secret-0123456789"

type fakeRunner struct {
	mu    sync.Mutex
	specs []CommandSpec
	out   CommandResult
}

func (f *fakeRunner) Run(_ context.Context, spec CommandSpec) CommandResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	return f.out
}

type fixture struct {
	t       *testing.T
	svc     *Service
	store   *core.Store
	model   *Scripted
	env     map[string]string
	secrets *MemorySecrets
	runner  *fakeRunner
	// clients counts NewClient calls per profile.
	clients map[string]int
	// failNewClient makes NewClient fail for these profiles.
	perProfile map[string]Client
}

func newFixture(t *testing.T, responses ...any) *fixture {
	t.Helper()
	tick := 0
	store, err := core.Open(core.Options{DataDir: t.TempDir(), Now: func() time.Time {
		tick++
		return testNow.Add(time.Duration(tick) * time.Millisecond)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, store: store, model: NewScripted(responses...), env: map[string]string{EnvAPIKey: testKey},
		secrets: &MemorySecrets{}, runner: &fakeRunner{}, clients: map[string]int{}, perProfile: map[string]Client{}}
	f.svc, err = Open(store, Options{
		Getenv:  func(k string) string { return f.env[k] },
		Secrets: f.secrets,
		Runner:  f.runner,
		NewClient: func(r *Resolved) (Client, error) {
			f.clients[r.Name]++
			if c, ok := f.perProfile[r.Name]; ok {
				return c, nil
			}
			return f.model, nil
		},
		Now:    func() time.Time { return testNow },
		Origin: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) task(title string, mods ...func(*core.NewTask)) *core.Task {
	f.t.Helper()
	in := core.NewTask{Title: title}
	for _, m := range mods {
		m(&in)
	}
	t, err := f.store.Create(in)
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fixture) get(id string) *core.Task {
	f.t.Helper()
	t, err := f.store.Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fixture) open() []core.Task {
	f.t.Helper()
	ts, err := f.store.List(core.Filter{Sort: core.SortManual})
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

func (f *fixture) setMode(m Mode) {
	f.t.Helper()
	if _, err := f.svc.UpdateSettings(func(s *Settings) error { s.Mode = m; return nil }); err != nil {
		f.t.Fatal(err)
	}
}

// lastPrompt is everything the model received in its last request.
func (f *fixture) lastPrompt() string {
	var b strings.Builder
	for _, m := range f.model.LastRequest().Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func must[T any](t *testing.T) func(v T, err error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func titles(ts []core.Task) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func p[T any](v T) *T { return &v }

func lastHistory(t *testing.T, s *core.Store, id string) core.HistoryEntry {
	t.Helper()
	h, err := s.History(id)
	if err != nil || len(h) == 0 {
		t.Fatalf("history of %s: %v %v", id, h, err)
	}
	return h[len(h)-1]
}

// refOf finds the short reference (T1…) the model was given for a task.
func refOf(t *testing.T, req Request, title string) string {
	t.Helper()
	var user struct {
		Tasks []struct {
			Ref   string `json:"ref"`
			Title string `json:"title"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &user); err != nil {
		t.Fatalf("user message is not JSON: %v", err)
	}
	for _, tk := range user.Tasks {
		if tk.Title == title {
			return tk.Ref
		}
	}
	t.Fatalf("task %q was not sent to the model", title)
	return ""
}
