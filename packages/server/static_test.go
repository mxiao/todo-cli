package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/server"
)

func TestWebUIIsServed(t *testing.T) {
	e := start(t, t.TempDir())
	r := e.must(200, "GET", "/", nil)
	body := string(r.body)
	for _, want := range []string{`lang="zh-CN"`, `src="/js/app.js"`, `href="/styles.css"`, `data-testid="task-list"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	csp := r.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP: %q", csp)
	}
	if r.header.Get("X-Frame-Options") != "DENY" || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers: %v", r.header)
	}
	if r.header.Get("Cache-Control") != "no-cache" || r.header.Get("ETag") == "" {
		t.Errorf("caching headers: %v", r.header)
	}

	for path, ctype := range map[string]string{
		"/js/app.js":   "text/javascript",
		"/js/api.js":   "text/javascript",
		"/js/model.js": "text/javascript",
		"/styles.css":  "text/css",
		"/index.html":  "text/html",
	} {
		r := e.must(200, "GET", path, nil)
		if !strings.HasPrefix(r.header.Get("Content-Type"), ctype) || len(r.body) == 0 {
			t.Errorf("%s: %q, %d bytes", path, r.header.Get("Content-Type"), len(r.body))
		}
		// Conditional requests revalidate cheaply.
		if c := e.do("GET", path, nil, "If-None-Match", r.header.Get("ETag")); c.status != http.StatusNotModified || len(c.body) != 0 {
			t.Errorf("%s revalidation: %d", path, c.status)
		}
	}
	if r := e.do("HEAD", "/js/app.js", nil); r.status != 200 || len(r.body) != 0 {
		t.Errorf("HEAD: %d %d bytes", r.status, len(r.body))
	}

	for _, path := range []string{"/nope.js", "/js/", "/js", "/embed.go", "/../go.mod", "/%2e%2e/go.mod", "/js/../../go.mod"} {
		if r := e.do("GET", path, nil); r.status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, r.status)
		}
	}
	if r := e.do("POST", "/", "{}"); r.status != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", r.status)
	}
	// The page is only served to loopback hosts, like the API.
	if r := e.do("GET", "/", nil, "Host", "evil.example:3210"); r.status != http.StatusForbidden {
		t.Errorf("foreign Host: %d", r.status)
	}
	// API routes keep precedence over static files.
	if r := e.do("GET", "/api/nope", nil); r.status != 404 || r.errorCode(t) != "not_found" {
		t.Errorf("unknown API path: %d %s", r.status, r.body)
	}
}

func TestCustomAndMissingAssets(t *testing.T) {
	st, err := core.Open(core.Options{DataDir: t.TempDir(), Actor: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assets := fstest.MapFS{
		"index.html":   {Data: []byte("<!doctype html><title>dev</title>")},
		"js/extra.mjs": {Data: []byte("export {}")},
	}
	srv := server.New(st, server.Options{PollInterval: time.Second, Assets: assets})
	defer srv.Close()
	e := &testEnv{t: t, srv: srv, client: http.DefaultClient}
	e.ts = newTestServer(t, srv)
	if r := e.must(200, "GET", "/", nil); string(r.body) != "<!doctype html><title>dev</title>" {
		t.Errorf("custom index: %s", r.body)
	}
	if r := e.must(200, "GET", "/js/extra.mjs", nil); !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") {
		t.Errorf("mjs type: %v", r.header)
	}

	// Without an index.html the landing page explains the API.
	empty := server.New(st, server.Options{PollInterval: time.Second, Assets: fstest.MapFS{}})
	defer empty.Close()
	e2 := &testEnv{t: t, srv: empty, client: http.DefaultClient}
	e2.ts = newTestServer(t, empty)
	if r := e2.must(200, "GET", "/", nil); !strings.Contains(string(r.body), "/api/tasks") {
		t.Errorf("fallback page: %s", r.body)
	}
}

func TestFacets(t *testing.T) {
	e := start(t, t.TempDir())
	e.create(map[string]any{"title": "a", "tags": []string{"work", "x"}, "category": "office"})
	e.create(map[string]any{"title": "b", "tags": []string{"work"}, "category": "home"})
	gone := e.create(map[string]any{"title": "c", "tags": []string{"old"}, "category": "attic"})
	e.must(200, "DELETE", "/api/tasks/"+gone.ID, nil)
	var f core.Facets
	e.must(200, "GET", "/api/facets", nil).json(t, &f)
	want := core.Facets{
		Tags:       []core.Facet{{Name: "work", Count: 2}, {Name: "x", Count: 1}},
		Categories: []core.Facet{{Name: "home", Count: 1}, {Name: "office", Count: 1}},
	}
	if len(f.Tags) != 2 || f.Tags[0] != want.Tags[0] || f.Tags[1] != want.Tags[1] ||
		len(f.Categories) != 2 || f.Categories[0] != want.Categories[0] || f.Categories[1] != want.Categories[1] {
		t.Fatalf("facets: %+v", f)
	}
}

func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}
