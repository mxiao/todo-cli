package server

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/apps/web"
	"github.com/mxiao/todo-cli/packages/core"
)

// The web page's REST client (apps/web/js/api.js) only calls endpoints this
// server routes with that method. The Playwright tests mock the model and
// agent endpoints (e2e/web/ai-mock.mjs), so this keeps the page, its mock
// and the real API on the same paths.
func TestWebClientCallsRealRoutes(t *testing.T) {
	st, err := core.Open(core.Options{DataDir: t.TempDir(), Actor: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(st, Options{PollInterval: 50 * time.Millisecond, Version: "test"})
	defer s.Close()

	src, err := fs.ReadFile(web.Assets, "js/api.js")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile("request\\('(GET|POST|PUT|PATCH|DELETE)',\\s*['`](/api/[^'`?]*)").FindAllStringSubmatch(string(src), -1)
	if len(calls) < 50 {
		t.Fatalf("found only %d API calls in api.js", len(calls))
	}
	param := regexp.MustCompile(`\$\{[^}]+\}`)
	seen := map[string]bool{}
	for _, c := range calls {
		method, path := c[1], param.ReplaceAllString(c[2], "x1")
		r := httptest.NewRequest(method, path, nil)
		_, pattern := s.mux.Handler(r)
		if !strings.HasPrefix(pattern, method+" /api/") {
			t.Errorf("api.js calls %s %s, which the server does not route (matched %q)", method, c[2], pattern)
		}
		seen[pattern] = true
	}
	// Every model, prompt and agent route has a caller in the page, except
	// the ones the page deliberately leaves to the CLI.
	cliOnly := map[string]bool{
		"GET /api/llm/calls": true, "GET /api/llm/config": true, "GET /api/llm/config/versions": true,
		"POST /api/llm/config/rollback": true, "POST /api/llm/config/versions/{v}/restore": true,
		"POST /api/prompts": true, "PUT /api/prompts/{pid}": true, "DELETE /api/prompts/{pid}": true,
		"GET /api/prompts/{pid}/export": true, "POST /api/tasks/{id}/results": true,
	}
	for _, p := range routePatterns(t, "llm_routes.go", "agent_routes.go") {
		if !seen[p] && !cliOnly[p] {
			t.Errorf("route %s has no caller in api.js (add one or list it as CLI-only)", p)
		}
	}
}

// routePatterns reads the route patterns registered in the given files,
// including the agent run controls registered in a loop over verbs.
func routePatterns(t *testing.T, files ...string) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`HandleFunc\("([A-Z]+ /api/[^"]+)"[,)]`).FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1])
		}
		loop := regexp.MustCompile(`range \[\]string\{([^}]*)\}[^\n]*\n\s*m\.HandleFunc\("([A-Z]+ /api/[^"]+/)"\+verb`).FindStringSubmatch(string(b))
		if loop != nil {
			for _, v := range regexp.MustCompile(`"(\w+)"`).FindAllStringSubmatch(loop[1], -1) {
				out = append(out, loop[2]+v[1])
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("found only %d routes in %v", len(out), files)
	}
	return out
}
