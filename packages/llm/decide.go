package llm

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// DecideInput asks the model what to do next.
type DecideInput struct {
	Selected []string
	Mode     Mode
	Profile  string
	// Feedback is passed on a re-decision ("太激进了", "先做 A").
	Feedback string
}

const decideFormat = `Return one JSON object only:
{
  "summary": "...",                                  // short overview of the current tasks
  "risks": [{"task": "T1", "level": "high|medium|low", "message": "..."}],
  "recommendations": [{"task": "T1", "reason": "...", "focus": "...", "estimate": "30m"}],  // in the order to work on them
  "changes": [{"task": "T1", "field": "priority|due_at|status|depends_on|order|delete", "to": ..., "reason": "..."}],
  "actions": [{"type": "command", "argv": ["prog", "arg"], "reason": "..."},
              {"type": "agent", "agent": "name", "task": "T1", "reason": "..."}]
}
"to" values: priority none|low|medium|high|urgent; due_at "YYYY-MM-DD HH:MM" local time or "" to clear;
status todo|in_progress|done|archived; depends_on a list of task refs; order the 1-based position in the suggested order.
Only propose changes that clearly help; give every change a short reason. Use only the task refs given.`

// Decide analyses the open tasks and proposes a summary, risks, an order of
// work and concrete changes (FR-401…404). When the model is unavailable it
// falls back to the local ranking rules and says so (Degraded).
func (s *Service) Decide(ctx context.Context, in DecideInput) (*Session, error) {
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	mode := st.Mode
	if in.Mode != "" {
		mode = in.Mode
	}
	sess := &Session{ID: newSessionID(), Kind: KindDecide, Mode: mode, Selected: in.Selected, Origin: s.opts.Origin,
		Feedback: s.Redact(in.Feedback), Profile: in.Profile}
	return s.runDecide(ctx, sess, st, nil)
}

// Redecide replaces a decision with a new one, telling the model what was
// rejected and the user's feedback (重新决策).
func (s *Service) Redecide(ctx context.Context, id, feedback string) (*Session, error) {
	old, err := s.Session(id)
	if err != nil {
		return nil, err
	}
	if old.Kind != KindDecide {
		return nil, fmt.Errorf("%w: session %s is not a decision", ErrInvalid, old.ID)
	}
	st, err := s.Settings()
	if err != nil {
		return nil, err
	}
	for i := range old.Items {
		if old.Items[i].Status == ItemPending {
			old.Items[i].Status, old.Items[i].Message = ItemRejected, "已重新决策"
		}
	}
	sess := &Session{ID: newSessionID(), Kind: KindDecide, Mode: old.Mode, Selected: old.Selected, Origin: s.opts.Origin,
		Feedback: s.Redact(feedback), Supersedes: old.ID, Profile: old.Profile}
	next, err := s.runDecide(ctx, sess, st, old)
	if err != nil {
		return nil, err
	}
	old.Status, old.SupersededBy = StatusSuperseded, next.ID
	old.Result = old.tally()
	return next, s.saveSession(old)
}

type decideResponse struct {
	Summary string `json:"summary"`
	Risks   []struct {
		Task    string `json:"task"`
		Level   string `json:"level"`
		Message string `json:"message"`
	} `json:"risks"`
	Recommendations []struct {
		Task     string `json:"task"`
		Reason   string `json:"reason"`
		Focus    string `json:"focus"`
		Estimate string `json:"estimate"`
	} `json:"recommendations"`
	Changes []struct {
		Task   string `json:"task"`
		Field  string `json:"field"`
		To     any    `json:"to"`
		Reason string `json:"reason"`
	} `json:"changes"`
	Actions []struct {
		Type   string   `json:"type"`
		Argv   []string `json:"argv"`
		Agent  string   `json:"agent"`
		Task   string   `json:"task"`
		Reason string   `json:"reason"`
	} `json:"actions"`
}

