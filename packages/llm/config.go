// Package llm connects todo-cli to OpenAI-compatible model services and
// implements the model features on top of packages/core: natural-language
// task intake, decision support, self-optimisation suggestions, and
// model-initiated commands under a permission policy.
//
// Credentials never enter the task database or the settings file: they come
// from environment variables or the macOS keychain, and every text that is
// logged, stored or sent to the model passes through Redact first.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Environment variables. The *_MODEL_* ones override the active profile.
const (
	EnvProvider = "TODO_CLI_MODEL_PROVIDER"
	EnvModel    = "TODO_CLI_MODEL"
	EnvBaseURL  = "TODO_CLI_MODEL_BASE_URL"
	EnvAPIKey   = "TODO_CLI_MODEL_API_KEY"
	// EnvProfile selects a profile for one process without `todo llm use`.
	EnvProfile = "TODO_CLI_MODEL_PROFILE"
	// EnvMode overrides the permission mode for one process.
	EnvMode = "TODO_CLI_LLM_MODE"
)

// Defaults: the user brings an OpenAI-compatible key; gpt-4o-mini.
const (
	DefaultProvider = "openai"
	DefaultModel    = "gpt-4o-mini"
	DefaultBaseURL  = "https://api.openai.com/v1"
	DefaultProfile  = "default"
	// SettingsFile lives in the data directory (0600) and never holds keys.
	SettingsFile = "llm.json"
	// KeychainService is the macOS keychain service name for API keys.
	KeychainService = "todo-cli"
)

// providerBaseURLs are the default endpoints of well-known OpenAI-compatible
// services; any other provider needs an explicit base URL.
var providerBaseURLs = map[string]string{
	"openai":     DefaultBaseURL,
	"deepseek":   "https://api.deepseek.com/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"moonshot":   "https://api.moonshot.cn/v1",
	"dashscope":  "https://dashscope.aliyuncs.com/compatible-mode/v1",
	"ollama":     "http://127.0.0.1:11434/v1",
	"lmstudio":   "http://127.0.0.1:1234/v1",
}

// ProviderBaseURL returns the default endpoint for a provider, if known.
func ProviderBaseURL(provider string) string {
	return providerBaseURLs[strings.ToLower(strings.TrimSpace(provider))]
}

// Mode is the permission mode for model-initiated changes (FR-305).
type Mode string

const (
	ModeSuggest Mode = "suggest" // 仅建议: nothing is applied without an explicit user action
	ModeConfirm Mode = "confirm" // 执行前确认 (default): changes wait for confirmation
	ModeAuto    Mode = "auto"    // 自动执行: authorised changes are applied directly
)

// Modes lists every mode.
var Modes = []Mode{ModeSuggest, ModeConfirm, ModeAuto}

// ErrInvalid reports bad user input (settings, flags, edits).
var ErrInvalid = errors.New("invalid_input")

// ParseMode accepts the mode names and their Chinese labels.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "suggest", "suggest-only", "suggestion", "仅建议", "建议":
		return ModeSuggest, nil
	case "confirm", "ask", "执行前确认", "确认":
		return ModeConfirm, nil
	case "auto", "automatic", "自动执行", "自动":
		return ModeAuto, nil
	}
	return "", fmt.Errorf("%w: unknown permission mode %q (want suggest|confirm|auto)", ErrInvalid, s)
}

// Label is the user-facing name of the mode.
func (m Mode) Label() string {
	switch m {
	case ModeSuggest:
		return "仅建议"
	case ModeAuto:
		return "自动执行"
	}
	return "执行前确认"
}

