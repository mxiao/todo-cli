package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Suggestion is a self-optimisation proposal for one part of the
// assistant configuration (FR-407).
type Suggestion struct {
	Target         string          `json:"target"`
	Problem        string          `json:"problem"`
	Proposal       string          `json:"proposal"`
	ExpectedImpact string          `json:"expected_impact"`
	Verification   string          `json:"verification"`
	Rollback       string          `json:"rollback"`
	Value          json.RawMessage `json:"value"`
	// Before is the target's value when the suggestion was applied; undo
	// restores it.
	Before json.RawMessage `json:"before,omitempty"`
}

const optimizeFormat = `Return one JSON object only:
{"suggestions": [{
  "target": "prompts.intake|prompts.decide|prompts.summarize|prompts.optimize|ranking|routing|failure",
  "problem": "...", "proposal": "...", "expected_impact": "...", "verification": "...", "rollback": "...",
  "value": ...   // the complete new value of the target, same shape as in current_config
}]}
Base every suggestion on the statistics. Return an empty list when nothing should change.`

// OptimizeStats summarises how the model's past proposals fared.
type OptimizeStats struct {
	Sessions       map[string]int            `json:"sessions"`
	Items          map[string]map[string]int `json:"items"`
	Fields         map[string]map[string]int `json:"fields"`
	Degraded       int                       `json:"degraded"`
	Undone         int                       `json:"undone"`
	Rejected       []string                  `json:"rejected_examples,omitempty"`
	Failures       []string                  `json:"failure_examples,omitempty"`
	Warnings       []string                  `json:"warning_examples,omitempty"`
	ModelErrors    map[string]int            `json:"model_errors"`
	CommandResults map[string]int            `json:"command_results"`
}

