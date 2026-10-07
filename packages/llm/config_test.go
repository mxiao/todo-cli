package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionFailureRetryAndSwitch(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.UpdateSettings(func(s *Settings) error {
		return s.PutProfile(Profile{Name: "backup", Provider: "deepseek", Model: "deepseek-chat"})
	}); err != nil {
		t.Fatal(err)
	}
	var fails atomic.Int32
	fails.Store(2)
	f.perProfile[DefaultProfile] = Func(func(context.Context, Request) (*Response, error) {
		if fails.Add(-1) >= 0 {
			return nil, &Error{Kind: KindUnreachable, Message: "无法连接模型服务：connection refused " + testKey, Retryable: true}
		}
		return &Response{Content: "OK"}, nil
	})
	rep, err := f.svc.TestConnection(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.Error == nil || rep.Error.Kind != KindUnreachable || !rep.Error.Retryable ||
		!slices.Contains(rep.Error.Profiles, "backup") {
		t.Fatalf("failure report %+v %+v", rep, rep.Error)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), testKey) {
		t.Fatalf("report leaks the key: %s", b)
	}
	calls, _ := f.svc.Calls(1)
	if len(calls) != 1 || strings.Contains(calls[0].Error, testKey) || calls[0].Purpose != "test" {
		t.Fatalf("call log %+v", calls)
	}

	// Switching to another profile works right away…
	rep, err = f.svc.TestConnection(ctx, "backup")
	if err != nil || rep.Error == nil && !rep.OK {
		t.Fatalf("backup: %+v %v", rep, err)
	}
	f.model.Push("OK")
	if _, err := f.svc.UpdateSettings(func(s *Settings) error { s.Active = "backup"; return nil }); err != nil {
		t.Fatal(err)
	}
	if rep, _ = f.svc.TestConnection(ctx, ""); !rep.OK || rep.Model.Profile != "backup" ||
		rep.Model.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("after switch %+v", rep)
	}
	// …and retrying the failing one eventually succeeds.
	if _, err := f.svc.UpdateSettings(func(s *Settings) error { s.Active = DefaultProfile; return nil }); err != nil {
		t.Fatal(err)
	}
	f.svc.TestConnection(ctx, "")
	if rep, _ = f.svc.TestConnection(ctx, ""); !rep.OK || rep.Reply != "OK" {
		t.Fatalf("retry %+v", rep)
	}
}

func TestResolveKeySourcesAndStatusHidesSecrets(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.UpdateSettings(func(s *Settings) error {
		s.Profiles[0].Keychain = true
		return s.PutProfile(Profile{Name: "local", Provider: "ollama", Model: "qwen", NoKey: true})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.secrets.Set(DefaultProfile, "sk-from-keychain-0000000"); err != nil {
		t.Fatal(err)
	}
	r := must[*Resolved](t)(f.svc.Resolve(""))
	if r.KeySource != "env:"+EnvAPIKey || r.APIKey != testKey {
		t.Fatalf("environment must win: %+v", r.Public())
	}
	delete(f.env, EnvAPIKey)
	r = must[*Resolved](t)(f.svc.Resolve(""))
	if r.KeySource != "keychain" || r.APIKey != "sk-from-keychain-0000000" || !r.Configured() {
		t.Fatalf("keychain: %+v", r.Public())
	}
	if r = must[*Resolved](t)(f.svc.Resolve("local")); !r.Configured() || r.BaseURL != "http://127.0.0.1:11434/v1" {
		t.Fatalf("no-key profile: %+v", r.Public())
	}
	f.env[EnvModel] = "gpt-4.1"
	if r = must[*Resolved](t)(f.svc.Resolve("")); r.Model != "gpt-4.1" || r.Source != "env" {
		t.Fatalf("env override: %+v", r.Public())
	}
	st := f.svc.Status()
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "sk-from-keychain") || !st.Available || st.Mode != ModeConfirm ||
		len(st.Profiles) != 2 || !slices.Contains(st.DefaultConfirm, CatDelete) {
		t.Fatalf("status %s", b)
	}
	if _, err := f.svc.Resolve("nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown profile: %v", err)
	}
}

func TestSettingsFileValidationAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadSettings(dir)
	if err != nil || s.Mode != ModeConfirm || s.Profiles[0].Model != DefaultModel ||
		!slices.Equal(s.SendFields, []string{"title", "description", "tags", "status", "priority"}) {
		t.Fatalf("defaults %+v %v", s, err)
	}
	if err := s.PutProfile(Profile{Name: "x", Provider: "acme"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider without URL: %v", err)
	}
	if err := s.PutProfile(Profile{Name: "x", BaseURL: "https://u:p@example.com/v1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("credentials in URL: %v", err)
	}
	s.Profiles = s.Profiles[:1]
	if err := s.RemoveProfile(DefaultProfile); err != nil {
		t.Fatal(err)
	}
	s = DefaultSettings()
	s.SendFields = append(s.SendFields, "password")
	if err := s.Save(dir); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown field: %v", err)
	}
	s = DefaultSettings()
	s.Mode = ModeAuto
	if err := s.PutProfile(Profile{Name: "two", Provider: "openrouter", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProfile(DefaultProfile); !errors.Is(err, ErrInvalid) {
		t.Fatalf("removing the active profile: %v", err)
	}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(SettingsPath(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode %v %v", fi.Mode(), err)
	}
	back, err := LoadSettings(dir)
	if err != nil || back.Mode != ModeAuto || len(back.Profiles) != 2 {
		t.Fatalf("reload %+v %v", back, err)
	}
	if m, err := ParseMode("自动执行"); err != nil || m != ModeAuto {
		t.Fatal("chinese mode label")
	}
	if _, err := ParseMode("yolo"); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad mode accepted")
	}
}

func TestModeFromEnvironment(t *testing.T) {
	f := newFixture(t)
	f.env[EnvMode] = "auto"
	if st, err := f.svc.Settings(); err != nil || st.Mode != ModeAuto {
		t.Fatalf("%+v %v", st, err)
	}
	f.env[EnvMode] = "bogus"
	if _, err := f.svc.Settings(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad env mode: %v", err)
	}
}

func TestOpenAIClient(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "bad request path/auth", 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		switch s := int(status.Load()); s {
		case 200:
			if req.Model != "gpt-4o-mini" || req.ResponseFormat["type"] != "json_object" {
				http.Error(w, "bad body", 400)
				return
			}
			w.Write([]byte(`{"model":"gpt-4o-mini","choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"}}]}`))
		case 299:
			w.Write([]byte(`not json`))
		case 503:
			status.Store(200) // recover after one failure
			w.WriteHeader(503)
		default:
			w.WriteHeader(s)
			w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + testKey + `"}}`))
		}
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL + "/v1/", Model: "gpt-4o-mini", APIKey: testKey, HTTP: srv.Client(), MaxRetries: 1, Backoff: time.Millisecond}
	req := Request{JSON: true, Messages: []Message{{Role: "user", Content: "hi"}}}

	resp, err := c.Complete(ctx, req)
	if err != nil || resp.Content != `{"ok":true}` {
		t.Fatalf("%+v %v", resp, err)
	}
	status.Store(503)
	hits.Store(0)
	if _, err := c.Complete(ctx, req); err != nil || hits.Load() != 2 {
		t.Fatalf("retry after 503: %v hits %d", err, hits.Load())
	}
	for code, kind := range map[int32]string{401: KindAuth, 404: KindNotFound, 429: KindRateLimited, 400: KindBadRequest, 299: KindBadResponse} {
		status.Store(code)
		_, err := c.Complete(ctx, req)
		e, ok := AsError(err)
		if !ok || e.Kind != kind || strings.Contains(e.Error(), testKey) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("HTTP %d: %v", code, err)
		}
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	c2 := &OpenAI{BaseURL: slow.URL, Model: "m", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := c2.Complete(ctx, req); !isKind(err, KindTimeout) {
		t.Errorf("timeout: %v", err)
	}
	slow.Close()
	c3 := &OpenAI{BaseURL: slow.URL, Model: "m", HTTP: &http.Client{}}
	if _, err := c3.Complete(ctx, req); !isKind(err, KindUnreachable) {
		t.Errorf("unreachable: %v", err)
	}

	if _, err := NewClient(&Resolved{Status: "not_configured", Problem: "未设置"}); !isKind(err, KindNotConfigured) {
		t.Errorf("not configured: %v", err)
	}
}

func isKind(err error, kind string) bool {
	e, ok := AsError(err)
	return ok && e.Kind == kind
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"key sk-abcdefgh12345678 end":            "key [REDACTED] end",
		"Authorization: Bearer abc.def.ghi12345": "Authorization: Bearer [REDACTED]",
		"api_key=xyz123 other":                   "api_key=[REDACTED] other",
		`password: "hunter2"`:                    "password: [REDACTED]",
		"密码：abc12345":                            "密码：[REDACTED]",
		"https://bob:pw@host/x":                  "https://[REDACTED]@host/x",
		"ghp_" + strings.Repeat("a", 30):         "[REDACTED]",
		"token 5 is fine":                        "token 5 is fine",
		"password reset email":                   "password reset email",
		"literal secretvalue here":               "literal [REDACTED] here",
	} {
		if got := Redact(in, "secretvalue"); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if RedactURL("https://u:p@h.com/v1?key=x#f") != "https://h.com/v1" {
		t.Error("RedactURL")
	}
}

