package llm

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Operation kinds a model can initiate.
const (
	OpTaskCreate    = "task.create"
	OpTaskUpdate    = "task.update"    // priority, due time, dependencies, order, status, tags, category
	OpTaskOverwrite = "task.overwrite" // replacing the title, description or notes of an existing task
	OpTaskDelete    = "task.delete"
	OpCommand       = "command"
	OpAgentStart    = "agent.start"
	OpConfigChange  = "config.change" // self-optimisation of prompts and rules
)

// Side-effect categories. Those in DefaultConfirm always need explicit
// confirmation, even in auto mode (NFR-025).
const (
	CatDelete       = "delete"
	CatOverwrite    = "overwrite"
	CatSend         = "send" // sending data off this machine
	CatCommit       = "commit"
	CatSystemConfig = "system_config"
	CatShell        = "shell" // inline scripts (sh -c, osascript -e …)
)

// DefaultConfirm are the categories confirmed by default: delete,
// overwrite, external send, commit, system configuration.
var DefaultConfirm = []string{CatDelete, CatOverwrite, CatSend, CatCommit, CatSystemConfig}

// KnownConfirmEntries are the category and kind names a confirm list may use;
// anything else is a command prefix.
var KnownConfirmEntries = []string{CatDelete, CatOverwrite, CatSend, CatCommit, CatSystemConfig, CatShell,
	OpTaskCreate, OpTaskUpdate, OpTaskOverwrite, OpTaskDelete, OpCommand, OpAgentStart}

// ErrForbidden rejects operations no mode allows (privilege escalation).
var ErrForbidden = errors.New("forbidden")

// Action describes one model-initiated operation for the policy.
type Action struct {
	Kind string
	Argv []string
}

// Verdict says what may happen with an action now.
type Verdict struct {
	// Execute: apply without asking. False means it waits for the user.
	Execute    bool     `json:"execute"`
	Categories []string `json:"categories,omitempty"`
	// Reason explains why confirmation is needed.
	Reason string `json:"reason,omitempty"`
}

// Policy combines the permission mode with the confirm list.
type Policy struct {
	Mode    Mode
	Confirm []string
}

// PolicyFor builds the policy of the settings.
func PolicyFor(s Settings) Policy { return Policy{Mode: s.Mode, Confirm: s.ConfirmList} }

// Check decides whether an action may run without asking. Suggest mode
// never executes, confirm mode always asks, auto mode executes everything
// except confirmed categories and confirm-list matches (FR-604). Privilege
// escalation is refused in every mode: the model only ever has the current
// user's macOS permissions.
func (p Policy) Check(a Action) (Verdict, error) {
	cats := Categories(a)
	v := Verdict{Categories: cats}
	if a.Kind == OpCommand || a.Kind == OpAgentStart {
		if err := checkEscalation(a.Argv); err != nil {
			return v, err
		}
	}
	switch p.Mode {
	case ModeSuggest:
		v.Reason = "仅建议模式：需要用户手动接受"
		return v, nil
	case ModeAuto:
	default:
		v.Reason = "执行前确认模式"
		return v, nil
	}
	for _, c := range cats {
		if slices.Contains(DefaultConfirm, c) {
			v.Reason = "高风险操作（" + categoryLabel(c) + "）默认需要确认"
			return v, nil
		}
	}
	for _, entry := range p.Confirm {
		if matchesConfirm(entry, a, cats) {
			v.Reason = "在自定义确认清单中：" + entry
			return v, nil
		}
	}
	v.Execute = true
	return v, nil
}

func categoryLabel(c string) string {
	switch c {
	case CatDelete:
		return "删除"
	case CatOverwrite:
		return "覆盖"
	case CatSend:
		return "外发"
	case CatCommit:
		return "提交"
	case CatSystemConfig:
		return "系统配置修改"
	case CatShell:
		return "内联脚本"
	}
	return c
}

func matchesConfirm(entry string, a Action, cats []string) bool {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return false
	}
	if entry == a.Kind || slices.Contains(cats, entry) {
		return true
	}
	if slices.Contains(KnownConfirmEntries, entry) || len(a.Argv) == 0 {
		return false
	}
	want := strings.Fields(entry)
	argv := append([]string{filepath.Base(a.Argv[0])}, a.Argv[1:]...)
	if len(want) > len(argv) {
		return false
	}
	for i, w := range want {
		if i == 0 {
			w = filepath.Base(w)
		}
		if argv[i] != w {
			return false
		}
	}
	return true
}

