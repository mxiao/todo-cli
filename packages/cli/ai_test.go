package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

const cliKey = "sk-cli-test-secret-987654321"

// modelHarness is a harness whose model answers from a script.
func modelHarness(t *testing.T, responses ...any) (*harness, *llm.Scripted) {
	h := newHarness(t)
	model := llm.NewScripted(responses...)
	h.hooks.NewClient = func(*llm.Resolved) (llm.Client, error) { return model, nil }
	h.env[EnvModelAPIKey] = cliKey
	return h, model
}

func (h *harness) fail(args ...string) (stderr string, code int) {
	h.t.Helper()
	_, errOut, code := h.run(args...)
	if code == 0 {
		h.t.Fatalf("todo %v succeeded, want failure", args)
	}
	return errOut, code
}

func (h *harness) session(args ...string) llm.Session {
	h.t.Helper()
	var s llm.Session
	h.json(&s, args...)
	return s
}

func TestAIAddConfirmApplyAndUndo(t *testing.T) {
	h, model := modelHarness(t, map[string]any{"items": []map[string]any{
		{"action": "create", "ref": "N1", "title": "整理需求", "priority": "high", "due": "2026-10-07 18:00", "due_text": "明天 18:00"},
		{"action": "create", "ref": "N2", "title": "更新页面", "depends_on": []string{"N1"}},
	}})
	out := h.ok("ai", "add", "明天", "18:00", "前整理需求（高优先级），然后更新页面")
	for _, want := range []string{"执行前确认", "#1 [待确认] 新增 整理需求", "priority: — → high", "depends_on: — → N1 整理需求",
		"结果：新增 0，修改 0，未执行 2，失败 0", "todo ai apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(h.list()) != 0 {
		t.Fatal("tasks created before confirmation")
	}
	if strings.Contains(model.LastRequest().Messages[1].Content, cliKey) {
		t.Fatal("key sent to the model")
	}
	list := struct {
		Sessions []llm.SessionSummary `json:"sessions"`
	}{}
	h.json(&list, "ai", "list")
	id := list.Sessions[0].ID

	// Edit before saving, then accept everything.
	h.ok("ai", "edit", id, "2", "--title", "更新官网页面", "--tag", "web")
	s := h.session("ai", "apply", id)
	if s.Result.Created != 2 || s.Status != llm.StatusApplied {
		t.Fatalf("apply: %+v", s.Result)
	}
	eq(t, h.list(), []string{"整理需求", "更新官网页面"})
	var shown taskOut
	h.json(&shown, "show", s.Items[1].ResultTaskID)
	if shown.Task.DependsOn[0] != s.Items[0].ResultTaskID || shown.History[0].Actor != llm.Actor || shown.Task.Tags[0] != "web" {
		t.Fatalf("task %+v history %+v", shown.Task, shown.History)
	}
	out = h.ok("ai", "undo", id)
	if !strings.Contains(out, "已撤销") || len(h.list()) != 0 {
		t.Fatalf("undo:\n%s\n%v", out, h.list())
	}
}

func TestAIAddAsksOnceAndAutoMode(t *testing.T) {
	h, _ := modelHarness(t,
		map[string]any{"questions": []string{"截止时间？"}, "items": []map[string]any{{"action": "create", "title": "交报告"}}},
		map[string]any{"items": []map[string]any{{"action": "create", "title": "交报告", "due": "2026-10-09 18:00", "due_text": "周五 18:00"}}})
	h.ok("llm", "mode", "auto")
	out := h.ok("ai", "add", "交报告")
	if !strings.Contains(out, "需要补充信息") || !strings.Contains(out, "截止时间？") {
		t.Fatalf("question:\n%s", out)
	}
	list := struct {
		Sessions []llm.SessionSummary `json:"sessions"`
	}{}
	h.json(&list, "ai", "list")
	s := h.session("ai", "answer", list.Sessions[0].ID, "周五", "18:00")
	if s.Status != llm.StatusApplied || s.Result.Created != 1 || s.Items[0].AppliedBy != "auto" {
		t.Fatalf("auto after answer: %+v", s)
	}
	eq(t, h.list(), []string{"交报告"})
	errOut, _ := h.fail("ai", "answer", s.ID, "again")
	if !strings.Contains(errOut, "invalid_state") && !strings.Contains(errOut, "does not wait") {
		t.Fatalf("second answer: %s", errOut)
	}
}

