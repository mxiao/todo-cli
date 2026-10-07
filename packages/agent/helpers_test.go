package agent

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
)

const modelKey = "sk-model-key-0123456789abcdef"

type fixture struct {
	t     *testing.T
	store *core.Store
	svc   *llm.Service
	m     *Manager
	model *llm.Scripted
	env   map[string]string
	dir   string // agent working directory
}

func mockScript(t *testing.T) string {
	p, err := filepath.Abs("testdata/mock-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := core.Open(core.Options{DataDir: t.TempDir(), Actor: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, store: store, model: llm.NewScripted(), dir: t.TempDir(),
		env: map[string]string{llm.EnvAPIKey: modelKey, "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": t.TempDir(), "OTHER_VAR": "not-passed"}}
	f.svc, err = llm.Open(store, llm.Options{Getenv: func(k string) string { return f.env[k] }, Secrets: &llm.MemorySecrets{},
		NewClient: func(*llm.Resolved) (llm.Client, error) { return f.model, nil }, Origin: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.m, err = Open(store, Options{LLM: f.svc, Getenv: func(k string) string { return f.env[k] }, PollInterval: 10 * time.Millisecond,
		KillGrace: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// mock registers a cli agent running the mock script in a mode.
func (f *fixture) mock(name, mode string, mods ...func(*Spec)) Spec {
	f.t.Helper()
	sp := Spec{Name: name, Command: []string{"/bin/sh", mockScript(f.t), mode}, Dir: f.dir, Description: "mock " + mode}
	for _, m := range mods {
		m(&sp)
	}
	sp, err := f.m.PutAgent(sp)
	if err != nil {
		f.t.Fatal(err)
	}
	return sp
}

func (f *fixture) task(title string) *core.Task {
	f.t.Helper()
	t, err := f.store.Create(core.NewTask{Title: title, Description: "描述 " + title, Tags: []string{"agent"}})
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *fixture) start(req StartRequest) *Run {
	f.t.Helper()
	r, err := f.m.Start(context.Background(), req)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// run starts and executes a run to its end.
func (f *fixture) run(taskID, agent string, mods ...func(*StartRequest)) *Run {
	f.t.Helper()
	req := StartRequest{TaskID: taskID, Agent: agent}
	for _, m := range mods {
		m(&req)
	}
	r := f.start(req)
	if err := f.m.Execute(context.Background(), r.ID); err != nil {
		f.t.Fatal(err)
	}
	return f.get(r.ID)
}

func (f *fixture) get(id string) *Run {
	f.t.Helper()
	r, err := f.m.Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) events(id string, kinds ...string) []Event {
	f.t.Helper()
	ev, err := f.m.Events(id, EventFilter{Kinds: kinds})
	if err != nil {
		f.t.Fatal(err)
	}
	return ev
}

func (f *fixture) log(id string) string {
	var b strings.Builder
	for _, e := range f.events(id) {
		b.WriteString(e.Kind + " " + e.Stream + " " + e.Message + "\n")
	}
	return b.String()
}

// waitFor polls until cond holds.
func (f *fixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) history(taskID string) []string {
	f.t.Helper()
	h, err := f.store.History(taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range h {
		out = append(out, e.Action+"@"+e.Actor)
	}
	return out
}

func resultTypes(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Type)
	}
	slices.Sort(out)
	return out
}

func p[T any](v T) *T { return &v }