func TestStoredSessionsAndCallsNeverHoldKeys(t *testing.T) {
	f := newFixture(t, map[string]any{"items": []map[string]any{{"action": "create", "title": "用 " + testKey + " 调接口"}}})
	if _, err := f.svc.Intake(ctx, IntakeInput{Text: "用 " + testKey + " 调接口，token=abcdefghijkl"}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.DB().Query(`SELECT data FROM llm_sessions UNION ALL SELECT error FROM llm_calls`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		rows.Scan(&d)
		if strings.Contains(d, testKey) || strings.Contains(d, "abcdefghijkl") {
			t.Fatalf("secret stored: %s", d)
		}
	}
}

func TestPolicy(t *testing.T) {
	cats := func(argv ...string) []string { return Categories(Action{Kind: OpCommand, Argv: argv}) }
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"ls", "-la"}, nil},
		{[]string{"rm", "x"}, []string{CatDelete}},
		{[]string{"git", "push"}, []string{CatCommit, CatSend}},
		{[]string{"git", "commit", "-m", "x"}, []string{CatCommit}},
		{[]string{"curl", "https://x"}, []string{CatSend}},
		{[]string{"defaults", "write", "x"}, []string{CatSystemConfig}},
		{[]string{"bash", "-c", "rm -rf x; echo > f"}, []string{CatDelete, CatOverwrite, CatShell}},
		{[]string{"/bin/cp", "a", "b"}, []string{CatOverwrite}},
	} {
		if got := cats(tc.argv...); !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v want %v", tc.argv, got, tc.want)
		}
	}
	auto := Policy{Mode: ModeAuto, Confirm: []string{"npm test", OpAgentStart, CatShell}}
	check := func(p Policy, a Action) Verdict {
		v, err := p.Check(a)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !check(auto, Action{Kind: OpTaskUpdate}).Execute || !check(auto, Action{Kind: OpCommand, Argv: []string{"npm", "run"}}).Execute {
		t.Error("auto mode must execute authorised operations")
	}
	for _, a := range []Action{{Kind: OpTaskDelete}, {Kind: OpTaskOverwrite}, {Kind: OpCommand, Argv: []string{"npm", "test", "--x"}},
		{Kind: OpAgentStart, Argv: []string{"claude"}}, {Kind: OpCommand, Argv: []string{"sh", "-c", "ls"}}} {
		if check(auto, a).Execute {
			t.Errorf("%+v executed without confirmation", a)
		}
	}
	if check(Policy{Mode: ModeConfirm}, Action{Kind: OpTaskCreate}).Execute || check(Policy{Mode: ModeSuggest}, Action{Kind: OpTaskCreate}).Execute {
		t.Error("confirm/suggest modes must not execute")
	}
	if _, err := auto.Check(Action{Kind: OpCommand, Argv: []string{"env", "sudo", "id"}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("escalation: %v", err)
	}
}

func TestKeychainCommands(t *testing.T) {
	var calls [][]string
	k := &Keychain{Service: "todo-cli", run: func(args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] == "find-generic-password" {
			return "", ErrSecretNotFound
		}
		return "", nil
	}}
	if err := k.Set("default", "sk-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("default"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal(err)
	}
	if !slices.Contains(calls[0], "-U") || !slices.Contains(calls[1], "-w") || calls[1][2] != "todo-cli" {
		t.Fatalf("security args %v", calls)
	}
}

func TestExecRunner(t *testing.T) {
	t.Setenv(EnvAPIKey, "sk-should-not-leak-000")
	res := ExecRunner{}.Run(ctx, CommandSpec{Argv: []string{"sh", "-c", `cat; echo "[$TODO_CLI_MODEL_API_KEY]"; exit 3`},
		Stdin: "from stdin", HideEnv: []string{EnvAPIKey}})
	if res.ExitCode != 3 || !strings.Contains(res.Stdout, "from stdin") || !strings.Contains(res.Stdout, "[]") {
		t.Fatalf("%+v", res)
	}
	res = ExecRunner{}.Run(ctx, CommandSpec{Argv: []string{"sleep", "5"}, Timeout: 100 * time.Millisecond})
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("timeout %+v", res)
	}
	res = ExecRunner{}.Run(ctx, CommandSpec{Argv: []string{"definitely-not-a-program-xyz"}})
	if res.ExitCode != -1 || res.Error == "" {
		t.Fatalf("missing program %+v", res)
	}
}
