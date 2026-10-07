// Package cli implements the non-interactive `todo` command line. Every
// command can print machine-readable JSON with --json.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Version is the todo-cli release version.
const Version = "0.1.0"

// Env carries the process environment so tests can run commands in-process.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Now    func() time.Time
}

// OSEnv returns the real process environment.
func OSEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Now: time.Now}
}

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitConflict = 4
)

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

type app struct {
	env     Env
	json    bool
	dataDir string
	actor   string
	store   *core.Store
}

type command struct {
	name    string
	aliases []string
	args    string
	summary string
	run     func(a *app, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"add", []string{"new", "create"}, "<title> [flags]", "Create a task", cmdAdd},
		{"list", []string{"ls"}, "[filters]", "List tasks with filters and sorting", cmdList},
		{"search", []string{"find"}, "<keywords> [filters]", "Search title, description, notes, tags and category", cmdSearch},
		{"show", []string{"view", "get"}, "<id>", "Show a task with its subtasks and history", cmdShow},
		{"edit", []string{"update"}, "<id> [flags]", "Edit task fields", cmdEdit},
		{"done", []string{"complete"}, "<id>...", "Mark tasks done", statusCmd((*core.Store).Complete, "Completed")},
		{"start", nil, "<id>...", "Mark tasks in progress", statusCmd((*core.Store).Start, "Started")},
		{"reopen", nil, "<id>...", "Reopen done or archived tasks", statusCmd((*core.Store).Reopen, "Reopened")},
		{"archive", nil, "<id>...", "Archive tasks", statusCmd((*core.Store).Archive, "Archived")},
		{"delete", []string{"rm"}, "<id>...", "Delete tasks (soft delete; see restore/undo)", statusCmd((*core.Store).Delete, "Deleted")},
		{"restore", nil, "<id>...", "Restore deleted tasks", statusCmd((*core.Store).Restore, "Restored")},
		{"priority", []string{"prio"}, "<level> <id>...", "Set priority (none|low|medium|high|urgent)", cmdPriority},
		{"move", []string{"mv"}, "<id>... (--category C | --parent P) | <id> (--before X | --after X | --top | --bottom)", "Re-file tasks or change manual order", cmdMove},
		{"batch", nil, "<done|start|reopen|archive|delete|restore|priority|move> ...", "Apply one action to many tasks atomically", cmdBatch},
		{"undo", nil, "", "Undo the most recent change", cmdUndo},
		{"history", []string{"log"}, "<id>", "Show a task's change history", cmdHistory},
		{"export", nil, "[-o file]", "Export all data as JSON", cmdExport},
		{"import", nil, "<file|-> [--replace]", "Import a JSON export", cmdImport},
		{"backup", nil, "", "Write a database backup into the data directory", cmdBackup},
		{"status", nil, "", "Show data directory, model configuration and runtime state", cmdStatus},
		{"tui", []string{"ui", "i"}, "[--no-mouse] [--no-color] [--keymap FILE]", "Interactive full-screen UI (keyboard + mouse); also `todo` with no arguments in a terminal", cmdTUI},
		{"keys", []string{"keybindings"}, "[--init [--force]] [--keymap FILE]", "Show or initialise the interactive UI key bindings", cmdKeys},
		{"completion", nil, "bash|zsh|fish", "Print a shell completion script", cmdCompletion},
		{"version", nil, "", "Print version", cmdVersion},
		{"help", nil, "[command]", "Show help", cmdHelp},
	}
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name || slices.Contains(commands[i].aliases, name) {
			return &commands[i]
		}
	}
	return nil
}

// Run executes one todo invocation and returns the process exit code.
func Run(args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	a := &app{env: env}
	rest, err := a.globalFlags(args)
	if err == nil {
		err = a.dispatch(rest)
	}
	if a.store != nil {
		a.store.Close()
	}
	if err == nil || errors.Is(err, errHelpShown) {
		return ExitOK
	}
	var sf statusFailed
	if errors.As(err, &sf) {
		return ExitError // report already printed
	}
	return a.fail(err)
}