// ValidateConfirmEntry checks a confirm list entry.
func ValidateConfirmEntry(entry string) (string, error) {
	entry = strings.Join(strings.Fields(entry), " ")
	if entry == "" {
		return "", fmt.Errorf("%w: empty confirm entry", ErrInvalid)
	}
	return entry, nil
}

var (
	deletePrograms    = []string{"rm", "rmdir", "unlink", "shred", "trash", "srm"}
	overwritePrograms = []string{"mv", "cp", "dd", "truncate", "ditto", "rsync", "tee", "install"}
	sendPrograms      = []string{"curl", "wget", "http", "https", "ssh", "scp", "sftp", "ftp", "nc", "ncat", "telnet", "mail", "sendmail", "rsync", "aws", "gsutil", "az"}
	systemPrograms    = []string{"defaults", "launchctl", "networksetup", "systemsetup", "scutil", "csrutil", "spctl", "pmset", "tccutil", "crontab", "chmod", "chown", "chflags", "diskutil", "nvram", "dscl", "softwareupdate", "kextload", "profiles"}
	shellPrograms     = []string{"sh", "bash", "zsh", "fish", "dash", "ksh", "osascript", "python", "python3", "node", "perl", "ruby", "eval", "xargs", "env"}
	escalation        = []string{"sudo", "su", "doas", "pkexec", "authopen"}
	inlineFlags       = []string{"-c", "-e", "--eval", "--command", "--exec"}
)

// Categories classifies an action's side effects. Inline scripts are
// scanned word by word, so `sh -c "rm -rf x"` is still a delete.
func Categories(a Action) []string {
	var cats []string
	add := func(c string) {
		if !slices.Contains(cats, c) {
			cats = append(cats, c)
		}
	}
	switch a.Kind {
	case OpTaskDelete:
		add(CatDelete)
	case OpTaskOverwrite:
		add(CatOverwrite)
	}
	if len(a.Argv) == 0 {
		return cats
	}
	words := commandWords(a.Argv)
	for i, w := range words {
		prog := filepath.Base(w)
		next := ""
		if i+1 < len(words) {
			next = words[i+1]
		}
		switch {
		case slices.Contains(deletePrograms, prog):
			add(CatDelete)
		case prog == "git" || prog == "hg" || prog == "svn" || prog == "gh":
			switch next {
			case "commit", "merge", "rebase", "tag", "am", "cherry-pick", "revert":
				add(CatCommit)
			case "push", "send-email", "publish":
				add(CatCommit)
				add(CatSend)
			case "pr", "release", "issue", "gist":
				add(CatSend)
			case "reset", "clean", "checkout", "restore", "rm", "branch":
				add(CatOverwrite)
			}
		case prog == "npm" || prog == "pnpm" || prog == "yarn" || prog == "cargo" || prog == "twine" || prog == "gem":
			if next == "publish" || next == "push" {
				add(CatSend)
			}
		case prog == "brew" || prog == "port" || prog == "pip" || prog == "pip3":
			if next == "install" || next == "uninstall" || next == "upgrade" || next == "remove" {
				add(CatSystemConfig)
			}
		}
		if slices.Contains(overwritePrograms, prog) {
			add(CatOverwrite)
		}
		if slices.Contains(sendPrograms, prog) {
			add(CatSend)
		}
		if slices.Contains(systemPrograms, prog) {
			add(CatSystemConfig)
		}
		if strings.Contains(w, ">") {
			add(CatOverwrite)
		}
	}
	prog := filepath.Base(a.Argv[0])
	if slices.Contains(shellPrograms, prog) {
		for _, arg := range a.Argv[1:] {
			if slices.Contains(inlineFlags, arg) {
				add(CatShell)
				break
			}
		}
	}
	slices.Sort(cats)
	return cats
}

// commandWords splits argv and any inline script into words.
func commandWords(argv []string) []string {
	var out []string
	for _, a := range argv {
		out = append(out, strings.FieldsFunc(a, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' || r == '(' || r == ')' || r == '`' || r == '"' || r == '\''
		})...)
	}
	return out
}

func checkEscalation(argv []string) error {
	for _, w := range commandWords(argv) {
		if slices.Contains(escalation, filepath.Base(w)) {
			return fmt.Errorf("%w: %q would raise privileges; the model only runs with the current user's permissions", ErrForbidden, w)
		}
	}
	return nil
}
