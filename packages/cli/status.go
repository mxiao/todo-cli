package cli

import (
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
)

// Environment variables that configure the model (see packages/llm);
// credentials never land in the task database (FR-601, FR-607).
const (
	EnvModelProvider = llm.EnvProvider
	EnvModelName     = llm.EnvModel
	EnvModelBaseURL  = llm.EnvBaseURL
	EnvModelAPIKey   = llm.EnvAPIKey
)

// StatusReport is the `todo status` output (FR-001).
type StatusReport struct {
	Version  string         `json:"version"`
	State    string         `json:"state"` // ok | error
	Error    string         `json:"error,omitempty"`
	DataDir  string         `json:"data_dir"`
	Database DatabaseStatus `json:"database"`
	Backups  BackupStatus   `json:"backups"`
	Tasks    *core.Stats    `json:"tasks"`
	Model    ModelStatus    `json:"model"`
	Runtime  RuntimeStatus  `json:"runtime"`
}

type DatabaseStatus struct {
	Path                string `json:"path"`
	SizeBytes           int64  `json:"size_bytes"`
	SchemaVersion       int    `json:"schema_version"`
	LatestSchemaVersion int    `json:"latest_schema_version"`
}

type BackupStatus struct {
	Count  int    `json:"count"`
	Latest string `json:"latest,omitempty"`
}

type ModelStatus struct {
	// Status is "configured" when the active profile has a model, an
	// endpoint and a key, otherwise "not_configured"; task management works
	// either way (FR-606).
	Status    string   `json:"status"`
	Source    string   `json:"source"`
	Profile   string   `json:"profile,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Model     string   `json:"model,omitempty"`
	BaseURL   string   `json:"base_url,omitempty"`
	APIKeySet bool     `json:"api_key_set"`
	KeySource string   `json:"key_source,omitempty"`
	Mode      llm.Mode `json:"mode,omitempty"`
	Problem   string   `json:"problem,omitempty"`
}

type RuntimeStatus struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`
	PID       int    `json:"pid"`
	User      string `json:"user,omitempty"`
}

// modelStatus resolves the active model profile without touching the
// database; the key itself is never reported.
func (a *app) modelStatus() ModelStatus {
	m := ModelStatus{Status: "not_configured"}
	dir, err := a.resolveDataDir()
	if err != nil {
		m.Problem = err.Error()
		return m
	}
	st, err := llm.LoadSettings(dir)
	if err != nil {
		m.Problem = err.Error()
		return m
	}
	m.Mode = st.Mode
	if v := a.env.Getenv(llm.EnvMode); v != "" {
		if mode, err := llm.ParseMode(v); err == nil {
			m.Mode = mode
		}
	}
	r, err := llm.Resolve(st, "", a.env.Getenv, a.secretStore())
	if err != nil {
		m.Problem = err.Error()
		return m
	}
	p := r.Public()
	m.Status, m.Source, m.Profile, m.Provider, m.Model = p.Status, p.Source, p.Profile, p.Provider, p.Model
	m.BaseURL, m.APIKeySet, m.KeySource, m.Problem = p.BaseURL, p.APIKeySet, p.KeySource, p.Problem
	return m
}

func cmdStatus(a *app, args []string) error {
	fs := newFlags("status")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	r := StatusReport{
		Version: Version, State: "ok", Model: a.modelStatus(),
		Runtime: RuntimeStatus{OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), PID: os.Getpid()},
	}
	if u, err := user.Current(); err == nil {
		r.Runtime.User = u.Username
	}
	r.Database.LatestSchemaVersion = core.LatestSchemaVersion()
	err := a.collectStatus(&r)
	if err != nil {
		r.State, r.Error = "error", err.Error()
	}
	if a.json {
		if jerr := a.emitJSON(r); jerr != nil {
			return jerr
		}
	} else {
		a.printStatus(&r)
	}
	if err != nil {
		return statusFailed{err}
	}
	return nil
}

// statusFailed signals a non-zero exit after the report was already printed.
type statusFailed struct{ error }

func (a *app) collectStatus(r *StatusReport) error {
	dir, err := a.resolveDataDir()
	if err != nil {
		return err
	}
	r.DataDir = dir
	s, err := a.open()
	if err != nil {
		return err
	}
	r.Database.Path = s.DBPath()
	if fi, err := os.Stat(s.DBPath()); err == nil {
		r.Database.SizeBytes = fi.Size()
	}
	if r.Database.SchemaVersion, err = s.SchemaVersion(); err != nil {
		return err
	}
	backups, err := s.Backups()
	if err != nil {
		return err
	}
	r.Backups.Count = len(backups)
	if len(backups) > 0 {
		r.Backups.Latest = backups[len(backups)-1]
	}
	st, err := s.Stats()
	if err != nil {
		return err
	}
	r.Tasks = &st
	return nil
}

func (a *app) printStatus(r *StatusReport) {
	a.printf("todo %s — %s\n", r.Version, r.State)
	if r.Error != "" {
		a.printf("error:       %s\n", r.Error)
	}
	a.printf("data dir:    %s\n", r.DataDir)
	if r.Database.Path != "" {
		a.printf("database:    %s (%d bytes, schema v%d/v%d)\n", r.Database.Path, r.Database.SizeBytes,
			r.Database.SchemaVersion, r.Database.LatestSchemaVersion)
		a.printf("backups:     %d", r.Backups.Count)
		if r.Backups.Latest != "" {
			a.printf(" (latest %s)", r.Backups.Latest)
		}
		a.printf("\n")
	}
	if r.Tasks != nil {
		var parts []string
		for _, s := range core.Statuses {
			parts = append(parts, string(s)+"="+strconv.Itoa(r.Tasks.ByStatus[s]))
		}
		a.printf("tasks:       %d (%s; overdue=%d, deleted=%d)\n", r.Tasks.Total, strings.Join(parts, " "),
			r.Tasks.Overdue, r.Tasks.Deleted)
	}
	m := r.Model
	a.printf("model:       %s", m.Status)
	if m.Model != "" {
		a.printf(" (%s/%s", m.Provider, m.Model)
		if m.BaseURL != "" {
			a.printf(" @ %s", m.BaseURL)
		}
		a.printf(", profile %s)", m.Profile)
	}
	key := "missing"
	if m.APIKeySet {
		key = "set (" + m.KeySource + ")"
	}
	a.printf(", api key %s", key)
	if m.Mode != "" {
		a.printf(", mode %s", m.Mode)
	}
	a.printf("\n")
	if m.Problem != "" {
		a.printf("             %s; task management works without a model\n", m.Problem)
	}
	a.printf("runtime:     %s/%s, %s, pid %d\n", r.Runtime.OS, r.Runtime.Arch, r.Runtime.GoVersion, r.Runtime.PID)
}
