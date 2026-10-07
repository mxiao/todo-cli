package cli

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/llm"
)

func cmdAI(a *app, args []string) error {
	return a.runGroup("ai", []subcommand{
		{"add", "<text...> [--select ID]... [--mode M]", "Create or change tasks from natural language (single or many)", aiAdd},
		{"answer", "<session> <text...>", "Answer the model's clarifying questions (asked once at most)", aiAnswer},
		{"decide", "[--select ID]... [--mode M] [--feedback F]", "Summary, risks, suggested order and changes for the open tasks", aiDecide},
		{"redecide", "<session> [feedback...]", "Reject a decision and decide again", aiRedecide},
		{"show", "<session>", "Show a session with its items, diffs and result", aiShow},
		{"list", "[--kind intake|decide|optimize] [--limit N]", "List recent model sessions", aiList},
		{"apply", "<session> [item...]", "Accept items (all pending ones without numbers)", aiApply},
		{"reject", "<session> [item...]", "Reject / ignore items (all pending ones without numbers)", aiReject},
		{"undo", "<session> [item...]", "Undo applied items (task changes or configuration changes)", aiUndo},
		{"edit", "<session> <item> [--title T] [--due D] [--priority P] [--tag T]... [--to V]", "Edit a proposed item before saving it", aiEdit},
		{"optimize", "[--profile P]", "Let the model analyse its own decisions and propose improvements", aiOptimize},
		{"config", "[show|versions|restore <v>|rollback|set <target> <json>]", "Show or version the assistant configuration (prompts, ranking, routing, failure handling)", aiConfig},
		{"stats", "", "Decision-effect statistics used for self-optimisation", aiStats},
	}, "", args)
}

func modeFlag(m **string) func(*fset) {
	return func(f *fset) {
		*m = f.String("mode", "", "permission mode for this request: suggest|confirm|auto (default: configured mode)")
	}
}

func parseModeFlag(v string) (llm.Mode, error) {
	if v == "" {
		return "", nil
	}
	return llm.ParseMode(v)
}