// Profile is one model service configuration. It never contains the key
// itself, only where to find it.
type Profile struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	// APIKeyEnv names the environment variable holding the key (default
	// TODO_CLI_MODEL_API_KEY).
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Keychain means the key was stored in the macOS keychain
	// (service "todo-cli", account = profile name).
	Keychain bool `json:"keychain,omitempty"`
	// NoKey marks services that need no key (e.g. a local Ollama).
	NoKey          bool `json:"no_key,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
	// MaxRetries is how often transient failures are retried (default 1).
	MaxRetries *int `json:"max_retries,omitempty"`
}

// AgentProfile is an agent the user or the model may start (FR-505,
// FR-510). Packages/agent runs it through the adapter named in Adapter:
// "cli" (default) runs Command without a shell, "http" calls URL, "llm"
// sends the final prompt to a model profile, and any other name is an
// adapter registered with packages/agent.
type AgentProfile struct {
	Name        string   `json:"name"`
	Command     []string `json:"command,omitempty"`
	Dir         string   `json:"dir,omitempty"`
	Description string   `json:"description,omitempty"`
	Adapter     string   `json:"adapter,omitempty"`
	// Env lists the environment variables passed through to the agent; the
	// rest of the environment (and always the model keys) is withheld.
	Env            []string `json:"env,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	// SuccessCodes are the exit codes that mean success (default 0).
	SuccessCodes []int `json:"success_codes,omitempty"`
	// Input is how the context reaches the agent: json-stdin (default),
	// json-arg, prompt-stdin, prompt-arg or none.
	Input string `json:"input,omitempty"`
	// Output is how stdout is parsed: jsonl (default), json or text.
	Output            string   `json:"output,omitempty"`
	MaxRetries        int      `json:"max_retries,omitempty"`
	RetryDelaySeconds int      `json:"retry_delay_seconds,omitempty"`
	Confirm           bool     `json:"confirm,omitempty"`
	Constraints       []string `json:"constraints,omitempty"`
	// URL, Method and HeaderEnv configure the http adapter. HeaderEnv maps
	// a header to the environment variable holding its value, so tokens
	// never enter the settings file.
	URL       string            `json:"url,omitempty"`
	Method    string            `json:"method,omitempty"`
	HeaderEnv map[string]string `json:"header_env,omitempty"`
	// Profile is the model profile of the llm adapter ("" = active).
	Profile string `json:"profile,omitempty"`
	// Options are free-form settings of registered adapters.
	Options map[string]string `json:"options,omitempty"`
}

// AdapterName is the adapter kind; empty means cli.
func (a *AgentProfile) AdapterName() string {
	if a.Adapter == "" {
		return "cli"
	}
	return a.Adapter
}

// Settings is the user's model configuration (data dir/llm.json).
type Settings struct {
	Active   string    `json:"active"`
	Profiles []Profile `json:"profiles"`
	Mode     Mode      `json:"mode"`
	// ConfirmList holds operations that always need confirmation, even in
	// auto mode (FR-604): categories (delete, overwrite, send, commit,
	// system_config, shell), operation kinds (task.update, command,
	// agent.start …) or command prefixes ("git push", "npm publish").
	ConfirmList []string `json:"confirm_list"`
	// SendFields are the task fields sent to the model. Default: title,
	// description, tags, status, priority.
	SendFields []string `json:"send_fields"`
	// DecisionFields are sent in addition for decision support, which needs
	// deadlines and dependencies (FR-403). Default: due_at, depends_on.
	DecisionFields []string `json:"decision_fields"`
	// AllowCommands lets the model propose shell commands (FR-603).
	AllowCommands bool           `json:"allow_commands"`
	CommandDir    string         `json:"command_dir,omitempty"`
	Agents        []AgentProfile `json:"agents,omitempty"`
}

// Task fields that may be sent to the model.
var SendableFields = []string{"title", "description", "tags", "status", "priority", "due_at", "depends_on", "category", "notes", "parent_id"}

// DefaultSettings returns the built-in configuration.
func DefaultSettings() Settings {
	return Settings{
		Active:         DefaultProfile,
		Profiles:       []Profile{{Name: DefaultProfile, Provider: DefaultProvider, Model: DefaultModel, BaseURL: DefaultBaseURL}},
		Mode:           ModeConfirm,
		ConfirmList:    []string{},
		SendFields:     []string{"title", "description", "tags", "status", "priority"},
		DecisionFields: []string{"due_at", "depends_on"},
		AllowCommands:  true,
	}
}

// SettingsPath is the settings file inside a data directory.
func SettingsPath(dataDir string) string { return filepath.Join(dataDir, SettingsFile) }