func (s *Service) runDecide(ctx context.Context, sess *Session, st Settings, previous *Session) (*Session, error) {
	cfg, cfgVersion, err := s.AssistConfig()
	if err != nil {
		return nil, err
	}
	sess.ConfigVersion = cfgVersion
	signals, err := s.historySignals()
	if err != nil {
		return nil, err
	}
	var ranked []scored
	tasks, refOf, refs, err := s.candidates(sess.Selected, 60, func(ts []core.Task) []core.Task {
		ranked = rankTasks(ts, cfg.Ranking, s.now(), signals)
		out := make([]core.Task, len(ranked))
		for i, r := range ranked {
			out[i] = r.task
		}
		return out
	})
	if err != nil {
		return nil, err
	}
	sess.Refs = refs
	c, r, err := s.client(sess.Profile)
	if err == nil {
		sess.Profile, sess.Model = r.Name, r.Model
		err = s.modelDecision(ctx, c, r, sess, st, cfg, tasks, refOf, ranked, signals, previous)
	}
	if err != nil {
		if _, ok := AsError(err); !ok {
			return nil, err
		}
		s.localDecision(sess, cfg.Ranking, ranked)
		sess.Degraded, sess.DegradedReason = true, err.Error()
		sess.Warnings = append(sess.Warnings, "大模型不可用，已按本地排序规则给出建议："+err.Error())
	}
	return s.finish(ctx, sess, st)
}