// globalFlags strips --json, --data-dir and --actor from anywhere before "--".
func (a *app) globalFlags(args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			continue
		}
		switch name {
		case "json":
			a.json = true
		case "data-dir", "actor":
			if !hasVal {
				if i+1 >= len(args) {
					return nil, usagef("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			if name == "data-dir" {
				a.dataDir = val
			} else {
				a.actor = val
			}
		default:
			rest = append(rest, arg)
		}
	}
	return rest, nil
}

func (a *app) dispatch(args []string) error {
	if len(args) == 0 {
		if a.isTerminal() && !a.json {
			return cmdTUI(a, nil)
		}
		return cmdHelp(a, nil)
	}
	switch args[0] {
	case "__complete": // hidden: used by the shell completion scripts
		return cmdComplete(a, args[1:])
	case "-h", "--help":
		return cmdHelp(a, args[1:])
	case "-v", "--version":
		return cmdVersion(a, nil)
	}
	c := findCommand(args[0])
	if c == nil {
		return usagef("unknown command %q%s (run `todo help`)", args[0], suggestCommand(args[0]))
	}
	return c.run(a, args[1:])
}

func (a *app) resolveDataDir() (string, error) {
	if a.dataDir != "" {
		return a.dataDir, nil
	}
	if d := a.env.Getenv(core.EnvHome); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".todo-cli"), nil
}

func (a *app) open() (*core.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	dir, err := a.resolveDataDir()
	if err != nil {
		return nil, err
	}
	s, err := core.Open(core.Options{DataDir: dir, Actor: a.actor, Now: a.env.Now})
	if err != nil {
		return nil, err
	}
	if m := s.LastMigration; m != nil && m.From > 0 && !a.json {
		fmt.Fprintf(a.env.Stderr, "todo: upgraded database schema v%d → v%d (backup: %s)\n", m.From, m.To, m.BackupPath)
	}
	a.store = s
	return s, nil
}

func (a *app) now() time.Time { return a.env.Now() }

// resolve expands task id prefixes.
func (a *app) resolve(ids []string) ([]string, error) {
	s, err := a.open()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		full, err := s.ResolveID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

func (a *app) resolveOne(id string) (string, error) {
	ids, err := a.resolve([]string{id})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

func (a *app) emitJSON(v any) error {
	enc := json.NewEncoder(a.env.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.env.Stdout, format, args...)
}

func errorCode(err error) (string, int) {
	var ue usageError
	switch {
	case errors.As(err, &ue):
		return "usage", ExitUsage
	case errors.Is(err, core.ErrNotFound):
		return "not_found", ExitNotFound
	case errors.Is(err, core.ErrConflict):
		return "version_conflict", ExitConflict
	}
	for _, e := range []error{core.ErrAmbiguousID, core.ErrInvalid, core.ErrDeleted, core.ErrNothingToUndo,
		core.ErrSchemaTooNew, core.ErrInvalidImportFile} {
		if errors.Is(err, e) {
			return e.Error(), ExitError
		}
	}
	return "internal_error", ExitError
}

func (a *app) fail(err error) int {
	code, exit := errorCode(err)
	if a.json {
		enc := json.NewEncoder(a.env.Stderr)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"error": map[string]string{"code": code, "message": err.Error()}})
	} else {
		fmt.Fprintf(a.env.Stderr, "todo: %v\n", err)
		if exit == ExitUsage {
			fmt.Fprintln(a.env.Stderr, "run `todo help` for usage")
		}
	}
	return exit
}

// stringList is a repeatable flag that also splits on commas.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse parses flags interleaved with positional arguments; everything after
// "--" is positional.
func (a *app) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				a.commandHelp(fs)
				return nil, errHelpShown
			}
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return append(pos, tail...), nil
}

var errHelpShown = errors.New("help shown")

func (a *app) commandHelp(fs *flag.FlagSet) {
	c := findCommand(fs.Name())
	w := a.env.Stdout
	if c != nil {
		fmt.Fprintf(w, "usage: todo %s %s\n\n%s\n", c.name, c.args, c.summary)
	}
	fs.SetOutput(w)
	if hasFlags(fs) {
		fmt.Fprintln(w, "\nflags:")
		fs.PrintDefaults()
	}
	fs.SetOutput(io.Discard)
}

func hasFlags(fs *flag.FlagSet) bool {
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	return n > 0
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
