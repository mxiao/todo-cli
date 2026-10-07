package llm

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

type decideTasks struct{ a, b, c, d *core.Task }

// decideSetup: A overdue/low, B high, C waits for A, D plain.
func decideSetup(f *fixture) decideTasks {
	overdue := time.Date(2026, 10, 5, 18, 0, 0, 0, cst)
	var ts decideTasks
	ts.a = f.task("修复线上 bug", func(n *core.NewTask) { n.DueAt, n.Priority = &overdue, core.PriorityLow })
	ts.b = f.task("写方案", func(n *core.NewTask) { n.Priority = core.PriorityHigh })
	ts.c = f.task("上线", func(n *core.NewTask) { n.DependsOn = []string{ts.a.ID} })
	ts.d = f.task("整理文档")
	return ts
}

func decideAnswer(t *testing.T) func(Request) any {
	return func(req Request) any {
		a, b, d := refOf(t, req, "修复线上 bug"), refOf(t, req, "写方案"), refOf(t, req, "整理文档")
		return map[string]any{
			"summary": "1 个任务已逾期，阻塞上线",
			"risks":   []map[string]any{{"task": a, "level": "high", "message": "已逾期且阻塞上线"}},
			"recommendations": []map[string]any{
				{"task": a, "reason": "已逾期并阻塞上线", "focus": "先回滚再修", "estimate": "1h"},
				{"task": b, "reason": "高优先级", "estimate": "2h"},
			},
			"changes": []map[string]any{
				{"task": a, "field": "priority", "to": "urgent", "reason": "逾期且被依赖"},
				{"task": b, "field": "due", "to": "2026-10-08 18:00", "reason": "给方案定截止"},
				{"task": d, "field": "status", "to": "in_progress", "reason": "顺手开始"},
				{"task": a, "field": "order", "to": 1, "reason": "最先做"},
				{"task": b, "field": "order", "to": 2, "reason": "其次"},
				{"task": d, "field": "delete", "reason": "已过时"},
				{"task": b, "field": "priority", "to": "high", "reason": "不变"},
				{"task": "T99", "field": "priority", "to": "low"},
			},
		}
	}
}

