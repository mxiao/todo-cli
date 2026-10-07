package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ActionRecord is the audit entry of a model-initiated command or agent
// start (FR-605, NFR-036).
type ActionRecord struct {
	ID        int64          `json:"id"`
	SessionID string         `json:"session_id"`
	Item      int            `json:"item"`
	Kind      string         `json:"kind"`
	Request   CommandSpec    `json:"request"`
	Status    string         `json:"status"`
	Result    *CommandResult `json:"result,omitempty"`
	Actor     string         `json:"actor"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// runCommandItem runs an accepted command or agent item and records it.
func (s *Service) runCommandItem(ctx context.Context, sess *Session, it *Item, by string) {
	st, err := s.Settings()
	if err != nil {
		it.Status, it.Message = ItemFailed, err.Error()
		return
	}
	if err := checkEscalation(it.Command.Argv); err != nil {
		it.Status, it.Message = ItemFailed, err.Error()
		return
	}
	cfg, _, _ := s.AssistConfig()
	spec := CommandSpec{Argv: it.Command.Argv, Dir: it.Command.Dir, Timeout: time.Duration(cfg.Failure.CommandTimeoutSeconds) * time.Second,
		HideEnv: []string{EnvAPIKey}}
	for _, p := range st.Profiles {
		if p.APIKeyEnv != "" {
			spec.HideEnv = append(spec.HideEnv, p.APIKeyEnv)
		}
	}
	if it.Kind == ItemAgent {
		if it.Command.Prompt == "" {
			it.Command.Prompt = s.agentPrompt(it)
		}
		spec.Stdin = it.Command.Prompt
	}
	req, _ := json.Marshal(spec)
	now := fmtTime(s.store.Now())
	res, err := s.db().Exec(`INSERT INTO llm_actions(session_id, item, kind, request, status, actor, created_at, updated_at) VALUES (?, ?, ?, ?, 'running', ?, ?, ?)`,
		sess.ID, it.N, it.Kind, s.Redact(string(req)), Actor+"/"+by, now, now)
	if err != nil {
		it.Status, it.Message = ItemFailed, err.Error()
		return
	}
	it.ActionID, _ = res.LastInsertId()
	out := s.opts.Runner.Run(ctx, spec)
	out.Stdout, out.Stderr, out.Error = s.Redact(out.Stdout), s.Redact(out.Stderr), s.Redact(out.Error)
	status := "succeeded"
	it.Status, it.AppliedBy, it.NeedsConfirm = ItemApplied, by, false
	it.Message = fmt.Sprintf("退出码 %d", out.ExitCode)
	if out.ExitCode != 0 || out.Error != "" {
		status, it.Status = "failed", ItemFailed
		if out.Error != "" {
			it.Message += "：" + out.Error
		} else if e := strings.TrimSpace(out.Stderr); e != "" {
			it.Message += "：" + truncate(e, 200)
		}
	}
	it.Command.Result = &out
	b, _ := json.Marshal(out)
	_, _ = s.db().Exec(`UPDATE llm_actions SET status = ?, result = ?, updated_at = ? WHERE id = ?`, status, string(b), fmtTime(s.store.Now()), it.ActionID)
}

// agentPrompt is the prompt passed to an agent on standard input; it is
// shown to the user before the agent starts (FR-506).
func (s *Service) agentPrompt(it *Item) string {
	var b strings.Builder
	if t, err := s.store.Get(it.TaskID); err == nil {
		fmt.Fprintf(&b, "任务：%s\n", t.Title)
		if t.Description != "" {
			fmt.Fprintf(&b, "描述：%s\n", t.Description)
		}
		if len(t.Tags) > 0 {
			fmt.Fprintf(&b, "标签：%s\n", strings.Join(t.Tags, ", "))
		}
	}
	if it.Reason != "" {
		fmt.Fprintf(&b, "要求：%s\n", it.Reason)
	}
	b.WriteString("完成后输出结果摘要；未实际完成时不要声称已完成。\n")
	return s.Redact(b.String())
}

// Actions lists recent command and agent runs, newest first.
func (s *Service) Actions(limit int) ([]ActionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db().Query(`SELECT id, session_id, item, kind, request, status, result, actor, created_at, updated_at
		FROM llm_actions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActionRecord{}
	for rows.Next() {
		var a ActionRecord
		var req, result, created, updated string
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Item, &a.Kind, &req, &a.Status, &result, &a.Actor, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(req), &a.Request)
		if result != "{}" {
			a.Result = &CommandResult{}
			_ = json.Unmarshal([]byte(result), a.Result)
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, a)
	}
	return out, rows.Err()
}
