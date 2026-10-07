package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysCommand(t *testing.T) {
	h := newHarness(t)
	out := h.ok("keys")
	for _, want := range []string{"keybindings.json", "toggle_done", "x, space", "完成 / 重新打开", "right-click"} {
		if !strings.Contains(out, want) {
			t.Errorf("keys output missing %q:\n%s", want, out)
		}
	}
	out = h.ok("keys", "--init")
	path := filepath.Join(h.dir, "keybindings.json")
	if !strings.Contains(out, path) {
		t.Errorf("init output: %s", out)
	}
	if _, stderr, code := h.run("keys", "--init"); code != ExitUsage || !strings.Contains(stderr, "already exists") {
		t.Errorf("second init should refuse: %d %s", code, stderr)
	}
	if err := os.WriteFile(path, []byte(`{"new": "ctrl+n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Path     string `json:"path"`
		Custom   []string
		Bindings []struct {
			Action string   `json:"action"`
			Keys   []string `json:"keys"`
		} `json:"bindings"`
	}
	h.json(&v, "keys")
	if v.Path != path || len(v.Custom) != 1 || v.Custom[0] != "new" {
		t.Errorf("keys json: %+v", v)
	}
	for _, b := range v.Bindings {
		if b.Action == "new" && strings.Join(b.Keys, ",") != "ctrl+n" {
			t.Errorf("custom binding not reported: %v", b.Keys)
		}
	}
	if out := h.ok("keys"); !strings.Contains(out, "* new") {
		t.Errorf("custom bindings are marked: %s", out)
	}

	// Invalid configuration: defaults are listed and the error is reported.
	os.WriteFile(path, []byte(`{"nwe": "a"}`), 0o600)
	out, stderr, code := h.run("keys")
	if code != ExitError || !strings.Contains(stderr, `did you mean "new"`) || !strings.Contains(out, "a, n") {
		t.Errorf("invalid keymap: %d %s %s", code, stderr, out)
	}
	// TODO_CLI_KEYMAP points somewhere else.
	other := filepath.Join(t.TempDir(), "k.json")
	h.env[EnvKeymap] = other
	if out := h.ok("keys", "--init"); !strings.Contains(out, other) {
		t.Errorf("env keymap path: %s", out)
	}
}

func TestCompletion(t *testing.T) {
	h := newHarness(t)
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out := h.ok("completion", sh)
		if !strings.Contains(out, "todo __complete") {
			t.Errorf("%s script should call todo __complete:\n%s", sh, out)
		}
	}
	if _, stderr, code := h.run("completion", "tcsh"); code != ExitUsage || !strings.Contains(stderr, "unsupported shell") {
		t.Errorf("unsupported shell: %d %s", code, stderr)
	}
	task := h.add("写周报")

	lines := func(args ...string) []string {
		out := h.ok(append([]string{"__complete"}, args...)...)
		return strings.Split(strings.TrimSpace(out), "\n")
	}
	contains := func(got []string, want string) bool {
		for _, g := range got {
			if g == want || strings.HasPrefix(g, want+"\t") {
				return true
			}
		}
		return false
	}
	if got := lines("ed"); !contains(got, "edit") || contains(got, "add") {
		t.Errorf("command completion: %v", got)
	}
	if got := lines(""); !contains(got, "tui") || !contains(got, "keys") || !contains(got, "list") {
		t.Errorf("all commands: %v", got)
	}
	if got := lines("list", "--so"); !contains(got, "--sort") {
		t.Errorf("flag completion: %v", got)
	}
	if got := lines("list", "--sort", ""); !contains(got, "priority") || !contains(got, "due") {
		t.Errorf("sort values: %v", got)
	}
	if got := lines("add", "x", "-p", "h"); len(got) != 1 || got[0] != "high" {
		t.Errorf("priority values: %v", got)
	}
	if got := lines("done", ""); !contains(got, task.ID[:8]) || !strings.Contains(strings.Join(got, "\n"), "写周报") {
		t.Errorf("task id completion: %v", got)
	}
	if got := lines("priority", ""); !contains(got, "urgent") {
		t.Errorf("priority level first: %v", got)
	}
	if got := lines("help", "st"); !contains(got, "start") || !contains(got, "status") {
		t.Errorf("help completion: %v", got)
	}
	if got := lines("--data-dir", h.dir, "sh"); !contains(got, "show") {
		t.Errorf("global flags are skipped: %v", got)
	}
}

func TestUnknownCommandSuggestion(t *testing.T) {
	h := newHarness(t)
	_, stderr, code := h.run("lsit")
	if code != ExitUsage || !strings.Contains(stderr, `did you mean "list"`) {
		t.Errorf("suggestion: %d %s", code, stderr)
	}
	_, stderr, _ = h.run("frobnicate")
	if strings.Contains(stderr, "did you mean") {
		t.Errorf("no suggestion for unrelated words: %s", stderr)
	}
	if out := h.ok("help"); !strings.Contains(out, "\n  tui (ui, i)") || !strings.Contains(out, "\n  keys") || !strings.Contains(out, "\n  completion") {
		t.Errorf("help should list the interactive commands:\n%s", out)
	}
	if strings.Contains(h.ok("help"), "__complete") {
		t.Errorf("__complete is hidden")
	}
	if out := h.ok("tui", "-h"); !strings.Contains(out, "-no-mouse") || !strings.Contains(out, "-keymap") {
		t.Errorf("tui -h: %s", out)
	}
	var v map[string]any
	_, stderr, code = h.run("--json", "tui")
	if code != ExitUsage || json.Unmarshal([]byte(stderr), &v) != nil {
		t.Errorf("tui --json: %d %s", code, stderr)
	}
}