func TestAIDecideAcceptRejectRedecide(t *testing.T) {
	h, model := modelHarness(t)
	a := h.add("修 bug", "--priority", "low")
	b := h.add("写方案")
	model.Push(func(req llm.Request) any {
		ra, rb := refByTitle(t, req, "修 bug"), refByTitle(t, req, "写方案")
		return map[string]any{"summary": "先修 bug", "recommendations": []map[string]any{{"task": ra, "reason": "影响用户", "focus": "回归测试", "estimate": "1h"}},
			"risks": []map[string]any{{"task": ra, "level": "high", "message": "影响线上"}},
			"changes": []map[string]any{{"task": ra, "field": "priority", "to": "urgent", "reason": "影响用户"},
				{"task": rb, "field": "status", "to": "in_progress", "reason": "开始写"}}}
	}, map[string]any{"summary": "保守", "changes": []map[string]any{}})
	out := h.ok("ai", "decide")
	for _, want := range []string{"摘要：先修 bug", "风险：", "建议顺序：", "1. 修 bug  — 影响用户；关注：回归测试；预计 1h", "priority: low → urgent", "todo ai redecide"} {
		if !strings.Contains(out, want) {
			t.Errorf("decide output lacks %q:\n%s", want, out)
		}
	}
	list := struct {
		Sessions []llm.SessionSummary `json:"sessions"`
	}{}
	h.json(&list, "ai", "list", "--kind", "decide")
	id := list.Sessions[0].ID
	h.ok("ai", "apply", id, "1")
	var got taskOut
	h.json(&got, "show", a.ID)
	if got.Task.Priority != core.PriorityUrgent {
		t.Fatalf("accept one: %+v", got.Task)
	}
	h.ok("ai", "reject", id, "2")
	h.json(&got, "show", b.ID)
	if got.Task.Status != core.StatusTodo {
		t.Fatal("rejected change applied")
	}
	next := h.session("ai", "redecide", id, "别动状态")
	if next.Supersedes != id || next.Summary != "保守" {
		t.Fatalf("redecide %+v", next)
	}
}

func TestAIWithoutModelDegradesAndKeepsTasksWorking(t *testing.T) {
	h := newHarness(t)
	h.add("逾期任务", "--due", "2026-10-01")
	h.add("普通任务")
	errOut, code := h.fail("--json", "ai", "add", "写周报")
	if code != ExitError || !strings.Contains(errOut, llm.KindNotConfigured) {
		t.Fatalf("intake without model: %d %s", code, errOut)
	}
	_, errOut, _ = h.run("ai", "add", "写周报")
	if !strings.Contains(errOut, "普通任务管理不受影响") {
		t.Fatalf("human error: %s", errOut)
	}
	out := h.ok("ai", "decide")
	if !strings.Contains(out, "本地规则") || !strings.Contains(out, "1. 逾期任务") {
		t.Fatalf("degraded decide:\n%s", out)
	}
	h.add("模型不可用时也能加")
	if n := len(h.list()); n != 3 {
		t.Fatalf("tasks %d", n)
	}
	out = h.ok("llm", "status")
	if !strings.Contains(out, "不可用") {
		t.Fatalf("llm status:\n%s", out)
	}
}

