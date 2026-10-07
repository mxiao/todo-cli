package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// syncBuffer is a goroutine-safe output sink for a running `todo serve`.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serve starts `todo serve` in the background and waits until it prints.
func (h *harness) serve(args ...string) (out *syncBuffer, stop func() int) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out = &syncBuffer{}
	errOut := &syncBuffer{}
	env := Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errOut, Ctx: ctx,
		Getenv: func(k string) string { return h.env[k] }}
	done := make(chan int, 1)
	go func() { done <- Run(append([]string{"--data-dir", h.dir, "serve"}, args...), env) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "http://") {
		select {
		case code := <-done:
			h.t.Fatalf("serve exited %d: %s %s", code, out, errOut)
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatal("serve did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	code := 0
	stop = func() int {
		once.Do(func() {
			cancel()
			select {
			case code = <-done:
			case <-time.After(10 * time.Second):
				h.t.Fatal("serve did not stop")
			}
		})
		return code
	}
	h.t.Cleanup(func() { stop() })
	return out, stop
}

func TestServeSharesDataWithCLI(t *testing.T) {
	h := newHarness(t)
	h.add("CLI 先建的任务")
	out, stop := h.serve("--json", "--port", "0")
	var info serveInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &info); err != nil {
		t.Fatalf("serve --json: %v: %s", err, out)
	}
	if !strings.HasPrefix(info.URL, "http://127.0.0.1:") || info.Port == 0 || info.Host != "127.0.0.1" || info.DataDir != h.dir {
		t.Fatalf("serve info: %+v", info)
	}
	api := strings.TrimSuffix(info.URL, "/")

	// Open the event stream like a browser tab would.
	resp, err := http.Get(api + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan map[string]any, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok && name == "task" {
				var m map[string]any
				_ = json.Unmarshal([]byte(v), &m)
				events <- m
			}
		}
		close(events)
	}()

	// The web sees what the CLI created before the server started...
	var list struct {
		Tasks []core.Task `json:"tasks"`
	}
	getJSON(t, api+"/api/tasks", &list)
	if len(list.Tasks) != 1 || list.Tasks[0].Title != "CLI 先建的任务" {
		t.Fatalf("web list: %+v", list)
	}
	// ...and a CLI command run while it is serving shows up on the stream within 2s.
	began := time.Now()
	created := h.add("CLI 新任务", "-p", "high")
	select {
	case ev := <-events:
		if ev["task_id"] != created.ID || ev["actor"] != "cli" || ev["action"] != "create" {
			t.Fatalf("event: %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLI change not pushed within 2s")
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}

	// The CLI sees a web edit immediately, recorded with actor "web".
	body := `{"version": 1, "notes": "网页补充"}`
	req, _ := http.NewRequest("PATCH", api+"/api/tasks/"+created.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(req)
	if err != nil || pr.StatusCode != 200 {
		t.Fatalf("patch: %v %v", pr, err)
	}
	pr.Body.Close()
	var shown taskOut
	h.json(&shown, "show", created.ID)
	if shown.Task.Notes != "网页补充" || shown.Task.Version != 2 || shown.History[len(shown.History)-1].Actor != "web" {
		t.Fatalf("CLI view of web edit: %+v", shown)
	}
	// A CLI edit based on a stale version is refused too (exit code 4).
	if _, _, code := h.run("edit", created.ID, "--title", "x", "--if-version", "1"); code != ExitConflict {
		t.Fatalf("stale CLI edit exit %d", code)
	}

	if code := stop(); code != ExitOK {
		t.Fatalf("serve exit %d", code)
	}
	if _, err := http.Get(api + "/api/health"); err == nil {
		t.Fatal("server still answering after stop")
	}
}

func TestServePortInUseAndLoopbackOnly(t *testing.T) {
	h := newHarness(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	out, stop := h.serve("--port", strconv.Itoa(port))
	if !strings.Contains(out.String(), "is in use") || strings.Contains(out.String(), ":"+strconv.Itoa(port)+"/") {
		t.Fatalf("expected a fallback port:\n%s", out)
	}
	stop()

	for _, args := range [][]string{{"serve", "--host", "0.0.0.0"}, {"serve", "--host", "192.168.1.5"}, {"serve", "--port", "70000"}, {"serve", "extra"}} {
		if _, errOut, code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d %s", args, code, errOut)
		}
	}
	h.env[EnvPort] = "abc"
	if _, _, code := h.run("serve"); code != ExitUsage {
		t.Errorf("bad %s: exit %d", EnvPort, code)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestServeWebUI(t *testing.T) {
	h := newHarness(t)
	page := func() (int, string) {
		t.Helper()
		out, stop := h.serve("--json", "--port", "0")
		defer stop()
		var info serveInfo
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &info); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(info.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := page(); code != 200 || !strings.Contains(body, `src="/js/app.js"`) {
		t.Fatalf("embedded web UI: %d %s", code, body)
	}

	// TODO_CLI_WEB_DIR serves the page from disk.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>dev page</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.env[EnvWebDir] = dir
	if _, body := page(); body != "<p>dev page</p>" {
		t.Fatalf("web dir page: %s", body)
	}

	h.env[EnvWebDir] = filepath.Join(dir, "missing")
	if _, errOut, code := h.run("serve"); code != ExitUsage || !strings.Contains(errOut, EnvWebDir) {
		t.Fatalf("missing web dir: exit %d %s", code, errOut)
	}
}
