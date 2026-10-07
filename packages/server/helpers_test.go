package server_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/server"
)

// testEnv is one running web service over a data directory, reached through
// a real HTTP listener (the Go counterpart of Supertest).
type testEnv struct {
	t      *testing.T
	dir    string
	store  *core.Store
	srv    *server.Server
	ts     *httptest.Server
	client *http.Client
}

func start(t *testing.T, dir string) *testEnv {
	t.Helper()
	st, err := core.Open(core.Options{DataDir: dir, Actor: "web"})
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(st, server.Options{PollInterval: 50 * time.Millisecond, Version: "test"})
	ts := httptest.NewServer(srv)
	e := &testEnv{t: t, dir: dir, store: st, srv: srv, ts: ts, client: &http.Client{Transport: &http.Transport{}}}
	t.Cleanup(e.stop)
	return e
}

// stop shuts the service down; event streams end first so the listener
// can close.
func (e *testEnv) stop() {
	if e.ts == nil {
		return
	}
	e.srv.Close()
	e.ts.Close()
	e.store.Close()
	e.ts = nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var b struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	r.json(t, &b)
	return b.Error.Code
}

// req sends method path with an optional JSON body (string or value) and
// header pairs.
func (e *testEnv) req(client *http.Client, method, path string, body any, hdr ...string) response {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if rd != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
			continue
		}
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := client.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func (e *testEnv) do(method, path string, body any, hdr ...string) response {
	e.t.Helper()
	return e.req(e.client, method, path, body, hdr...)
}

// must sends a request and fails the test unless the status matches.
func (e *testEnv) must(status int, method, path string, body any, hdr ...string) response {
	e.t.Helper()
	r := e.do(method, path, body, hdr...)
	if r.status != status {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
	}
	return r
}

type taskResp struct {
	Task     core.Task `json:"task"`
	Revision int64     `json:"revision"`
}

func (e *testEnv) create(body map[string]any) core.Task {
	e.t.Helper()
	var out taskResp
	e.must(http.StatusCreated, "POST", "/api/tasks", body).json(e.t, &out)
	return out.Task
}

func (e *testEnv) get(id string) core.Task {
	e.t.Helper()
	var out taskResp
	e.must(http.StatusOK, "GET", "/api/tasks/"+id, nil).json(e.t, &out)
	return out.Task
}

type listResp struct {
	Tasks    []core.Task `json:"tasks"`
	Count    int         `json:"count"`
	Revision int64       `json:"revision"`
}

func (e *testEnv) list(query string) []core.Task {
	e.t.Helper()
	var out listResp
	e.must(http.StatusOK, "GET", "/api/tasks"+query, nil).json(e.t, &out)
	return out.Tasks
}

func titles(ts []core.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Title
	}
	return out
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	ID   string
	Name string
	Data string
}

// eventStream reads server-sent events from /api/events.
type eventStream struct {
	resp   *http.Response
	events chan sseEvent
}

func (e *testEnv) events(hdr ...string) *eventStream {
	e.t.Helper()
	r, err := http.NewRequest("GET", e.ts.URL+"/api/events", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &eventStream{resp: resp, events: make(chan sseEvent, 1000)}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Name != "" || ev.Data != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	e.t.Cleanup(func() { resp.Body.Close() })
	return s
}

// next waits up to timeout for an event accepted by match.
func (s *eventStream) next(t *testing.T, timeout time.Duration, match func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatal("event stream closed")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no matching event within %v", timeout)
		}
	}
}

func (s *eventStream) close() { s.resp.Body.Close() }

func taskEvent(t *testing.T, ev sseEvent) server.ChangeEvent {
	t.Helper()
	var ce server.ChangeEvent
	if err := json.Unmarshal([]byte(ev.Data), &ce); err != nil {
		t.Fatalf("event data %q: %v", ev.Data, err)
	}
	return ce
}