func TestLLMConfigureTestRetryAndSwitch(t *testing.T) {
	h := newHarness(t)
	fails := 2
	backup := llm.NewScripted("OK", "OK")
	h.hooks.NewClient = func(r *llm.Resolved) (llm.Client, error) {
		if r.Name == "backup" {
			return backup, nil
		}
		return llm.Func(func(context.Context, llm.Request) (*llm.Response, error) {
			if fails > 0 {
				fails--
				return nil, &llm.Error{Kind: llm.KindAuth, Message: "模型服务拒绝了访问凭据（HTTP 401）", Hint: "检查 API Key"}
			}
			return &llm.Response{Content: "OK"}, nil
		}), nil
	}
	// The key is read from stdin into the keychain, never printed.
	h.stdin = cliKey + "\n"
	out := h.ok("llm", "set-key")
	if strings.Contains(out, cliKey) || !strings.Contains(out, "钥匙串") {
		t.Fatalf("set-key:\n%s", out)
	}
	if k, _ := h.hooks.Secrets.Get(llm.DefaultProfile); k != cliKey {
		t.Fatal("key not in the keychain")
	}
	settings, _ := os.ReadFile(filepath.Join(h.dir, llm.SettingsFile))
	if strings.Contains(string(settings), cliKey) {
		t.Fatal("key written to the settings file")
	}
	h.stdin = ""
	h.ok("llm", "set", "--profile", "backup", "--provider", "deepseek", "--model", "deepseek-chat", "--key-env", "DEEPSEEK_KEY")
	h.env["DEEPSEEK_KEY"] = "sk-deepseek-000000000"

	_, errOut, code := h.run("llm", "test")
	if code != ExitError || !strings.Contains(errOut, "") {
		t.Fatalf("failing test exit %d", code)
	}
	out, _, _ = h.run("llm", "test")
	if !strings.Contains(out, "连接失败 [llm_auth_failed]") || !strings.Contains(out, "可切换模型配置：backup") {
		t.Fatalf("failure output:\n%s", out)
	}
	// Retrying succeeds once the service accepts the key.
	if out = h.ok("llm", "test"); !strings.Contains(out, "连接成功") {
		t.Fatalf("retry:\n%s", out)
	}
	// Switching profiles.
	h.ok("llm", "use", "backup")
	var rep llm.ConnectionReport
	h.json(&rep, "llm", "test")
	if !rep.OK || rep.Model.Profile != "backup" || rep.Model.KeySource != "env:DEEPSEEK_KEY" {
		t.Fatalf("backup %+v", rep)
	}
	var st llm.StatusReport
	h.json(&st, "llm", "status")
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "sk-deepseek") || strings.Contains(string(raw), cliKey) || st.Model.Profile != "backup" {
		t.Fatalf("status %s", raw)
	}
	var status StatusReport
	h.json(&status, "status")
	if status.Model.Profile != "backup" || status.Model.Status != "configured" || status.Model.Mode != llm.ModeConfirm {
		t.Fatalf("todo status model %+v", status.Model)
	}
	h.ok("llm", "use", "default")
	h.ok("llm", "remove", "backup")
	if _, code := h.fail("llm", "use", "backup"); code != ExitError {
		t.Fatal("removed profile still usable")
	}
	h.ok("llm", "delete-key")
	if _, err := h.hooks.Secrets.Get(llm.DefaultProfile); err == nil {
		t.Fatal("key still in keychain")
	}
}

func TestLLMPermissionSettings(t *testing.T) {
	h := newHarness(t)
	h.ok("llm", "mode", "自动执行")
	if out := h.ok("llm", "mode"); !strings.Contains(out, "自动执行") {
		t.Fatalf("mode: %s", out)
	}
	h.ok("llm", "confirm", "add", "git", "push")
	h.ok("llm", "confirm", "add", "agent.start")
	h.ok("llm", "confirm", "remove", "agent.start")
	var c struct {
		ConfirmList []string `json:"confirm_list"`
	}
	h.json(&c, "llm", "confirm")
	if !slices.Equal(c.ConfirmList, []string{"git push"}) {
		t.Fatalf("confirm list %v", c.ConfirmList)
	}
	h.ok("llm", "commands", "off")
	h.ok("llm", "agent", "add", "coder", "--desc", "写代码", "--", "claude", "-p")
	h.ok("llm", "fields", "--send", "title,tags")
	var st llm.StatusReport
	h.json(&st, "llm")
	if st.Mode != llm.ModeAuto || st.AllowCommands || !slices.Equal(st.Agents, []string{"coder"}) ||
		!slices.Equal(st.SendFields, []string{"title", "tags"}) {
		t.Fatalf("status %+v", st)
	}
	if errOut, _ := h.fail("llm", "fields", "--send", "secret"); !strings.Contains(errOut, "unknown task field") {
		t.Fatalf("bad field: %s", errOut)
	}
	if errOut, _ := h.fail("llm", "mode", "yolo"); !strings.Contains(errOut, "unknown permission mode") {
		t.Fatalf("bad mode: %s", errOut)
	}
	if out := h.ok("llm", "-h"); !strings.Contains(out, "set-key") {
		t.Fatalf("help:\n%s", out)
	}
}