// Stats collects the decision-effect statistics used by Optimize.
func (s *Service) Stats() (*OptimizeStats, error) {
	st := &OptimizeStats{Sessions: map[string]int{}, Items: map[string]map[string]int{}, Fields: map[string]map[string]int{},
		ModelErrors: map[string]int{}, CommandResults: map[string]int{}}
	sessions, err := s.loadSessions(`SELECT data FROM llm_sessions WHERE kind != ? ORDER BY created_at DESC LIMIT 200`, KindOptimize)
	if err != nil {
		return nil, err
	}
	bump := func(m map[string]map[string]int, k, v string) {
		if m[k] == nil {
			m[k] = map[string]int{}
		}
		m[k][v]++
	}
	for _, sess := range sessions {
		st.Sessions[sess.Kind]++
		if sess.Degraded {
			st.Degraded++
		}
		if sess.Status == StatusUndone {
			st.Undone++
		}
		for _, w := range sess.Warnings {
			if len(st.Warnings) < 10 {
				st.Warnings = append(st.Warnings, truncate(w, 160))
			}
		}
		for _, it := range sess.Items {
			bump(st.Items, sess.Kind+"."+it.Kind, it.Status)
			if it.Change != nil {
				bump(st.Fields, it.Change.Field, it.Status)
			}
			if it.Command != nil && it.Command.Result != nil {
				if it.Command.Result.ExitCode == 0 {
					st.CommandResults["ok"]++
				} else {
					st.CommandResults["failed"]++
				}
			}
			switch it.Status {
			case ItemRejected, ItemUndone:
				if len(st.Rejected) < 10 {
					st.Rejected = append(st.Rejected, truncate(fmt.Sprintf("%s %s %s: %s", sess.Kind, it.Kind, diffLine(it.Diff), it.Reason), 200))
				}
			case ItemFailed, ItemSkipped:
				if len(st.Failures) < 10 {
					st.Failures = append(st.Failures, truncate(fmt.Sprintf("%s %s: %s", sess.Kind, it.Kind, it.Message), 200))
				}
			}
		}
	}
	rows, err := s.db().Query(`SELECT status, count(*) FROM llm_calls WHERE status != 'ok' GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		st.ModelErrors[k] = n
	}
	return st, rows.Err()
}

func diffLine(ds []Diff) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, fmt.Sprintf("%s %v→%v", d.Field, d.Before, d.After))
	}
	return strings.Join(parts, "; ")
}

// Optimize asks the model to review its own decision effect and propose
// changes to prompts, ranking rules, agent routing and failure handling.
// Suggestions always wait for the user: they are applied, rejected or
// undone like other items and every application is a new config version.
func (s *Service) Optimize(ctx context.Context, profile string) (*Session, error) {
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	cfg, cfgVersion, err := s.AssistConfig()
	if err != nil {
		return nil, err
	}
	stats, err := s.Stats()
	if err != nil {
		return nil, err
	}
	c, r, err := s.client(profile)
	if err != nil {
		return nil, err
	}
	sess := &Session{ID: newSessionID(), Kind: KindOptimize, Mode: st.Mode, Origin: s.opts.Origin, Profile: r.Name, Model: r.Model,
		ConfigVersion: cfgVersion}
	user := map[string]any{"statistics": stats, "current_config": cfg, "config_version": cfgVersion}
	var resp struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	system := cfg.Prompts.Optimize + "\n\n" + optimizeFormat
	if err := s.ask(ctx, c, r, sess.ID, "optimize", []Message{{Role: "system", Content: system}, {Role: "user", Content: mustJSON(user)}}, &resp); err != nil {
		return nil, err
	}
	for _, sg := range resp.Suggestions {
		sg.Target = strings.TrimSpace(sg.Target)
		before, err := cfg.Get(sg.Target)
		if err != nil {
			sess.Warnings = append(sess.Warnings, "忽略无效的优化对象："+sg.Target)
			continue
		}
		trial := cfg
		trial.Routing = slices.Clone(cfg.Routing)
		if err := trial.Set(sg.Target, sg.Value); err != nil {
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("忽略 %s 的建议：%v", sg.Target, err))
			continue
		}
		after, _ := trial.Get(sg.Target)
		if bytes.Equal(before, after) {
			continue
		}
		sg.Value = after
		for _, p := range []*string{&sg.Problem, &sg.Proposal, &sg.ExpectedImpact, &sg.Verification, &sg.Rollback} {
			*p = s.Redact(truncate(*p, 600))
		}
		if sg.Rollback == "" {
			sg.Rollback = "撤销该建议或恢复上一配置版本"
		}
		sess.Items = append(sess.Items, Item{N: len(sess.Items) + 1, Kind: ItemOptimize, Status: ItemPending, Optimize: &sg,
			Reason: sg.Problem, Diff: ValueDiff(sg.Target, before, after)})
	}
	sess.Summary = fmt.Sprintf("基于 %d 次会话的执行记录，提出 %d 条优化建议", sum(stats.Sessions), len(sess.Items))
	return s.finish(ctx, sess, st)
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// applySuggestion writes the suggested value as a new config version.
func (s *Service) applySuggestion(sess *Session, it *Item, by string) {
	cfg, _, err := s.AssistConfig()
	if err == nil {
		var before json.RawMessage
		if before, err = cfg.Get(it.Optimize.Target); err == nil {
			if err = cfg.Set(it.Optimize.Target, it.Optimize.Value); err == nil {
				var v int
				if v, err = s.saveAssistConfig(cfg, "suggestion", fmt.Sprintf("%s #%d: %s", sess.ID, it.N, it.Optimize.Proposal)); err == nil {
					it.Optimize.Before, it.ConfigVersion = before, v
					it.Status, it.AppliedBy, it.NeedsConfirm = ItemApplied, by, false
					it.Message = fmt.Sprintf("已应用为配置版本 v%d", v)
					return
				}
			}
		}
	}
	it.Status, it.Message = ItemFailed, err.Error()
}

// undoSuggestion restores the value from before the suggestion, unless the
// target changed again since.
func (s *Service) undoSuggestion(it *Item) error {
	cfg, _, err := s.AssistConfig()
	if err != nil {
		return err
	}
	cur, err := cfg.Get(it.Optimize.Target)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur, it.Optimize.Value) {
		return fmt.Errorf("%w: %s changed after this suggestion was applied; use `todo ai config restore <version>`", ErrState, it.Optimize.Target)
	}
	if err := cfg.Set(it.Optimize.Target, it.Optimize.Before); err != nil {
		return err
	}
	v, err := s.saveAssistConfig(cfg, "undo", fmt.Sprintf("undo %s (v%d)", it.Optimize.Target, it.ConfigVersion))
	if err != nil {
		return err
	}
	it.Status, it.Message = ItemUndone, fmt.Sprintf("已撤销（配置版本 v%d）", v)
	return nil
}