func (s *Service) modelDecision(ctx context.Context, c Client, r *Resolved, sess *Session, st Settings, cfg AssistConfig,
	tasks []core.Task, refOf map[string]string, ranked []scored, signals map[string]taskSignals, previous *Session) error {
	fields := append(slices.Clone(st.SendFields), st.DecisionFields...)
	view := s.taskContext(tasks, fields, refOf, sess.Selected)
	blocked := map[string]bool{}
	for _, r := range ranked {
		blocked[r.task.ID] = r.blocked
	}
	for i, t := range tasks {
		if sig := signals[t.ID]; sig.Reopened > 0 {
			view[i]["reopened"] = sig.Reopened
		}
		if blocked[t.ID] {
			view[i]["blocked"] = true
		}
	}
	user := map[string]any{"tasks": view, "ranking_rules": cfg.Ranking}
	if len(sess.Selected) > 0 {
		var sel []string
		for _, id := range sess.Selected {
			sel = append(sel, refOf[id])
		}
		user["selected"] = sel
	}
	if len(st.Agents) > 0 {
		var agents []map[string]string
		for _, a := range st.Agents {
			agents = append(agents, map[string]string{"name": a.Name, "description": a.Description})
		}
		user["agents"], user["routing"] = agents, cfg.Routing
	}
	if previous != nil {
		var rejected []string
		for _, it := range previous.Items {
			rejected = append(rejected, itemLabel(&it)+" "+fmt.Sprint(it.Change))
		}
		user["previous_decision"] = map[string]any{"summary": previous.Summary, "rejected": rejected}
	}
	if sess.Feedback != "" {
		user["feedback"] = sess.Feedback
	}
	system := cfg.Prompts.Decide + "\n\nCurrent local time: " + s.nowLine() + "\n\n" + decideFormat
	if !st.AllowCommands && len(st.Agents) == 0 {
		system += "\nDo not propose actions."
	}
	var resp decideResponse
	if err := s.ask(ctx, c, r, sess.ID, "decide", []Message{{Role: "system", Content: system}, {Role: "user", Content: mustJSON(user)}}, &resp); err != nil {
		return err
	}
	sess.Summary = s.Redact(truncate(resp.Summary, 1000))
	byID := map[string]*core.Task{}
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	for _, rk := range resp.Risks {
		risk := Risk{Level: normLevel(rk.Level), Message: s.Redact(truncate(rk.Message, 300))}
		if id, ok := sess.Refs[rk.Task]; ok {
			risk.TaskID, risk.Title = id, byID[id].Title
		}
		sess.Risks = append(sess.Risks, risk)
	}
	for _, rec := range resp.Recommendations {
		id, ok := sess.Refs[rec.Task]
		if !ok {
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("建议引用了不存在的任务 %q，已忽略", rec.Task))
			continue
		}
		sess.Recommendations = append(sess.Recommendations, Recommendation{Order: len(sess.Recommendations) + 1, TaskID: id,
			Title: byID[id].Title, Reason: s.Redact(truncate(rec.Reason, 300)), Focus: s.Redact(truncate(rec.Focus, 200)), Estimate: truncate(rec.Estimate, 40)})
	}
	n := 0
	for _, ch := range resp.Changes {
		id, ok := sess.Refs[ch.Task]
		if !ok {
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("变更引用了不存在的任务 %q，已忽略", ch.Task))
			continue
		}
		field := normField(ch.Field)
		to, err := s.changeValue(field, ch.To, sess.Refs, false)
		if err != nil {
			sess.Warnings = append(sess.Warnings, fmt.Sprintf("%s 的 %s 变更无效：%v", byID[id].Title, ch.Field, err))
			continue
		}
		change := &Change{Field: field, To: to}
		d := s.changeDiff(sess, byID[id], change)
		if field != "order" && field != "delete" && fmt.Sprint(d.Before) == fmt.Sprint(d.After) {
			continue // no-op
		}
		n++
		sess.Items = append(sess.Items, Item{N: n, Kind: ItemChange, Status: ItemPending, TaskID: id, TaskTitle: byID[id].Title,
			Change: change, Reason: s.Redact(truncate(ch.Reason, 300)), Diff: []Diff{d}})
	}
	for _, a := range resp.Actions {
		it := Item{N: n + 1, Status: ItemPending, Reason: s.Redact(truncate(a.Reason, 300)), Command: &Command{}}
		switch strings.ToLower(a.Type) {
		case "command", "cli", "shell":
			if !st.AllowCommands {
				sess.Warnings = append(sess.Warnings, "命令调用未开启（todo llm commands on），已忽略模型提出的命令")
				continue
			}
			if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
				continue
			}
			it.Kind, it.Command.Argv, it.Command.Dir = ItemCommand, a.Argv, s.commandDir(st)
			it.Diff = []Diff{{Field: "command", After: strings.Join(a.Argv, " ")}}
		case "agent":
			agent := findAgent(st, a.Agent)
			id := sess.Refs[a.Task]
			it.Kind, it.TaskID = ItemAgent, id
			if t := byID[id]; t != nil {
				it.TaskTitle = t.Title
			}
			if agent == nil {
				it.Status = ItemSkipped
				it.Message = fmt.Sprintf("智能体 %q 未配置，能力不可用（todo llm agent add）", a.Agent)
				it.Command.Agent = a.Agent
			} else {
				it.Command.Agent, it.Command.Argv, it.Command.Dir = agent.Name, agent.Command, agent.Dir
				if it.Command.Dir == "" {
					it.Command.Dir = s.commandDir(st)
				}
				it.Command.Prompt = s.agentPrompt(&it)
				if l := s.agentLauncher(); l != nil {
					if p, err := l.PreviewAgentPrompt(ctx, AgentLaunch{SessionID: sess.ID, Item: it.N, TaskID: id, Agent: agent.Name, Reason: it.Reason}); err == nil {
						it.Command.Prompt = p
					} else {
						sess.Warnings = append(sess.Warnings, fmt.Sprintf("智能体 %s 的提示词预览失败：%v", agent.Name, err))
					}
				}
				it.Diff = []Diff{{Field: "agent", After: agent.Name + "：" + strings.Join(agent.Command, " ")}}
			}
		default:
			continue
		}
		n++
		sess.Items = append(sess.Items, it)
	}
	return nil
}

func normLevel(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "high", "高", "critical":
		return "high"
	case "low", "低":
		return "low"
	}
	return "medium"
}

func normField(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "due", "deadline", "due_at":
		return "due_at"
	case "deps", "dependencies", "depends_on":
		return "depends_on"
	case "order", "position", "rank":
		return "order"
	}
	return strings.ToLower(strings.TrimSpace(f))
}