func TestDecideShowsDiffAndAcceptsItems(t *testing.T) {
	f := newFixture(t)
	ts := decideSetup(f)
	f.model.Push(decideAnswer(t))
	sess, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Degraded || sess.Summary == "" || len(sess.Risks) != 1 || sess.Risks[0].Title != ts.a.Title {
		t.Fatalf("decision %+v", sess)
	}
	if r := sess.Recommendations; len(r) != 2 || r[0].Order != 1 || r[0].TaskID != ts.a.ID || r[0].Reason == "" ||
		r[0].Focus == "" || r[0].Estimate != "1h" {
		t.Fatalf("recommendations %+v", r)
	}
	// Decision support sees deadlines and dependencies besides the defaults.
	if prompt := f.lastPrompt(); !strings.Contains(prompt, "due_at") || !strings.Contains(prompt, "depends_on") ||
		!strings.Contains(prompt, `"blocked": true`) {
		t.Fatalf("decision context:\n%s", prompt)
	}
	// The no-op change is dropped, the unknown task is reported.
	if len(sess.Items) != 6 || !strings.Contains(strings.Join(sess.Warnings, ""), "T99") {
		t.Fatalf("items %d warnings %v", len(sess.Items), sess.Warnings)
	}
	d := sess.Items[0].Diff[0]
	if d.Field != "priority" || d.Before != "low" || d.After != "urgent" {
		t.Fatalf("priority diff %+v", d)
	}
	d = sess.Items[1].Diff[0]
	if d.Field != "due_at" || d.Before != nil || d.After != "2026-10-08 18:00 Thu" {
		t.Fatalf("due diff %+v", d)
	}
	for _, it := range sess.Items {
		if it.Status != ItemPending || !it.NeedsConfirm {
			t.Fatalf("confirm mode applied %+v", it)
		}
	}
	if f.get(ts.a.ID).Priority != core.PriorityLow {
		t.Fatal("changed before confirmation")
	}

	// Accept a single item.
	sess, err = f.svc.Apply(ctx, sess.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusPartial || f.get(ts.a.ID).Priority != core.PriorityUrgent {
		t.Fatalf("accept one: %s", sess.Status)
	}
	if h := lastHistory(t, f.store, ts.a.ID); h.Actor != Actor || h.Changes["priority"].To != "urgent" {
		t.Fatalf("history %+v", h)
	}
	// Ignore the deletion, accept everything else.
	if _, err := f.svc.Reject(sess.ID, []int{6}); err != nil {
		t.Fatal(err)
	}
	sess, err = f.svc.Apply(ctx, sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusApplied || sess.Result.Updated != 5 || sess.Result.NotExecuted != 1 {
		t.Fatalf("accept all: %s %+v", sess.Status, sess.Result)
	}
	want := time.Date(2026, 10, 8, 18, 0, 0, 0, cst)
	if b := f.get(ts.b.ID); b.DueAt == nil || !b.DueAt.Equal(want) {
		t.Fatalf("due %v", b.DueAt)
	}
	if dd := f.get(ts.d.ID); dd.Status != core.StatusInProgress || dd.Deleted() {
		t.Fatalf("d %+v", dd)
	}
	if got := titles(f.open())[:2]; !slices.Equal(got, []string{"修复线上 bug", "写方案"}) {
		t.Fatalf("order %v", got)
	}

	// Undoing one of the accept-all items reverts that whole accept step.
	sess, err = f.svc.Undo(sess.ID, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	if f.get(ts.b.ID).DueAt != nil || f.get(ts.d.ID).Status != core.StatusTodo || f.get(ts.a.ID).Priority != core.PriorityUrgent {
		t.Fatal("undo did not revert the accept-all step only")
	}
	if sess.Status != StatusApplied || sess.Items[0].Status != ItemApplied || sess.Items[1].Status != ItemUndone {
		t.Fatalf("after undo %s %+v", sess.Status, sess.Items)
	}
}

func TestRedecideReplacesTheDecision(t *testing.T) {
	f := newFixture(t)
	decideSetup(f)
	f.model.Push(decideAnswer(t), map[string]any{"summary": "保守方案", "recommendations": []map[string]any{}})
	first, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.svc.Redecide(ctx, first.ID, "太激进了，不要删除任务")
	if err != nil {
		t.Fatal(err)
	}
	prompt := f.lastPrompt()
	if !strings.Contains(prompt, "太激进了") || !strings.Contains(prompt, "previous_decision") {
		t.Fatalf("redecide prompt:\n%s", prompt)
	}
	if next.Supersedes != first.ID || next.Summary != "保守方案" {
		t.Fatalf("next %+v", next)
	}
	old, err := f.svc.Session(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusSuperseded || old.SupersededBy != next.ID || old.Items[0].Status != ItemRejected {
		t.Fatalf("old %+v", old)
	}
	if _, err := f.svc.Apply(ctx, first.ID, nil); !errors.Is(err, ErrState) {
		t.Fatalf("apply superseded: %v", err)
	}
}

func TestDecideAutoModeAppliesButConfirmsDeletes(t *testing.T) {
	f := newFixture(t)
	ts := decideSetup(f)
	f.setMode(ModeAuto)
	f.model.Push(func(req Request) any {
		a, b, d := refOf(t, req, "修复线上 bug"), refOf(t, req, "写方案"), refOf(t, req, "整理文档")
		return map[string]any{"summary": "s", "changes": []map[string]any{
			{"task": a, "field": "priority", "to": "high"},
			{"task": b, "field": "depends_on", "to": []string{a}},
			{"task": b, "field": "due_at", "to": "2026-10-09"},
			{"task": d, "field": "delete"},
		}}
	})
	sess, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range sess.Items[:3] {
		if it.Status != ItemApplied || it.AppliedBy != "auto" {
			t.Fatalf("auto item %+v", it)
		}
	}
	del := sess.Items[3]
	if del.Status != ItemPending || !del.NeedsConfirm || !slices.Contains(del.Categories, CatDelete) {
		t.Fatalf("delete item %+v", del)
	}
	b := f.get(ts.b.ID)
	if !slices.Equal(b.DependsOn, []string{ts.a.ID}) || b.DueAt == nil {
		t.Fatalf("b %+v", b)
	}
	if h := lastHistory(t, f.store, ts.b.ID); h.Actor != Actor {
		t.Fatalf("history %+v", h)
	}
	if f.get(ts.d.ID).Deleted() {
		t.Fatal("deleted without confirmation")
	}
	if sess.Status != StatusPartial || sess.Result.Updated != 3 || sess.Result.NotExecuted != 1 {
		t.Fatalf("session %s %+v", sess.Status, sess.Result)
	}
}

func TestDecideDegradesToLocalRanking(t *testing.T) {
	f := newFixture(t)
	ts := decideSetup(f)
	delete(f.env, EnvAPIKey)
	sess, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Degraded || !strings.Contains(sess.DegradedReason, "未配置") || len(f.model.Requests) != 0 {
		t.Fatalf("not degraded: %+v", sess)
	}
	var order []string
	for _, r := range sess.Recommendations {
		order = append(order, r.TaskID)
		if r.Reason == "" {
			t.Fatalf("recommendation without reason %+v", r)
		}
	}
	// Overdue first, then high priority; the blocked task is held back.
	if len(order) != 3 || order[0] != ts.a.ID || order[1] != ts.b.ID || slices.Contains(order, ts.c.ID) {
		t.Fatalf("order %v", order)
	}
	if len(sess.Risks) == 0 || sess.Risks[0].TaskID != ts.a.ID || sess.Risks[0].Level != "high" {
		t.Fatalf("risks %+v", sess.Risks)
	}

	// A failing model also degrades, and the failed call is audited.
	f.env[EnvAPIKey] = testKey
	f.model.Push(&Error{Kind: KindServer, Message: "模型服务暂时不可用（HTTP 503）", Retryable: true})
	sess, err = f.svc.Decide(ctx, DecideInput{})
	if err != nil || !sess.Degraded {
		t.Fatalf("server error: %+v %v", sess, err)
	}
	calls, err := f.svc.Calls(5)
	if err != nil || len(calls) != 1 || calls[0].Status != KindServer || calls[0].Purpose != "decide" {
		t.Fatalf("calls %+v %v", calls, err)
	}
}

func TestDecideCommandsAndAgents(t *testing.T) {
	f := newFixture(t)
	ts := decideSetup(f)
	if _, err := f.svc.UpdateSettings(func(s *Settings) error {
		s.Mode = ModeAuto
		s.ConfirmList = []string{"git status"}
		s.Agents = []AgentProfile{{Name: "coder", Command: []string{"claude", "-p"}, Description: "写代码"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.runner.out = CommandResult{ExitCode: 0, Stdout: "done token=abcdefgh12345 " + testKey}
	f.model.Push(func(req Request) any {
		a := refOf(t, req, "修复线上 bug")
		return map[string]any{"summary": "s", "actions": []map[string]any{
			{"type": "command", "argv": []string{"echo", "hi"}, "reason": "检查"},
			{"type": "command", "argv": []string{"rm", "-rf", "/tmp/x"}},
			{"type": "command", "argv": []string{"sh", "-c", "sudo ls"}},
			{"type": "agent", "agent": "coder", "task": a, "reason": "修复 bug"},
			{"type": "agent", "agent": "missing", "task": a},
			{"type": "command", "argv": []string{"git", "status"}},
		}}
	})
	sess, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastPrompt(), "coder") {
		t.Fatal("agents not offered to the model")
	}
	it := sess.Items
	if len(it) != 6 {
		t.Fatalf("items %+v", it)
	}
	if it[0].Status != ItemApplied || it[0].Command.Result == nil || strings.Contains(it[0].Command.Result.Stdout, testKey) ||
		strings.Contains(it[0].Command.Result.Stdout, "abcdefgh12345") {
		t.Fatalf("echo %+v %+v", it[0], it[0].Command.Result)
	}
	if it[1].Status != ItemPending || !slices.Contains(it[1].Categories, CatDelete) {
		t.Fatalf("rm %+v", it[1])
	}
	if it[2].Status != ItemSkipped || !strings.Contains(it[2].Message, "privileges") {
		t.Fatalf("sudo %+v", it[2])
	}
	if it[3].Status != ItemApplied || !strings.Contains(it[3].Command.Prompt, ts.a.Title) {
		t.Fatalf("agent %+v", it[3])
	}
	if it[4].Status != ItemSkipped || !strings.Contains(it[4].Message, "未配置") {
		t.Fatalf("missing agent %+v", it[4])
	}
	if it[5].Status != ItemPending || !strings.Contains(it[5].ConfirmReason, "自定义确认清单") {
		t.Fatalf("confirm list %+v", it[5])
	}
	if len(f.runner.specs) != 2 {
		t.Fatalf("runs %+v", f.runner.specs)
	}
	agent := f.runner.specs[1]
	if !slices.Equal(agent.Argv, []string{"claude", "-p"}) || !strings.Contains(agent.Stdin, ts.a.Title) ||
		!slices.Contains(agent.HideEnv, EnvAPIKey) {
		t.Fatalf("agent spec %+v", agent)
	}
	actions, err := f.svc.Actions(10)
	if err != nil || len(actions) != 2 || actions[1].Status != "succeeded" || actions[1].Actor != "llm/auto" ||
		strings.Contains(actions[1].Result.Stdout, testKey) {
		t.Fatalf("actions %+v %v", actions, err)
	}

	// The user confirms the deletion; a failing command counts as failed.
	f.runner.out = CommandResult{ExitCode: 1, Stderr: "rm: permission denied"}
	sess, err = f.svc.Apply(ctx, sess.ID, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Items[1].Status != ItemFailed || !strings.Contains(sess.Items[1].Message, "permission denied") || sess.Result.Failed != 1 {
		t.Fatalf("rm after confirm %+v", sess.Items[1])
	}
	sess, err = f.svc.Undo(sess.ID, []int{1})
	if err != nil || !strings.Contains(strings.Join(sess.Warnings, ""), "无法自动撤销") {
		t.Fatalf("undo command: %v %v", sess.Warnings, err)
	}
}

func TestDecideIgnoresCommandsWhenDisabled(t *testing.T) {
	f := newFixture(t)
	decideSetup(f)
	if _, err := f.svc.UpdateSettings(func(s *Settings) error { s.AllowCommands = false; return nil }); err != nil {
		t.Fatal(err)
	}
	f.model.Push(map[string]any{"summary": "s", "actions": []map[string]any{{"type": "command", "argv": []string{"echo"}}}})
	sess, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Items) != 0 || !strings.Contains(f.lastPrompt(), "Do not propose actions") {
		t.Fatalf("items %+v", sess.Items)
	}
}
