package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/tui"
)

// Environment switches for the interactive UI.
const (
	EnvKeymap  = "TODO_CLI_KEYMAP" // custom key binding file
	EnvMouse   = "TODO_CLI_MOUSE"  // "0"/"off" disables mouse reporting
	EnvNoColor = "NO_COLOR"        // https://no-color.org
)

// isTerminal reports whether both stdin and stdout are interactive terminals.
func (a *app) isTerminal() bool {
	in, ok1 := a.env.Stdin.(*os.File)
	out, ok2 := a.env.Stdout.(*os.File)
	return ok1 && ok2 && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func (a *app) keymapPath() (string, error) {
	if p := a.env.Getenv(EnvKeymap); p != "" {
		return p, nil
	}
	dir, err := a.resolveDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tui.KeymapFileName), nil
}

func cmdTUI(a *app, args []string) error {
	fs := newFlags("tui")
	noMouse := fs.Bool("no-mouse", false, "disable mouse reporting (keyboard only)")
	noColor := fs.Bool("no-color", false, "disable colours (also NO_COLOR=1)")
	keymap := fs.String("keymap", "", "custom key binding file (default $"+EnvKeymap+" or <data-dir>/"+tui.KeymapFileName+")")
	rest, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("tui takes no arguments")
	}
	if a.json {
		return usagef("the interactive UI has no JSON mode; use the non-interactive commands with --json")
	}
	in, inOK := a.env.Stdin.(*os.File)
	out, outOK := a.env.Stdout.(*os.File)
	if !inOK || !outOK || !a.isTerminal() {
		return usagef("the interactive UI needs a terminal on stdin and stdout; in scripts use the non-interactive commands (todo help)")
	}
	path := *keymap
	if path == "" {
		if path, err = a.keymapPath(); err != nil {
			return err
		}
	}
	km, kerr := tui.LoadKeymap(path)
	warning := ""
	if kerr != nil {
		warning = "快捷键配置有误，已使用默认快捷键：" + kerr.Error()
	}
	if a.actor == "" {
		a.actor = "tui"
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	mouse := !*noMouse
	switch strings.ToLower(a.env.Getenv(EnvMouse)) {
	case "0", "off", "false", "no":
		mouse = false
	}
	color := !*noColor && a.env.Getenv(EnvNoColor) == ""
	opts := tui.Options{
		Store: s, Keymap: km, Warning: warning, Mouse: mouse, Color: color, Now: a.env.Now,
		ParseDue: parseDue,
	}
	err = tui.Run(opts, in, out)
	if errors.Is(err, tui.ErrNotTerminal) {
		return usagef("%v", err)
	}
	return err
}

func cmdKeys(a *app, args []string) error {
	fs := newFlags("keys")
	initFile := fs.Bool("init", false, "write the default bindings to the key binding file as a template")
	force := fs.Bool("force", false, "with --init, overwrite an existing file")
	keymap := fs.String("keymap", "", "key binding file (default $"+EnvKeymap+" or <data-dir>/"+tui.KeymapFileName+")")
	rest, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("keys takes no arguments")
	}
	path := *keymap
	if path == "" {
		if path, err = a.keymapPath(); err != nil {
			return err
		}
	}
	if *initFile {
		if _, err := os.Stat(path); err == nil && !*force {
			return usagef("%s already exists (use --force to overwrite)", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, tui.DefaultKeymapJSON(), 0o600); err != nil {
			return err
		}
		if a.json {
			return a.emitJSON(map[string]any{"path": path, "written": true})
		}
		a.printf("Wrote default key bindings to %s\nEdit it, then restart `todo tui`. Listed actions replace their default keys.\n", path)
		return nil
	}
	km, kerr := tui.LoadKeymap(path)
	if a.json {
		out := map[string]any{"path": path, "bindings": km.Bindings(), "custom": km.Custom}
		if out["custom"] == nil || len(km.Custom) == 0 {
			out["custom"] = []string{}
		}
		if kerr != nil {
			out["error"] = kerr.Error()
		}
		if err := a.emitJSON(out); err != nil {
			return err
		}
	} else {
		a.printf("Key bindings for `todo tui` (file: %s)\n", path)
		group := ""
		for _, b := range km.Bindings() {
			if b.Group != group {
				group = b.Group
				a.printf("\n%s\n", group)
			}
			keys := strings.Join(b.Keys, ", ")
			if keys == "" {
				keys = "(unbound)"
			}
			mark := " "
			if slices.Contains(km.Custom, b.Action) {
				mark = "*"
			}
			a.printf(" %s %-16s %-22s %s\n", mark, b.Action, keys, b.Label)
		}
		a.printf("\n* = customised. Mouse: click selects, double-click opens details, right-click opens the action menu, wheel scrolls.\n")
		a.printf("Customise: todo keys --init, then edit the file, e.g. {\"new\": [\"ctrl+n\", \"a\"], \"delete\": \"D\"}.\n")
	}
	if kerr != nil {
		if !a.json {
			fmt.Fprintf(a.env.Stderr, "todo: invalid key bindings, defaults shown: %v\n", kerr)
		}
		return statusFailed{}
	}
	return nil
}

// --- shell completion -------------------------------------------------------------

const bashCompletion = `# bash completion for todo — add to ~/.bashrc:  eval "$(todo completion bash)"
_todo_complete() {
  local IFS=$'\n'
  COMPREPLY=($(todo __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null | cut -f1))
}
complete -o default -F _todo_complete todo
`