func TestAIOptimizeAndConfigVersions(t *testing.T) {
	h, _ := modelHarness(t, map[string]any{"suggestions": []map[string]any{{
		"target": "prompts.intake", "problem": "常缺截止时间", "proposal": "提示用户补充截止时间", "expected_impact": "减少追问",
		"verification": "统计追问次数", "rollback": "撤销建议",
		"value": "只提取用户明确说出的信息。\n截止时间缺失时提问。",
	}}})
	out := h.ok("ai", "optimize")
	for _, want := range []string{"[待确认] 优化 prompts.intake", "问题：常缺截止时间", "预期影响：减少追问", "+ 截止时间缺失时提问。", "需确认"} {
		if !strings.Contains(out, want) {
			t.Errorf("optimize output lacks %q:\n%s", want, out)
		}
	}
	list := struct {
		Sessions []llm.SessionSummary `json:"sessions"`
	}{}
	h.json(&list, "ai", "list", "--kind", "optimize")
	id := list.Sessions[0].ID
	h.ok("ai", "apply", id)
	var cfg struct {
		Version int              `json:"version"`
		Config  llm.AssistConfig `json:"config"`
	}
	h.json(&cfg, "ai", "config")
	if cfg.Version != 1 || !strings.Contains(cfg.Config.Prompts.Intake, "截止时间缺失时提问") {
		t.Fatalf("config %+v", cfg)
	}
	h.ok("ai", "undo", id)
	h.json(&cfg, "ai", "config")
	if cfg.Version != 2 || strings.Contains(cfg.Config.Prompts.Intake, "截止时间缺失时提问") {
		t.Fatalf("after undo %+v", cfg)
	}
	h.ok("ai", "config", "restore", "v1")
	if out := h.ok("ai", "config", "rollback"); !strings.Contains(out, "v4") {
		t.Fatalf("rollback: %s", out)
	}
	if out := h.ok("ai", "config", "versions"); !strings.Contains(out, "suggestion") || !strings.Contains(out, "restore") {
		t.Fatalf("versions:\n%s", out)
	}
	h.ok("ai", "config", "set", "ranking", `{"overdue_weight":1,"due_soon_hours":24,"due_soon_weight":1,"priority_weight":1,"in_progress_bonus":1,"blocked_penalty":1,"reopen_weight":1,"max_recommendations":2}`)
	if errOut, _ := h.fail("ai", "config", "set", "ranking", `{"max_recommendations":0}`); !strings.Contains(errOut, "max_recommendations") {
		t.Fatalf("invalid config: %s", errOut)
	}
}

