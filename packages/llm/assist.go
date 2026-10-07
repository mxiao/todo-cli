package llm

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// AssistConfig is the model's own, self-optimisable configuration: prompt
// guidance, ranking rules, agent routing and failure handling. Every change
// is a new stored version, so any change can be undone or rolled back
// (FR-408). Permissions are deliberately not part of it: optimisation can
// never widen what the model may do (NFR-026).
type AssistConfig struct {
	Prompts PromptSet     `json:"prompts"`
	Ranking RankingRules  `json:"ranking"`
	Routing []RouteRule   `json:"routing"`
	Failure FailurePolicy `json:"failure"`
}

// PromptSet holds the guidance prepended to each task's system prompt; the
// response format itself is fixed in code.
type PromptSet struct {
	Intake    string `json:"intake"`
	Decide    string `json:"decide"`
	Summarize string `json:"summarize"`
	Optimize  string `json:"optimize"`
}

// RankingRules weight the signals of the local ranking, which orders the
// tasks given to the model and answers on its own when the model is
// unavailable.
type RankingRules struct {
	OverdueWeight      float64 `json:"overdue_weight"`
	DueSoonHours       float64 `json:"due_soon_hours"`
	DueSoonWeight      float64 `json:"due_soon_weight"`
	PriorityWeight     float64 `json:"priority_weight"`
	InProgressBonus    float64 `json:"in_progress_bonus"`
	BlockedPenalty     float64 `json:"blocked_penalty"`
	ReopenWeight       float64 `json:"reopen_weight"`
	MaxRecommendations int     `json:"max_recommendations"`
}

// RouteRule picks an agent for matching tasks: "tag:NAME" or a keyword.
type RouteRule struct {
	Match string `json:"match"`
	Agent string `json:"agent"`
	Note  string `json:"note,omitempty"`
}

// FailurePolicy controls retries of model calls and commands.
type FailurePolicy struct {
	MaxRetries            int    `json:"max_retries"`
	CommandTimeoutSeconds int    `json:"command_timeout_seconds"`
	OnRepeatedFailure     string `json:"on_repeated_failure"` // ask | skip | stop
}

// DefaultAssistConfig is version 0.
func DefaultAssistConfig() AssistConfig {
	return AssistConfig{
		Prompts: PromptSet{
			Intake: "你是待办任务录入助手。只提取用户明确说出的信息；截止时间、优先级、执行要求不明确时不要编造，宁可留空或提问。" +
				"一段话包含多件事时拆成多条任务；有先后顺序时用 depends_on 表示依赖。",
			Decide: "你是任务决策助手。优先处理已逾期、临近截止、被其他任务依赖的高优先级任务；进行中的任务优先收尾；" +
				"被未完成依赖阻塞的任务不要排在前面。理由要简短具体。",
			Summarize: "把长提示词整理为可复用模板：保留关键要求和约束，删除重复与无关内容，每次需要替换的内容用 {{变量名}} 占位。",
			Optimize: "根据执行记录找出提示词歧义、排序规则不合理、智能体选择错误、参数缺失或重复失败模式，" +
				"每条建议说明问题、改进方案、预期影响、验证方式和回滚方案。",
		},
		Ranking: RankingRules{OverdueWeight: 100, DueSoonHours: 48, DueSoonWeight: 40, PriorityWeight: 10,
			InProgressBonus: 15, BlockedPenalty: 50, ReopenWeight: 5, MaxRecommendations: 5},
		Routing: []RouteRule{},
		Failure: FailurePolicy{MaxRetries: 1, CommandTimeoutSeconds: 120, OnRepeatedFailure: "ask"},
	}
}