const zshCompletion = `#compdef todo
# zsh completion for todo — add to ~/.zshrc:  eval "$(todo completion zsh)"
_todo() {
  local -a items
  local line value desc
  for line in "${(@f)$(todo __complete "${(@)words[2,CURRENT]}" 2>/dev/null)}"; do
    [[ -z "$line" ]] && continue
    value="${line%%$'\t'*}"
    desc="${line#*$'\t'}"
    [[ "$desc" == "$line" ]] && desc=""
    items+=("${value//:/\\:}:${desc}")
  done
  _describe -t todo 'todo' items
}
if (( $+functions[compdef] )); then compdef _todo todo; fi
`

const fishCompletion = `# fish completion for todo — save as ~/.config/fish/completions/todo.fish
complete -c todo -f -a '(todo __complete (commandline -opc)[2..-1] (commandline -ct))'
`

func cmdCompletion(a *app, args []string) error {
	if len(args) != 1 {
		return usagef("usage: todo completion bash|zsh|fish")
	}
	switch args[0] {
	case "bash":
		a.printf("%s", bashCompletion)
	case "zsh":
		a.printf("%s", zshCompletion)
	case "fish":
		a.printf("%s", fishCompletion)
	default:
		return usagef("unsupported shell %q (want bash, zsh or fish)", args[0])
	}
	return nil
}

var flagLine = regexp.MustCompile(`^\s+-(\S+)`)

// commandFlags discovers a command's flags from its -h output.
func commandFlags(c *command) []string {
	var out bytes.Buffer
	sub := &app{env: Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }, Now: time.Now}}
	_ = c.run(sub, []string{"-h"})
	var flags []string
	for _, line := range strings.Split(out.String(), "\n") {
		if m := flagLine.FindStringSubmatch(line); m != nil {
			flags = append(flags, "--"+m[1])
		}
	}
	return append(flags, "--json", "--data-dir")
}

var idCommands = []string{"show", "edit", "done", "start", "reopen", "archive", "delete", "restore", "history", "priority", "move"}

var flagValues = map[string][]string{
	"--priority": {"none", "low", "medium", "high", "urgent"}, "-p": {"none", "low", "medium", "high", "urgent"},
	"--status": {"todo", "in_progress", "done", "archived"}, "-s": {"todo", "in_progress", "done", "archived"},
	"--sort":       {"manual", "due", "priority", "created", "updated"},
	"--due":        {"today", "tomorrow", "+1d", "+3d", "+1w", "mon", "fri", "next mon"},
	"--due-before": {"today", "tomorrow", "+1w", "fri"}, "--due-after": {"today", "tomorrow", "+1w"},
}

// cmdComplete prints completion candidates ("value\tdescription" per line)
// for the words typed so far; the last word is the one being completed.
func cmdComplete(a *app, words []string) error {
	cur := ""
	if len(words) > 0 {
		cur, words = words[len(words)-1], words[:len(words)-1]
	}
	// Skip global flags so the command word is found.
	var plain []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--data-dir" || w == "--actor" {
			i++
			continue
		}
		if !strings.HasPrefix(w, "-") || len(plain) > 0 {
			plain = append(plain, w)
		}
	}
	emit := func(value, desc string) {
		if strings.HasPrefix(value, cur) {
			if desc != "" {
				a.printf("%s\t%s\n", value, desc)
			} else {
				a.printf("%s\n", value)
			}
		}
	}
	if len(plain) == 0 {
		for _, c := range commands {
			emit(c.name, c.summary)
		}
		return nil
	}
	c := findCommand(plain[0])
	if c == nil {
		return nil
	}
	prev := ""
	if len(words) > 0 {
		prev = words[len(words)-1]
	}
	if vals, ok := flagValues[prev]; ok {
		for _, v := range vals {
			emit(v, "")
		}
		return nil
	}
	if strings.HasPrefix(cur, "-") {
		for _, f := range commandFlags(c) {
			emit(f, "")
		}
		return nil
	}
	switch c.name {
	case "help":
		for _, c := range commands {
			emit(c.name, c.summary)
		}
		return nil
	case "completion":
		for _, sh := range []string{"bash", "zsh", "fish"} {
			emit(sh, "")
		}
		return nil
	case "batch":
		if len(plain) == 1 {
			for _, v := range []string{"done", "start", "reopen", "archive", "delete", "restore", "priority", "move"} {
				emit(v, "")
			}
			return nil
		}
	case "priority":
		if len(plain) == 1 {
			for _, v := range flagValues["--priority"] {
				emit(v, "")
			}
			return nil
		}
	}
	if !slices.Contains(idCommands, c.name) && c.name != "batch" {
		return nil
	}
	s, err := a.open()
	if err != nil {
		return nil
	}
	f := core.Filter{IncludeArchived: true, Limit: 200}
	if c.name == "restore" {
		f = core.Filter{OnlyDeleted: true, Limit: 200}
	}
	tasks, err := s.List(f)
	if err != nil {
		return nil
	}
	for _, t := range tasks {
		emit(shortID(t.ID), t.Title)
	}
	return nil
}

// suggestCommand returns a "did you mean" hint for a mistyped command.
func suggestCommand(name string) string {
	best, bestD := "", 3
	for _, c := range commands {
		for _, n := range append([]string{c.name}, c.aliases...) {
			if d := tui.EditDistance(name, n); d < bestD {
				best, bestD = c.name, d
			}
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; did you mean %q?", best)
}