// LoadSettings reads the settings file; a missing file yields the defaults.
func LoadSettings(dataDir string) (Settings, error) {
	s := DefaultSettings()
	b, err := os.ReadFile(SettingsPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var in Settings
	if err := json.Unmarshal(b, &in); err != nil {
		return s, fmt.Errorf("%w: %s: %v", ErrInvalid, SettingsPath(dataDir), err)
	}
	if in.Profiles == nil {
		in.Profiles = s.Profiles
	}
	if in.Active == "" && len(in.Profiles) > 0 {
		in.Active = in.Profiles[0].Name
	}
	if in.Mode == "" {
		in.Mode = s.Mode
	}
	if in.ConfirmList == nil {
		in.ConfirmList = []string{}
	}
	if in.SendFields == nil {
		in.SendFields = s.SendFields
	}
	if in.DecisionFields == nil {
		in.DecisionFields = s.DecisionFields
	}
	return in, in.Validate()
}

// Validate checks names, modes and fields.
func (s *Settings) Validate() error {
	if _, err := ParseMode(string(s.Mode)); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, p := range s.Profiles {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("%w: a model profile needs a name", ErrInvalid)
		}
		if names[p.Name] {
			return fmt.Errorf("%w: duplicate model profile %q", ErrInvalid, p.Name)
		}
		names[p.Name] = true
		if p.BaseURL != "" {
			if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("%w: profile %q: base_url %q must be an http(s) URL", ErrInvalid, p.Name, RedactURL(p.BaseURL))
			}
			if u, _ := url.Parse(p.BaseURL); u.User != nil {
				return fmt.Errorf("%w: profile %q: do not put credentials in base_url; use a key (todo llm set-key)", ErrInvalid, p.Name)
			}
		}
	}
	if len(s.Profiles) > 0 && !names[s.Active] {
		return fmt.Errorf("%w: active profile %q does not exist", ErrInvalid, s.Active)
	}
	for _, f := range append(slices.Clone(s.SendFields), s.DecisionFields...) {
		if !slices.Contains(SendableFields, f) {
			return fmt.Errorf("%w: unknown task field %q (allowed: %s)", ErrInvalid, f, strings.Join(SendableFields, ", "))
		}
	}
	agents := map[string]bool{}
	for _, a := range s.Agents {
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("%w: an agent needs a name", ErrInvalid)
		}
		if agents[strings.ToLower(a.Name)] {
			return fmt.Errorf("%w: duplicate agent %q", ErrInvalid, a.Name)
		}
		agents[strings.ToLower(a.Name)] = true
		switch a.AdapterName() {
		case "cli":
			if len(a.Command) == 0 || strings.TrimSpace(a.Command[0]) == "" {
				return fmt.Errorf("%w: agent %q needs a command", ErrInvalid, a.Name)
			}
		case "http":
			if a.URL == "" {
				return fmt.Errorf("%w: agent %q needs a url", ErrInvalid, a.Name)
			}
		}
	}
	return nil
}

// Save writes the settings atomically with 0600 permissions.
func (s *Settings) Save(dataDir string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, ".llm-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), SettingsPath(dataDir))
}

// Profile returns the named profile.
func (s *Settings) Profile(name string) (*Profile, error) {
	for i := range s.Profiles {
		if s.Profiles[i].Name == name {
			return &s.Profiles[i], nil
		}
	}
	return nil, fmt.Errorf("%w: no model profile %q (have: %s)", ErrInvalid, name, strings.Join(s.ProfileNames(), ", "))
}

// ProfileNames lists the configured profiles.
func (s *Settings) ProfileNames() []string {
	out := make([]string, len(s.Profiles))
	for i, p := range s.Profiles {
		out[i] = p.Name
	}
	return out
}

// PutProfile adds or replaces a profile, filling the provider's default
// base URL.
func (s *Settings) PutProfile(p Profile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Provider == "" {
		p.Provider = DefaultProvider
	}
	if p.Model == "" {
		p.Model = DefaultModel
	}
	if p.BaseURL == "" {
		p.BaseURL = ProviderBaseURL(p.Provider)
	}
	if p.BaseURL == "" {
		return fmt.Errorf("%w: provider %q has no known endpoint; pass --base-url", ErrInvalid, p.Provider)
	}
	for i := range s.Profiles {
		if s.Profiles[i].Name == p.Name {
			p.Keychain = p.Keychain || s.Profiles[i].Keychain
			s.Profiles[i] = p
			return s.Validate()
		}
	}
	s.Profiles = append(s.Profiles, p)
	if s.Active == "" {
		s.Active = p.Name
	}
	return s.Validate()
}

// RemoveProfile deletes a profile; the active one cannot be removed while
// other profiles exist, so switch first.
func (s *Settings) RemoveProfile(name string) error {
	if _, err := s.Profile(name); err != nil {
		return err
	}
	if name == s.Active && len(s.Profiles) > 1 {
		return fmt.Errorf("%w: %q is the active profile; switch with `todo llm use <other>` first", ErrInvalid, name)
	}
	s.Profiles = slices.DeleteFunc(s.Profiles, func(p Profile) bool { return p.Name == name })
	if len(s.Profiles) == 0 {
		s.Active = ""
	}
	return nil
}