// Validate checks value ranges.
func (c *AssistConfig) Validate() error {
	r := c.Ranking
	for name, v := range map[string]float64{"overdue_weight": r.OverdueWeight, "due_soon_hours": r.DueSoonHours,
		"due_soon_weight": r.DueSoonWeight, "priority_weight": r.PriorityWeight, "in_progress_bonus": r.InProgressBonus,
		"blocked_penalty": r.BlockedPenalty, "reopen_weight": r.ReopenWeight} {
		if v < 0 || v > 10000 {
			return fmt.Errorf("%w: ranking.%s must be between 0 and 10000", ErrInvalid, name)
		}
	}
	if r.MaxRecommendations < 1 || r.MaxRecommendations > 20 {
		return fmt.Errorf("%w: ranking.max_recommendations must be 1-20", ErrInvalid)
	}
	for _, p := range []string{c.Prompts.Intake, c.Prompts.Decide, c.Prompts.Summarize, c.Prompts.Optimize} {
		if len(p) > 8000 {
			return fmt.Errorf("%w: prompt guidance is limited to 8000 bytes", ErrInvalid)
		}
	}
	for _, rr := range c.Routing {
		if strings.TrimSpace(rr.Match) == "" || strings.TrimSpace(rr.Agent) == "" {
			return fmt.Errorf("%w: a routing rule needs match and agent", ErrInvalid)
		}
	}
	f := c.Failure
	if f.MaxRetries < 0 || f.MaxRetries > 5 {
		return fmt.Errorf("%w: failure.max_retries must be 0-5", ErrInvalid)
	}
	if f.CommandTimeoutSeconds < 1 || f.CommandTimeoutSeconds > 3600 {
		return fmt.Errorf("%w: failure.command_timeout_seconds must be 1-3600", ErrInvalid)
	}
	if !slices.Contains([]string{"ask", "skip", "stop"}, f.OnRepeatedFailure) {
		return fmt.Errorf("%w: failure.on_repeated_failure must be ask, skip or stop", ErrInvalid)
	}
	return nil
}

// OptimizeTargets are the configuration parts a suggestion may change.
var OptimizeTargets = []string{"prompts.intake", "prompts.decide", "prompts.summarize", "prompts.optimize", "ranking", "routing", "failure"}

// Get returns the JSON value of a target.
func (c *AssistConfig) Get(target string) (json.RawMessage, error) {
	var v any
	switch target {
	case "prompts.intake":
		v = c.Prompts.Intake
	case "prompts.decide":
		v = c.Prompts.Decide
	case "prompts.summarize":
		v = c.Prompts.Summarize
	case "prompts.optimize":
		v = c.Prompts.Optimize
	case "ranking":
		v = c.Ranking
	case "routing":
		v = c.Routing
	case "failure":
		v = c.Failure
	default:
		return nil, fmt.Errorf("%w: unknown optimisation target %q (want %s)", ErrInvalid, target, strings.Join(OptimizeTargets, ", "))
	}
	return json.Marshal(v)
}

// Set replaces a target with a JSON value, strictly decoded and validated.
func (c *AssistConfig) Set(target string, raw json.RawMessage) error {
	next := *c
	next.Routing = slices.Clone(c.Routing)
	strict := func(dst any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("%w: value for %s: %v", ErrInvalid, target, err)
		}
		return nil
	}
	var err error
	switch target {
	case "prompts.intake":
		err = strict(&next.Prompts.Intake)
	case "prompts.decide":
		err = strict(&next.Prompts.Decide)
	case "prompts.summarize":
		err = strict(&next.Prompts.Summarize)
	case "prompts.optimize":
		err = strict(&next.Prompts.Optimize)
	case "ranking":
		err = strict(&next.Ranking)
	case "routing":
		next.Routing = nil
		err = strict(&next.Routing)
		if next.Routing == nil {
			next.Routing = []RouteRule{}
		}
	case "failure":
		err = strict(&next.Failure)
	default:
		_, err = c.Get(target)
	}
	if err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

