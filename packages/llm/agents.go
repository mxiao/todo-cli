package llm

import (
	"context"
	"time"
)

// AgentLauncher starts agent runs for accepted agent items (implemented by
// packages/agent). Without one, an accepted agent item runs its command
// directly and synchronously.
type AgentLauncher interface {
	// PreviewAgentPrompt returns the final prompt an agent would receive
	// for a task, so the user sees it before accepting (FR-506).
	PreviewAgentPrompt(ctx context.Context, req AgentLaunch) (string, error)
	// LaunchAgent starts a run. The prompt shown in the item is the prompt
	// the agent receives.
	LaunchAgent(ctx context.Context, req AgentLaunch) (*AgentLaunchResult, error)
}

// AgentLaunch asks for an agent run on a task.
type AgentLaunch struct {
	SessionID string `json:"session_id,omitempty"`
	Item      int    `json:"item,omitempty"`
	TaskID    string `json:"task_id"`
	Agent     string `json:"agent"`
	// Reason is the model's reason, passed on as context.
	Reason string `json:"reason,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// By is who accepted the item (user, auto).
	By string `json:"by,omitempty"`
}

// AgentLaunchResult reports the started run.
type AgentLaunchResult struct {
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// SetAgentLauncher routes accepted agent items to l.
func (s *Service) SetAgentLauncher(l AgentLauncher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launcher = l
}

func (s *Service) agentLauncher() AgentLauncher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launcher
}

// KeyEnvNames lists the environment variables that hold model keys; agents
// and commands never inherit them.
func (s *Service) KeyEnvNames() []string {
	names := []string{EnvAPIKey}
	if st, err := s.Settings(); err == nil {
		for _, p := range st.Profiles {
			if p.APIKeyEnv != "" {
				names = append(names, p.APIKeyEnv)
			}
		}
	}
	return names
}

// CheckEscalation refuses command lines that would raise privileges (sudo,
// su, doas …): agents and commands only ever have the current user's
// permissions.
func CheckEscalation(argv []string) error { return checkEscalation(argv) }

// Ask sends one system/user exchange to a model profile and returns the
// plain text answer, with the same redaction and auditing as the built-in
// features (used by prompt agents).
func (s *Service) Ask(ctx context.Context, purpose, profile, system, user string) (string, error) {
	c, r, err := s.client(profile)
	if err != nil {
		return "", err
	}
	msgs := []Message{{Role: "system", Content: s.Redact(system)}, {Role: "user", Content: s.Redact(user)}}
	start := time.Now()
	resp, err := c.Complete(ctx, Request{Purpose: purpose, Messages: msgs})
	s.recordCall("", purpose, r, time.Since(start), err)
	if err != nil {
		if e, ok := AsError(err); ok {
			return "", s.withAlternatives(s.redactError(e), r.Name)
		}
		return "", err
	}
	return s.Redact(resp.Content), nil
}