func TestPromptCommands(t *testing.T) {
	h, _ := modelHarness(t, map[string]any{
		"goal": "为 {{产品}} 写发布说明", "context": "面向 {{读者}}", "constraints": []string{"不要夸大"},
		"steps": []string{"列出变更", "写摘要"}, "output_format": "Markdown",
		"variables": []map[string]any{{"name": "读者", "default": "用户"}},
	})
	h.stdin = "请帮我为 FlowOS 写发布说明，面向用户……（很长的提示词）\nAPI key: " + cliKey
	var sum struct {
		Body     string           `json:"body"`
		Degraded bool             `json:"degraded"`
		Template *prompt.Template `json:"template"`
	}
	h.json(&sum, "prompt", "summarize", "--save", "--name", "发布说明")
	h.stdin = ""
	if sum.Degraded || sum.Template == nil || !strings.Contains(sum.Body, "## 执行步骤") || strings.Contains(sum.Template.Original, cliKey) {
		t.Fatalf("summarize %+v", sum)
	}
	if out := h.ok("prompt", "list"); !strings.Contains(out, "发布说明") || !strings.Contains(out, "产品") || !strings.Contains(out, "读者") {
		t.Fatalf("list:\n%s", out)
	}
	if out := h.ok("prompt", "show", "发布说明", "--original"); !strings.Contains(out, "很长的提示词") {
		t.Fatalf("original:\n%s", out)
	}
	h.ok("prompt", "edit", "发布说明", "--step", "列出变更", "--step", "写摘要", "--step", "校对", "--constraint", "不要夸大, 也不要缩水", "--note", "加校对")
	if out := h.ok("prompt", "show", "发布说明", "--raw"); !strings.Contains(out, "3. 校对") || !strings.Contains(out, "- 不要夸大, 也不要缩水") {
		t.Fatalf("edited:\n%s", out)
	}
	if out := h.ok("prompt", "versions", "发布说明"); !strings.Contains(out, "v2") || !strings.Contains(out, "加校对") {
		t.Fatalf("versions:\n%s", out)
	}
	h.ok("prompt", "rollback", "发布说明")
	if out := h.ok("prompt", "show", "发布说明", "--raw"); strings.Contains(out, "校对") {
		t.Fatalf("rollback:\n%s", out)
	}
	if out := h.ok("prompt", "render", "发布说明", "--var", "产品=FlowOS, Pro"); !strings.Contains(out, "为 FlowOS, Pro 写发布说明") || !strings.Contains(out, "面向 用户") {
		t.Fatalf("render:\n%s", out)
	}
	if errOut, _ := h.fail("prompt", "render", "发布说明"); !strings.Contains(errOut, "产品") {
		t.Fatalf("missing var: %s", errOut)
	}
	var copied []string
	orig := clipboardWrite
	clipboardWrite = func(s string) error { copied = append(copied, s); return nil }
	t.Cleanup(func() { clipboardWrite = orig })
	h.ok("prompt", "copy", "发布说明", "--clipboard")
	if len(copied) != 1 || !strings.Contains(copied[0], "{{产品}}") {
		t.Fatalf("clipboard %v", copied)
	}
	h.ok("prompt", "copy", "发布说明", "--name", "发布说明 B")
	file := filepath.Join(t.TempDir(), "p.json")
	h.ok("prompt", "export", "发布说明", "--format", "json", "-o", file)
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), `"format": "todo-cli/prompt"`) || !strings.Contains(string(raw), "很长的提示词") {
		t.Fatalf("export:\n%s", raw)
	}
	if md := h.ok("prompt", "export", "发布说明"); !strings.HasPrefix(md, "# 发布说明") {
		t.Fatalf("md export:\n%s", md)
	}
	var created struct {
		Task   core.Task `json:"task"`
		Prompt string    `json:"prompt"`
	}
	h.json(&created, "prompt", "task", "发布说明", "--var", "产品=FlowOS")
	if !strings.Contains(created.Task.Description, "为 FlowOS 写发布说明") || !slices.Contains(created.Task.Tags, "agent") {
		t.Fatalf("agent task %+v", created.Task)
	}
	h.ok("prompt", "rename", "发布说明 B", "发布说明 C")
	h.ok("prompt", "delete", "发布说明 C")
	var tl struct {
		Templates []prompt.Template `json:"templates"`
	}
	h.json(&tl, "prompt", "list")
	if len(tl.Templates) != 1 {
		t.Fatalf("templates %d", len(tl.Templates))
	}

	// Without a model the summary falls back to local rules.
	delete(h.env, EnvModelAPIKey)
	h.stdin = "写一份周报\n1. 汇总进展\n2. 列出风险\n不要超过 500 字\n输出 Markdown"
	out, errOut, code := h.run("prompt", "summarize")
	if code != 0 || !strings.Contains(errOut, "本地规则") || !strings.Contains(out, "2. 列出风险") || !strings.Contains(out, "- 不要超过 500 字") {
		t.Fatalf("degraded summarize %d:\n%s\n%s", code, out, errOut)
	}
}

// refByTitle finds the short task reference the model was given.
func refByTitle(t *testing.T, req llm.Request, title string) string {
	t.Helper()
	var user struct {
		Tasks []struct{ Ref, Title string } `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &user); err != nil {
		t.Fatal(err)
	}
	for _, tk := range user.Tasks {
		if tk.Title == title {
			return tk.Ref
		}
	}
	t.Fatalf("task %q not sent", title)
	return ""
}