func aiAdd(a *app, args []string) error {
	var mode, profile *string
	var sel *stringList
	pos, err := a.subFlags("ai", "add", "<text...>", func(f *fset) {
		modeFlag(&mode)(f)
		profile = f.String("profile", "", "model profile (default: active)")
		sel = f.List("select", "selected task id: the model may update it or add subtasks (repeatable)")
	}, args)
	if err != nil {
		return err
	}
	text := strings.Join(pos, " ")
	if text == "-" || text == "" && !a.isTerminal() {
		b, err := io.ReadAll(a.env.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return usagef("usage: todo ai add <描述要做的事…>  (or pipe the text on stdin)")
	}
	m, err := parseModeFlag(*mode)
	if err != nil {
		return err
	}
	ids, err := a.resolve(*sel)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	sess, err := svc.Intake(ctx, llm.IntakeInput{Text: text, Selected: ids, Mode: m, Profile: *profile})
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiAnswer(a *app, args []string) error {
	pos, err := a.subFlags("ai", "answer", "<session> <text...>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("usage: todo ai answer <session> <回答…>")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	sess, err := svc.Answer(ctx, pos[0], strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiDecide(a *app, args []string) error {
	var mode, profile, feedback *string
	var sel *stringList
	if _, err := a.subFlags("ai", "decide", "", func(f *fset) {
		modeFlag(&mode)(f)
		profile = f.String("profile", "", "model profile (default: active)")
		feedback = f.String("feedback", "", "extra guidance for the decision")
		sel = f.List("select", "focus on these task ids (repeatable)")
	}, args); err != nil {
		return err
	}
	m, err := parseModeFlag(*mode)
	if err != nil {
		return err
	}
	ids, err := a.resolve(*sel)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	sess, err := svc.Decide(ctx, llm.DecideInput{Selected: ids, Mode: m, Profile: *profile, Feedback: *feedback})
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiRedecide(a *app, args []string) error {
	pos, err := a.subFlags("ai", "redecide", "<session> [feedback...]", nil, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return usagef("usage: todo ai redecide <session> [反馈…]")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	sess, err := svc.Redecide(ctx, pos[0], strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiShow(a *app, args []string) error {
	pos, err := a.subFlags("ai", "show", "<session>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo ai show <session>")
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	sess, err := svc.Session(pos[0])
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiList(a *app, args []string) error {
	var kind *string
	var limit *int
	if _, err := a.subFlags("ai", "list", "", func(f *fset) {
		kind = f.String("kind", "", "intake, decide or optimize")
		limitFlag(&limit)(f)
	}, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	list, err := svc.Sessions(*kind, *limit)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"sessions": list})
	}
	if len(list) == 0 {
		a.printf("没有大模型会话\n")
	}
	for _, s := range list {
		text := s.Input
		if text == "" {
			text = s.Summary
		}
		deg := ""
		if s.Degraded {
			deg = " [本地规则]"
		}
		a.printf("%s  %s  %-8s %-11s %d 项（待处理 %d）%s  %s\n", s.ID, a.localTime(s.CreatedAt), s.Kind, statusLabel(s.Status), s.Items, s.Pending, deg, text)
	}
	return nil
}

// itemArgs parses "<session> [item...]" (numbers may be "1,2" or "#3").
func itemArgs(group, name string, pos []string) (string, []int, error) {
	if len(pos) < 1 {
		return "", nil, usagef("usage: todo %s %s <session> [item...]", group, name)
	}
	var ns []int
	for _, p := range pos[1:] {
		for _, part := range strings.Split(p, ",") {
			part = strings.TrimPrefix(strings.TrimSpace(part), "#")
			if part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 {
				return "", nil, usagef("%s %s: %q is not an item number", group, name, part)
			}
			ns = append(ns, n)
		}
	}
	return pos[0], ns, nil
}

func aiApply(a *app, args []string) error {
	return a.itemAction("apply", args, func(svc *llm.Service, id string, ns []int) (*llm.Session, error) {
		ctx, cancel := a.ctx()
		defer cancel()
		return svc.Apply(ctx, id, ns)
	})
}

func aiReject(a *app, args []string) error {
	return a.itemAction("reject", args, func(svc *llm.Service, id string, ns []int) (*llm.Session, error) {
		return svc.Reject(id, ns)
	})
}

func aiUndo(a *app, args []string) error {
	return a.itemAction("undo", args, func(svc *llm.Service, id string, ns []int) (*llm.Session, error) {
		return svc.Undo(id, ns)
	})
}

func (a *app) itemAction(name string, args []string, fn func(*llm.Service, string, []int) (*llm.Session, error)) error {
	pos, err := a.subFlags("ai", name, "<session> [item...]", nil, args)
	if err != nil {
		return err
	}
	id, ns, err := itemArgs("ai", name, pos)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	sess, err := fn(svc, id, ns)
	if sess != nil {
		if perr := a.printSession(sess); perr != nil {
			return perr
		}
	}
	return err
}

func aiEdit(a *app, args []string) error {
	var title, desc, due, prio, cat, to *string
	var tags, deps *stringList
	var fs *fset
	pos, err := a.subFlags("ai", "edit", "<session> <item>", func(f *fset) {
		fs = f
		title = f.String("title", "", "new title")
		desc = f.String("desc", "", "new description")
		due = f.String("due", "", "due time (明天 18:00, fri, 2026-10-09; empty clears)")
		prio = f.String("priority", "", "priority (none|low|medium|high|urgent, 高/中/低)")
		cat = f.String("category", "", "category")
		tags = f.List("tag", "tags (replace the proposed ones; repeatable)")
		deps = f.List("dep", "dependencies: task ids or new-task refs N1 (repeatable)")
		to = f.String("to", "", "new target value of a decision change")
	}, args)
	if err != nil {
		return err
	}
	id, ns, err := itemArgs("ai", "edit", pos)
	if err != nil {
		return err
	}
	if len(ns) != 1 {
		return usagef("usage: todo ai edit <session> <item> [flags]")
	}
	var e llm.ItemEdit
	set := func(name string, v *string) *string {
		if flagSet(fs.FlagSet, name) {
			return v
		}
		return nil
	}
	e.Title, e.Description, e.Due, e.Priority, e.Category, e.To = set("title", title), set("desc", desc), set("due", due), set("priority", prio), set("category", cat), set("to", to)
	if flagSet(fs.FlagSet, "tag") {
		t := []string(*tags)
		e.Tags = &t
	}
	if flagSet(fs.FlagSet, "dep") {
		d := []string(*deps)
		e.DependsOn = &d
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	sess, err := svc.EditItem(id, ns[0], e)
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiOptimize(a *app, args []string) error {
	var profile *string
	if _, err := a.subFlags("ai", "optimize", "", func(f *fset) { profile = f.String("profile", "", "model profile") }, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx()
	defer cancel()
	sess, err := svc.Optimize(ctx, *profile)
	if err != nil {
		return err
	}
	return a.printSession(sess)
}

func aiStats(a *app, args []string) error {
	if _, err := a.subFlags("ai", "stats", "", nil, args); err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	st, err := svc.Stats()
	if err != nil {
		return err
	}
	return a.emitJSON(st) // structured either way
}

func aiConfig(a *app, args []string) error {
	pos, err := a.subFlags("ai", "config", "[show|versions|restore <v>|rollback|set <target> <json>]", nil, args)
	if err != nil {
		return err
	}
	svc, err := a.llm()
	if err != nil {
		return err
	}
	verb := "show"
	if len(pos) > 0 {
		verb, pos = pos[0], pos[1:]
	}
	var cv *llm.ConfigVersion
	switch verb {
	case "show":
		cfg, v, err := svc.AssistConfig()
		if err != nil {
			return err
		}
		if a.json {
			return a.emitJSON(map[string]any{"version": v, "config": cfg})
		}
		a.printf("助手配置 v%d\n", v)
		b, _ := json.MarshalIndent(cfg, "", "  ")
		a.printf("%s\n", b)
		return nil
	case "versions":
		vs, err := svc.AssistVersions()
		if err != nil {
			return err
		}
		if a.json {
			return a.emitJSON(map[string]any{"versions": vs})
		}
		for _, v := range vs {
			when := "内置"
			if !v.CreatedAt.IsZero() {
				when = a.localTime(v.CreatedAt)
			}
			a.printf("v%-3d %-10s %s  %s\n", v.Version, v.Source, when, v.Note)
		}
		return nil
	case "restore":
		if len(pos) != 1 {
			return usagef("usage: todo ai config restore <version>")
		}
		n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "v"))
		if err != nil {
			return usagef("ai config restore: %q is not a version", pos[0])
		}
		if cv, err = svc.RestoreAssistVersion(n); err != nil {
			return err
		}
	case "rollback":
		if cv, err = svc.RollbackAssist(); err != nil {
			return err
		}
	case "set":
		if len(pos) != 2 {
			return usagef("usage: todo ai config set <%s> <json>", strings.Join(llm.OptimizeTargets, "|"))
		}
		if cv, err = svc.SetAssistValue(pos[0], json.RawMessage(pos[1])); err != nil {
			return err
		}
	default:
		return usagef("todo ai config: unknown action %q", verb)
	}
	if a.json {
		return a.emitJSON(cv)
	}
	a.printf("助手配置已更新为 v%d（%s：%s）；可用 `todo ai config rollback` 恢复上一版本\n", cv.Version, cv.Source, cv.Note)
	return nil
}

// ---- output ----

var statusLabels = map[string]string{
	llm.StatusNeedsInput: "待回答", llm.StatusPending: "待确认", llm.StatusApplied: "已处理", llm.StatusPartial: "部分处理",
	llm.StatusRejected: "已拒绝", llm.StatusUndone: "已撤销", llm.StatusSuperseded: "已重新决策",
}

var itemLabels = map[string]string{
	llm.ItemPending: "待确认", llm.ItemApplied: "已执行", llm.ItemRejected: "已拒绝", llm.ItemSkipped: "未执行",
	llm.ItemFailed: "失败", llm.ItemUndone: "已撤销",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

var kindLabels = map[string]string{
	llm.ItemCreate: "新增", llm.ItemSubtask: "子任务", llm.ItemUpdate: "修改", llm.ItemChange: "调整",
	llm.ItemCommand: "命令", llm.ItemAgent: "智能体", llm.ItemOptimize: "优化",
}

func fmtVal(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		if x == "" {
			return "—"
		}
		return x
	case []string:
		if len(x) == 0 {
			return "—"
		}
		return strings.Join(x, ", ")
	case []any:
		var parts []string
		for _, p := range x {
			parts = append(parts, fmtVal(p))
		}
		if len(parts) == 0 {
			return "—"
		}
		return strings.Join(parts, ", ")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// printSession shows a model session for review: questions, summary,
// risks, the suggested order, each proposed change with its before/after
// diff, and the counts of created, updated, not executed and failed items.
func (a *app) printSession(s *llm.Session) error {
	if a.json {
		return a.emitJSON(s)
	}
	a.printf("会话 %s  %s · %s · %s\n", s.ID, s.Kind, s.Mode.Label(), statusLabel(s.Status))
	if s.Degraded {
		a.printf("⚠ 大模型不可用，以下为本地规则结果\n")
	}
	if s.Status == llm.StatusNeedsInput {
		a.printf("需要补充信息（只问这一次）：\n")
		for i, q := range s.Questions {
			a.printf("  %d. %s\n", i+1, q)
		}
		a.printf("回答：todo ai answer %s <回答…>    放弃：todo ai reject %s\n", s.ID, s.ID)
		return nil
	}
	if s.Summary != "" {
		a.printf("摘要：%s\n", s.Summary)
	}
	if len(s.Risks) > 0 {
		a.printf("风险：\n")
		for _, r := range s.Risks {
			a.printf("  [%s] %s %s\n", r.Level, r.Title, r.Message)
		}
	}
	if len(s.Recommendations) > 0 {
		a.printf("建议顺序：\n")
		for _, r := range s.Recommendations {
			a.printf("  %d. %s  — %s", r.Order, r.Title, r.Reason)
			if r.Focus != "" {
				a.printf("；关注：%s", r.Focus)
			}
			if r.Estimate != "" {
				a.printf("；预计 %s", r.Estimate)
			}
			a.printf("\n")
		}
	}
	if len(s.Items) > 0 {
		a.printf("变更：\n")
	}
	for _, it := range s.Items {
		title := it.TaskTitle
		if it.Fields != nil && it.Fields.Title != nil && it.Kind != llm.ItemUpdate {
			title = *it.Fields.Title
		}
		if it.Optimize != nil {
			title = it.Optimize.Target
		}
		if it.Command != nil && title == "" {
			title = strings.Join(it.Command.Argv, " ")
		}
		a.printf("  #%d [%s] %s %s\n", it.N, itemLabels[it.Status], kindLabels[it.Kind], title)
		for _, d := range it.Diff {
			if lines, ok := d.After.([]string); ok && d.Before == nil && strings.HasSuffix(d.Field, "(lines)") {
				for _, l := range lines {
					if !strings.HasPrefix(l, "  ") {
						a.printf("       %s\n", l)
					}
				}
				continue
			}
			a.printf("       %s: %s → %s\n", d.Field, fmtVal(d.Before), fmtVal(d.After))
		}
		if o := it.Optimize; o != nil {
			a.printf("       问题：%s\n       方案：%s\n       预期影响：%s\n       验证：%s\n       回滚：%s\n", o.Problem, o.Proposal, o.ExpectedImpact, o.Verification, o.Rollback)
		}
		if it.Command != nil && it.Command.Prompt != "" && it.Status == llm.ItemPending {
			a.printf("       提示词：%s\n", strings.ReplaceAll(strings.TrimSpace(it.Command.Prompt), "\n", "\n               "))
		}
		if it.Reason != "" && it.Optimize == nil {
			a.printf("       理由：%s\n", it.Reason)
		}
		if it.NeedsConfirm && it.Status == llm.ItemPending && it.ConfirmReason != "" {
			a.printf("       需确认：%s\n", it.ConfirmReason)
		}
		if it.Message != "" {
			a.printf("       %s\n", it.Message)
		}
	}
	for _, w := range s.Warnings {
		a.printf("提示：%s\n", w)
	}
	if r := s.Result; r != nil && len(s.Items) > 0 {
		a.printf("结果：新增 %d，修改 %d，未执行 %d，失败 %d\n", r.Created, r.Updated, r.NotExecuted, r.Failed)
	}
	pending := false
	for _, it := range s.Items {
		if it.Status == llm.ItemPending {
			pending = true
		}
	}
	switch {
	case pending && s.Kind == llm.KindDecide:
		a.printf("接受：todo ai apply %s [编号…]  忽略：todo ai reject %s  重新决策：todo ai redecide %s [反馈]\n", s.ID, s.ID, s.ID)
	case pending:
		a.printf("确认：todo ai apply %s [编号…]  拒绝：todo ai reject %s [编号…]  修改：todo ai edit %s <编号> --title …\n", s.ID, s.ID, s.ID)
	case s.Status == llm.StatusApplied || s.Status == llm.StatusPartial:
		a.printf("撤销：todo ai undo %s [编号…]\n", s.ID)
	}
	return nil
}