// changeValue validates and normalises a change's target value.
func (s *Service) changeValue(field string, to any, refs map[string]string, user bool) (any, error) {
	str := strings.TrimSpace(fmt.Sprint(to))
	if to == nil {
		str = ""
	}
	switch field {
	case "priority":
		p, err := parsePriority(str)
		if err != nil {
			return nil, err
		}
		return p.String(), nil
	case "due_at":
		if str == "" {
			return "", nil
		}
		t, err := s.parseDue(str, "")
		if err != nil {
			return nil, err
		}
		return t.Format(time.RFC3339), nil
	case "status":
		st, err := core.ParseStatus(str)
		if err != nil {
			return nil, err
		}
		return string(st), nil
	case "depends_on":
		var out []string
		list := toStrings(to)
		if user {
			list = strings.FieldsFunc(str, func(r rune) bool { return r == ',' || r == ' ' })
		}
		for _, r := range list {
			r = strings.TrimSpace(r)
			if id, ok := refs[r]; ok {
				out = append(out, id)
			} else if id, err := s.store.ResolveID(r); err == nil && user {
				out = append(out, id)
			} else {
				return nil, fmt.Errorf("unknown task %q", r)
			}
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	case "order":
		n, ok := toInt(to)
		if !ok || n < 1 {
			return nil, fmt.Errorf("order must be a positive number")
		}
		return n, nil
	case "delete":
		return true, nil
	}
	return nil, fmt.Errorf("unsupported field %q (priority, due_at, status, depends_on, order, delete)", field)
}

// changeDiff shows a change before and after (FR-404).
func (s *Service) changeDiff(sess *Session, t *core.Task, c *Change) Diff {
	d := Diff{Field: c.Field}
	switch c.Field {
	case "priority":
		d.Before, d.After = t.Priority.String(), c.To
	case "due_at":
		if t.DueAt != nil {
			d.Before = s.localTime(*t.DueAt)
		}
		if v, _ := c.To.(string); v != "" {
			due, _ := time.Parse(time.RFC3339, v)
			d.After = s.localTime(due)
		}
	case "status":
		d.Before, d.After = string(t.Status), c.To
	case "depends_on":
		d.Before, d.After = s.refTitles(sess, t.DependsOn), s.refTitles(sess, toStrings(c.To))
	case "order":
		d.Before, d.After = "当前位置", fmt.Sprintf("第 %v 位", c.To)
	case "delete":
		d.Before, d.After = t.Title, "（删除）"
	}
	return d
}

func findAgent(st Settings, name string) *AgentProfile {
	for i := range st.Agents {
		if strings.EqualFold(st.Agents[i].Name, name) {
			return &st.Agents[i]
		}
	}
	return nil
}

// ---- local ranking ----

type taskSignals struct {
	Reopened int
}

// historySignals counts how often tasks were reopened after being done, a
// sign of failed attempts (历史执行结果).
func (s *Service) historySignals() (map[string]taskSignals, error) {
	rows, err := s.db().Query(`SELECT task_id, count(*) FROM task_history WHERE action IN ('reopen', 'undo_complete') GROUP BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]taskSignals{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = taskSignals{Reopened: n}
	}
	return out, rows.Err()
}

type scored struct {
	task       core.Task
	score      float64
	reasons    []string
	overdue    bool
	dueSoon    bool
	blocked    bool
	dependents int
}

// rankTasks orders open tasks by the ranking rules: overdue and soon-due
// work first, then priority, in-progress work and tasks others wait for;
// blocked tasks drop.
func rankTasks(tasks []core.Task, rules RankingRules, now time.Time, signals map[string]taskSignals) []scored {
	open := map[string]bool{}
	dependents := map[string]int{}
	for _, t := range tasks {
		open[t.ID] = true
	}
	for _, t := range tasks {
		for _, d := range t.DependsOn {
			dependents[d]++
		}
	}
	out := make([]scored, 0, len(tasks))
	for _, t := range tasks {
		sc := scored{task: t, dependents: dependents[t.ID]}
		sc.score += float64(t.Priority) * rules.PriorityWeight
		if t.Priority >= core.PriorityHigh {
			sc.reasons = append(sc.reasons, "优先级"+map[core.Priority]string{core.PriorityHigh: "高", core.PriorityUrgent: "紧急"}[t.Priority])
		}
		if t.DueAt != nil {
			left := t.DueAt.Sub(now).Hours()
			switch {
			case left < 0:
				sc.overdue = true
				sc.score += rules.OverdueWeight
				sc.reasons = append(sc.reasons, "已逾期 "+humanHours(-left))
			case left <= rules.DueSoonHours && rules.DueSoonHours > 0:
				sc.dueSoon = true
				sc.score += rules.DueSoonWeight * (1 - left/rules.DueSoonHours)
				sc.reasons = append(sc.reasons, humanHours(left)+"后截止")
			}
		}
		if t.Status == core.StatusInProgress {
			sc.score += rules.InProgressBonus
			sc.reasons = append(sc.reasons, "进行中，建议先收尾")
		}
		if sc.dependents > 0 {
			sc.score += float64(sc.dependents) * rules.PriorityWeight / 2
			sc.reasons = append(sc.reasons, fmt.Sprintf("有 %d 个任务依赖它", sc.dependents))
		}
		for _, d := range t.DependsOn {
			if open[d] {
				sc.blocked = true
			}
		}
		if sc.blocked {
			sc.score -= rules.BlockedPenalty
			sc.reasons = append(sc.reasons, "依赖的任务尚未完成")
		}
		if n := signals[t.ID].Reopened; n > 0 {
			sc.score += float64(n) * rules.ReopenWeight
			sc.reasons = append(sc.reasons, fmt.Sprintf("曾 %d 次重新打开", n))
		}
		out = append(out, sc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

func humanHours(h float64) string {
	switch {
	case h < 1:
		return fmt.Sprintf("%d 分钟", int(math.Max(1, h*60)))
	case h < 48:
		return fmt.Sprintf("%d 小时", int(h))
	}
	return fmt.Sprintf("%d 天", int(h/24))
}

// localDecision answers from the ranking alone (model unavailable).
func (s *Service) localDecision(sess *Session, rules RankingRules, ranked []scored) {
	overdue, soon, blocked := 0, 0, 0
	for _, r := range ranked {
		switch {
		case r.overdue:
			overdue++
			sess.Risks = append(sess.Risks, Risk{TaskID: r.task.ID, Title: r.task.Title, Level: "high", Message: "已逾期"})
		case r.dueSoon:
			soon++
			sess.Risks = append(sess.Risks, Risk{TaskID: r.task.ID, Title: r.task.Title, Level: "medium", Message: "即将截止"})
		}
		if r.blocked {
			blocked++
			if r.overdue || r.dueSoon {
				sess.Risks = append(sess.Risks, Risk{TaskID: r.task.ID, Title: r.task.Title, Level: "high", Message: "临近截止但被未完成的依赖阻塞"})
			}
		}
	}
	sess.Summary = fmt.Sprintf("共 %d 个未完成任务：%d 个已逾期，%d 个即将截止，%d 个被依赖阻塞。（本地规则）", len(ranked), overdue, soon, blocked)
	for _, r := range ranked {
		if len(sess.Recommendations) >= rules.MaxRecommendations {
			break
		}
		if r.blocked {
			continue
		}
		reason := strings.Join(r.reasons, "，")
		if reason == "" {
			reason = "按手动顺序"
		}
		focus := ""
		if r.task.DueAt != nil {
			focus = "截止 " + s.localTime(*r.task.DueAt)
		}
		sess.Recommendations = append(sess.Recommendations, Recommendation{Order: len(sess.Recommendations) + 1,
			TaskID: r.task.ID, Title: r.task.Title, Reason: reason, Focus: focus})
	}
}

func (s *Service) commandDir(st Settings) string {
	if st.CommandDir != "" {
		return st.CommandDir
	}
	return ""
}