// ConfigVersion is one stored assistant configuration.
type ConfigVersion struct {
	Version   int          `json:"version"`
	Source    string       `json:"source"` // default | suggestion | undo | restore | manual
	Note      string       `json:"note,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	Config    AssistConfig `json:"config"`
}

// AssistConfig returns the current configuration and its version (0 = the
// built-in default).
func (s *Service) AssistConfig() (AssistConfig, int, error) {
	var raw string
	var v int
	err := s.db().QueryRow(`SELECT version, config FROM llm_config_versions ORDER BY version DESC LIMIT 1`).Scan(&v, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultAssistConfig(), 0, nil
	}
	if err != nil {
		return AssistConfig{}, 0, err
	}
	cfg := DefaultAssistConfig()
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AssistConfig{}, 0, fmt.Errorf("assistant config v%d: %w", v, err)
	}
	return cfg, v, nil
}

// AssistVersions lists every configuration version, oldest first,
// starting with the built-in default (version 0).
func (s *Service) AssistVersions() ([]ConfigVersion, error) {
	out := []ConfigVersion{{Version: 0, Source: "default", Config: DefaultAssistConfig()}}
	rows, err := s.db().Query(`SELECT version, config, source, note, created_at FROM llm_config_versions ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cv ConfigVersion
		var raw, created string
		if err := rows.Scan(&cv.Version, &raw, &cv.Source, &cv.Note, &created); err != nil {
			return nil, err
		}
		cv.Config = DefaultAssistConfig()
		if err := json.Unmarshal([]byte(raw), &cv.Config); err != nil {
			return nil, err
		}
		cv.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, cv)
	}
	return out, rows.Err()
}

func (s *Service) assistVersion(v int) (*ConfigVersion, error) {
	all, err := s.AssistVersions()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Version == v {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: assistant config version %d", ErrNotFound, v)
}

func (s *Service) saveAssistConfig(cfg AssistConfig, source, note string) (int, error) {
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	tx, err := s.db().Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur sql.NullInt64
	if err := tx.QueryRow(`SELECT max(version) FROM llm_config_versions`).Scan(&cur); err != nil {
		return 0, err
	}
	v := int(cur.Int64) + 1
	if _, err := tx.Exec(`INSERT INTO llm_config_versions(version, config, source, note, created_at) VALUES (?, ?, ?, ?, ?)`,
		v, string(b), source, Redact(note), fmtTime(s.store.Now())); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// RestoreAssistVersion makes an earlier version current again, as a new
// version (nothing is overwritten).
func (s *Service) RestoreAssistVersion(v int) (*ConfigVersion, error) {
	old, err := s.assistVersion(v)
	if err != nil {
		return nil, err
	}
	nv, err := s.saveAssistConfig(old.Config, "restore", fmt.Sprintf("restore v%d", v))
	if err != nil {
		return nil, err
	}
	return s.assistVersion(nv)
}

// RollbackAssist restores the version before the current one (恢复上一版本).
func (s *Service) RollbackAssist() (*ConfigVersion, error) {
	_, cur, err := s.AssistConfig()
	if err != nil {
		return nil, err
	}
	if cur == 0 {
		return nil, fmt.Errorf("%w: the assistant configuration is still the built-in default", ErrState)
	}
	return s.RestoreAssistVersion(cur - 1)
}

// SetAssistValue changes one target manually (source "manual").
func (s *Service) SetAssistValue(target string, raw json.RawMessage) (*ConfigVersion, error) {
	cfg, _, err := s.AssistConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.Set(target, raw); err != nil {
		return nil, err
	}
	v, err := s.saveAssistConfig(cfg, "manual", "set "+target)
	if err != nil {
		return nil, err
	}
	return s.assistVersion(v)
}

// ValueDiff compares two JSON values of a target for review: prompts give
// a line diff, objects one entry per changed field.
func ValueDiff(target string, before, after json.RawMessage) []Diff {
	var a, b any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return []Diff{{Field: target, Before: as, After: bs}, {Field: target + " (lines)", Before: nil, After: LineDiff(as, bs)}}
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		var names []string
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		var out []Diff
		for _, k := range names {
			ja, _ := json.Marshal(am[k])
			jb, _ := json.Marshal(bm[k])
			if !bytes.Equal(ja, jb) {
				out = append(out, Diff{Field: target + "." + k, Before: am[k], After: bm[k]})
			}
		}
		return out
	}
	return []Diff{{Field: target, Before: a, After: b}}
}

// LineDiff is a minimal line diff ("  same", "- removed", "+ added").
func LineDiff(a, b string) []string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, "  "+x[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "- "+x[i])
			i++
		default:
			out = append(out, "+ "+y[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+x[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+y[j])
	}
	return out
}
