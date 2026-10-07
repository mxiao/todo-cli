package agent

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mxiao/todo-cli/packages/llm"
)

// Spec is an agent definition; agents live in the model settings
// (llm.json) so the model's decisions and routing see the same agents.
type Spec = llm.AgentProfile

// Input modes: how the context reaches an agent.
const (
	InputJSONStdin   = "json-stdin"   // the JSON input on stdin (default)
	InputJSONArg     = "json-arg"     // the JSON input as the last argument
	InputPromptStdin = "prompt-stdin" // the final prompt text on stdin
	InputPromptArg   = "prompt-arg"   // the final prompt as the last argument
	InputNone        = "none"         // nothing; arguments may use {{placeholders}}
)

// Output modes: how stdout is parsed.
const (
	OutputJSONL = "jsonl" // one protocol message per line; other lines are plain output (default)
	OutputJSON  = "json"  // one JSON document at the end: {status, message, results, events}
	OutputText  = "text"  // plain text; all of stdout becomes one text result
)

var (
	inputModes  = []string{InputJSONStdin, InputJSONArg, InputPromptStdin, InputPromptArg, InputNone}
	outputModes = []string{OutputJSONL, OutputJSON, OutputText}
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerRE    = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

const (
	// DefaultTimeout bounds a run attempt unless the agent sets its own.
	DefaultTimeout = 30 * time.Minute
	maxTimeout     = 24 * time.Hour
	maxRetries     = 5
)

// Factory builds the adapter of one attempt.
type Factory func(spec Spec, deps Deps) (Adapter, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds an adapter kind, so agents can reach applications and
// systems that are not built in (FR-510). Registering a name again
// replaces it.
func Register(kind string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[kind] = f
}

// Adapters lists the registered adapter kinds.
func Adapters() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func factory(kind string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := registry[kind]
	return f, ok
}

func init() {
	Register("cli", newCLIAdapter)
	Register("http", newHTTPAdapter)
	Register("llm", newLLMAdapter)
}

// Normalize fills defaults and checks an agent definition. hidden are the
// environment variables that must never reach an agent (model keys).
func Normalize(sp *Spec, hidden []string) error {
	sp.Name = strings.TrimSpace(sp.Name)
	if sp.Name == "" || strings.ContainsAny(sp.Name, " \t\n/") || sp.Name == "auto" {
		return fmt.Errorf("%w: agent name %q must be one word and not \"auto\"", ErrInvalid, sp.Name)
	}
	kind := sp.AdapterName()
	if _, ok := factory(kind); !ok {
		return fmt.Errorf("%w: agent %q: unknown adapter %q (available: %s)", ErrInvalid, sp.Name, kind, strings.Join(Adapters(), ", "))
	}
	if sp.Input == "" {
		sp.Input = InputJSONStdin
	}
	if !slices.Contains(inputModes, sp.Input) {
		return fmt.Errorf("%w: agent %q: input %q (want %s)", ErrInvalid, sp.Name, sp.Input, strings.Join(inputModes, ", "))
	}
	if sp.Output == "" {
		sp.Output = OutputJSONL
		if kind == "llm" {
			sp.Output = OutputText
		}
	}
	if !slices.Contains(outputModes, sp.Output) {
		return fmt.Errorf("%w: agent %q: output %q (want %s)", ErrInvalid, sp.Name, sp.Output, strings.Join(outputModes, ", "))
	}
	if sp.TimeoutSeconds < 0 || time.Duration(sp.TimeoutSeconds)*time.Second > maxTimeout {
		return fmt.Errorf("%w: agent %q: timeout must be between 1s and 24h", ErrInvalid, sp.Name)
	}
	if sp.MaxRetries < 0 || sp.MaxRetries > maxRetries {
		return fmt.Errorf("%w: agent %q: max_retries must be 0-%d", ErrInvalid, sp.Name, maxRetries)
	}
	if sp.RetryDelaySeconds < 0 || sp.RetryDelaySeconds > 3600 {
		return fmt.Errorf("%w: agent %q: retry_delay_seconds must be 0-3600", ErrInvalid, sp.Name)
	}
	for _, c := range sp.SuccessCodes {
		if c < 0 || c > 255 {
			return fmt.Errorf("%w: agent %q: success code %d out of range 0-255", ErrInvalid, sp.Name, c)
		}
	}
	for _, e := range sp.Env {
		if !envNameRE.MatchString(e) {
			return fmt.Errorf("%w: agent %q: %q is not an environment variable name", ErrInvalid, sp.Name, e)
		}
		if slices.Contains(hidden, e) {
			return fmt.Errorf("%w: agent %q: %s holds a model key and is never passed to agents", ErrInvalid, sp.Name, e)
		}
	}
	if err := llm.CheckEscalation(sp.Command); err != nil {
		return fmt.Errorf("%w: agent %q: %v", ErrInvalid, sp.Name, err)
	}
	switch kind {
	case "cli":
		if len(sp.Command) == 0 || strings.TrimSpace(sp.Command[0]) == "" {
			return fmt.Errorf("%w: agent %q needs a command (todo agent add %s -- <program> [args])", ErrInvalid, sp.Name, sp.Name)
		}
	case "http":
		u, err := url.Parse(sp.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: agent %q: url %q must be an http(s) URL", ErrInvalid, sp.Name, llm.RedactURL(sp.URL))
		}
		if u.User != nil {
			return fmt.Errorf("%w: agent %q: put credentials in an environment variable (--header-env), not in the url", ErrInvalid, sp.Name)
		}
		sp.Method = strings.ToUpper(strings.TrimSpace(sp.Method))
		if sp.Method == "" {
			sp.Method = "POST"
		}
		if !slices.Contains([]string{"POST", "PUT", "PATCH"}, sp.Method) {
			return fmt.Errorf("%w: agent %q: method must be POST, PUT or PATCH", ErrInvalid, sp.Name)
		}
		for h, e := range sp.HeaderEnv {
			if !headerRE.MatchString(h) || !envNameRE.MatchString(e) {
				return fmt.Errorf("%w: agent %q: header_env %q=%q needs a header name and an environment variable name", ErrInvalid, sp.Name, h, e)
			}
			if slices.Contains(hidden, e) {
				return fmt.Errorf("%w: agent %q: %s holds a model key and is never sent to agents", ErrInvalid, sp.Name, e)
			}
		}
	}
	return nil
}

// timeout is the attempt timeout of a spec.
func timeout(sp Spec) time.Duration {
	if sp.TimeoutSeconds > 0 {
		return time.Duration(sp.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// successCodes are the exit codes that count as success.
func successCodes(sp Spec) []int {
	if len(sp.SuccessCodes) == 0 {
		return []int{0}
	}
	return sp.SuccessCodes
}