// Resolved is a profile with environment overrides and its key applied.
type Resolved struct {
	Profile
	// APIKey is the secret; never print or store it.
	APIKey string `json:"-"`
	// KeySource is where the key came from: env:NAME, keychain or none.
	KeySource string `json:"key_source"`
	// Source is "env" when environment variables override the profile.
	Source string `json:"source"`
	// Status is configured or not_configured.
	Status string `json:"status"`
	// Problem explains why the profile is not usable.
	Problem string `json:"problem,omitempty"`
}

// Configured reports whether model calls can be attempted.
func (r *Resolved) Configured() bool { return r.Status == "configured" }

// Resolve picks the profile (explicit name, $TODO_CLI_MODEL_PROFILE or the
// active one), applies environment overrides and finds its key: the
// environment first, then the macOS keychain.
func Resolve(s Settings, name string, getenv func(string) string, secrets SecretStore) (*Resolved, error) {
	if name == "" {
		name = getenv(EnvProfile)
	}
	if name == "" {
		name = s.Active
	}
	r := &Resolved{Source: "profile"}
	if p, err := s.Profile(name); err == nil {
		r.Profile = *p
	} else if len(s.Profiles) > 0 || name != "" && name != DefaultProfile {
		return nil, err
	} else {
		r.Profile = DefaultSettings().Profiles[0]
	}
	if v := getenv(EnvProvider); v != "" {
		r.Provider, r.Source = v, "env"
		if getenv(EnvBaseURL) == "" && ProviderBaseURL(v) != "" {
			r.BaseURL = ProviderBaseURL(v)
		}
	}
	if v := getenv(EnvModel); v != "" {
		r.Model, r.Source = v, "env"
	}
	if v := getenv(EnvBaseURL); v != "" {
		r.BaseURL, r.Source = v, "env"
	}
	keyEnv := r.APIKeyEnv
	if keyEnv == "" {
		keyEnv = EnvAPIKey
	}
	switch {
	case getenv(keyEnv) != "":
		r.APIKey, r.KeySource = getenv(keyEnv), "env:"+keyEnv
	case keyEnv != EnvAPIKey && getenv(EnvAPIKey) != "":
		r.APIKey, r.KeySource = getenv(EnvAPIKey), "env:"+EnvAPIKey
	case r.Keychain && secrets != nil:
		key, err := secrets.Get(r.Name)
		if err == nil && key != "" {
			r.APIKey, r.KeySource = key, "keychain"
		} else if err != nil && !errors.Is(err, ErrSecretNotFound) {
			r.Problem = "读取 macOS 钥匙串失败：" + Redact(err.Error())
		}
	}
	if r.KeySource == "" {
		r.KeySource = "none"
	}
	r.Status = "configured"
	switch {
	case r.Model == "":
		r.Status, r.Problem = "not_configured", "未设置模型名称"
	case r.BaseURL == "":
		r.Status, r.Problem = "not_configured", "未设置接口地址"
	case r.APIKey == "" && !r.NoKey:
		r.Status = "not_configured"
		if r.Problem == "" {
			r.Problem = fmt.Sprintf("未找到访问凭据：设置环境变量 %s，或运行 `todo llm set-key`（macOS 钥匙串）", keyEnv)
		}
	}
	return r, nil
}

// Public is the display form of a resolved profile: no key, no URL
// credentials or query strings.
type Public struct {
	Profile   string `json:"profile"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url"`
	Status    string `json:"status"`
	Source    string `json:"source"`
	KeySource string `json:"key_source"`
	APIKeySet bool   `json:"api_key_set"`
	Problem   string `json:"problem,omitempty"`
}

// Public strips secrets for display.
func (r *Resolved) Public() Public {
	return Public{Profile: r.Name, Provider: r.Provider, Model: r.Model, BaseURL: RedactURL(r.BaseURL), Status: r.Status,
		Source: r.Source, KeySource: r.KeySource, APIKeySet: r.APIKey != "", Problem: r.Problem}
}

// RedactURL drops credentials, query strings and fragments, which may
// carry keys.
func RedactURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "[unparseable]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
