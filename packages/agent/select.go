package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

const selectSystem = `你为待办任务选择最合适的智能体。只能从给出的 agents 中选择；参考每个智能体的说明和 routing 规则。
只返回 JSON：{"agent":"名称","reason":"一句话理由"}；没有合适的智能体时 agent 为空字符串并说明原因。`

// SelectAgent lets the model choose an agent for a task (FR-501). Without
// a usable model it falls back to the routing rules of the assistant
// configuration, then to the only configured agent.
func (m *Manager) SelectAgent(ctx context.Context, t *core.Task) (string, Selection, error) {
	agents, err := m.Agents()
	if err != nil {
		return "", Selection{}, err
	}
	if len(agents) == 0 {
		return "", Selection{}, fmt.Errorf("%w: 尚未配置任何智能体，无法自动选择（todo agent add <名称> -- <命令>）", ErrNotFound)
	}
	cfg, _, _ := m.svc.AssistConfig()
	var list []map[string]string
	for _, a := range agents {
		list = append(list, map[string]string{"name": a.Name, "description": a.Description, "adapter": a.AdapterName()})
	}
	user, _ := json.Marshal(map[string]any{
		"task":    map[string]any{"title": m.redact(t.Title, nil), "description": m.redact(clip(t.Description, 1000), nil), "tags": t.Tags},
		"agents":  list,
		"routing": cfg.Routing,
	})
	var resp struct {
		Agent  string `json:"agent"`
		Reason string `json:"reason"`
	}
	degraded := false
	if err := m.svc.AskJSON(ctx, "agent_select", "", selectSystem, string(user), &resp); err != nil {
		// Unavailable or unusable answer: fall back to the local rules.
		degraded = true
	} else {
		for _, a := range agents {
			if strings.EqualFold(a.Name, strings.TrimSpace(resp.Agent)) {
				return a.Name, Selection{By: "llm", Reason: m.redact(clip(resp.Reason, 300), nil)}, nil
			}
		}
	}
	text := strings.ToLower(t.Title + " " + t.Description)
	for _, rr := range cfg.Routing {
		match := strings.ToLower(strings.TrimSpace(rr.Match))
		hit := false
		if tag, ok := strings.CutPrefix(match, "tag:"); ok {
			for _, tg := range t.Tags {
				hit = hit || strings.EqualFold(tg, tag)
			}
		} else {
			hit = match != "" && strings.Contains(text, match)
		}
		if !hit {
			continue
		}
		for _, a := range agents {
			if strings.EqualFold(a.Name, rr.Agent) {
				return a.Name, Selection{By: "routing", Reason: "匹配 " + rr.Match, Degraded: degraded}, nil
			}
		}
	}
	if len(agents) == 1 {
		return agents[0].Name, Selection{By: "only", Degraded: degraded}, nil
	}
	why := "大模型没有选出合适的智能体"
	if resp.Reason != "" {
		why += "：" + m.redact(resp.Reason, nil)
	}
	if degraded {
		why = "大模型不可用，且没有路由规则匹配"
	}
	return "", Selection{}, fmt.Errorf("%w: 无法自动选择智能体（%s）；请用 --agent 指定", ErrInvalid, why)
}

func (m *Manager) renderTemplate(ref string, vars map[string]string) (string, error) {
	t, err := m.opts.Prompts.Get(ref)
	if err != nil {
		return "", err
	}
	return prompt.Render(t.Latest.Body, t.Latest.Summary.Variables, vars)
}

// PreviewAgentPrompt implements llm.AgentLauncher: the prompt shown in a
// model session before an agent item is accepted.
func (m *Manager) PreviewAgentPrompt(ctx context.Context, req llm.AgentLaunch) (string, error) {
	r, err := m.Start(ctx, launchRequest(req, true))
	if err != nil {
		return "", err
	}
	return r.Prompt, nil
}

// LaunchAgent implements llm.AgentLauncher: an accepted agent item of a
// model decision starts a run (FR-501). The web server runs it in the
// background; the CLI waits for it.
func (m *Manager) LaunchAgent(ctx context.Context, req llm.AgentLaunch) (*llm.AgentLaunchResult, error) {
	r, err := m.Start(ctx, launchRequest(req, false))
	if err != nil {
		return nil, err
	}
	if r.Status == StatusQueued {
		if m.opts.Async {
			m.Background(r.ID)
		} else if err := m.Execute(ctx, r.ID); err != nil && !errors.Is(err, ErrState) {
			return nil, err
		}
	}
	if r, err = m.Get(r.ID); err != nil {
		return nil, err
	}
	return &llm.AgentLaunchResult{RunID: r.ID, Status: r.Status, Message: r.Error}, nil
}

func launchRequest(req llm.AgentLaunch, dry bool) StartRequest {
	sr := StartRequest{TaskID: req.TaskID, Agent: req.Agent, Initiator: llm.Actor, SessionID: req.SessionID, Item: req.Item,
		DryRun: dry, Confirmed: req.By == "user", Selection: &Selection{By: "llm", Reason: req.Reason}}
	if req.Reason != "" {
		sr.Context = "大模型建议：" + req.Reason
	}
	if !dry {
		sr.Prompt = req.Prompt
	}
	return sr
}
