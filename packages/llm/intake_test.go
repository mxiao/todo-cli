package llm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

var ctx = context.Background()

func TestIntakeSingleTaskWaitsForConfirmation(t *testing.T) {
	f := newFixture(t, map[string]any{
		"items": []map[string]any{{"action": "create", "title": "写周报", "description": "汇总本周进展",
			"due": "2026-10-07 18:00", "due_text": "明天下午6点", "priority": "high", "tags": []string{"work"}, "category": "工作"}},
		"summary": "新增 1 条任务",
	})
	f.task("周会准备", func(n *core.NewTask) {
		n.Notes, n.Category, n.Description = "私密备注", "内部", "登录 password=hunter22"
	})

	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "明天下午6点前写周报，高优先级，标签 work。key 是 " + testKey})
	if err != nil {
		t.Fatal(err)
	}
	// Default mode is 执行前确认: nothing is written yet.
	if sess.Mode != ModeConfirm || sess.Status != StatusPending || len(f.open()) != 1 {
		t.Fatalf("session %+v, tasks %v", sess, titles(f.open()))
	}
	it := sess.Items[0]
	if it.Kind != ItemCreate || it.Status != ItemPending || !it.NeedsConfirm || it.ConfirmReason == "" {
		t.Fatalf("item %+v", it)
	}
	var fields []string
	for _, d := range it.Diff {
		fields = append(fields, d.Field)
	}
	for _, want := range []string{"title", "description", "due_at", "priority", "tags", "category"} {
		if !slices.Contains(fields, want) {
			t.Errorf("diff lacks %s: %v", want, fields)
		}
	}
	if r := sess.Result; r.Created != 0 || r.NotExecuted != 1 {
		t.Fatalf("result %+v", r)
	}

	// Only title, description, tags, status and priority leave the machine;
	// secrets are hidden from the model and from the stored session.
	prompt := f.lastPrompt()
	for _, leak := range []string{"私密备注", "内部", "hunter22", testKey} {
		if strings.Contains(prompt, leak) {
			t.Errorf("model prompt contains %q", leak)
		}
	}
	if !strings.Contains(prompt, "周会准备") || !strings.Contains(prompt, Redacted) {
		t.Errorf("prompt misses task context or redaction:\n%s", prompt)
	}
	if strings.Contains(sess.Input, testKey) {
		t.Errorf("session input keeps the key: %s", sess.Input)
	}

	sess, err = f.svc.Apply(ctx, sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusApplied || sess.Result.Created != 1 || sess.Items[0].AppliedBy != "user" {
		t.Fatalf("after apply: %+v %+v", sess, sess.Result)
	}
	task := f.get(sess.Items[0].ResultTaskID)
	want := time.Date(2026, 10, 7, 18, 0, 0, 0, cst)
	if task.Title != "写周报" || task.Priority != core.PriorityHigh || task.Category != "工作" ||
		!slices.Equal(task.Tags, []string{"work"}) || task.DueAt == nil || !task.DueAt.Equal(want) {
		t.Fatalf("task %+v", task)
	}
	if h := lastHistory(t, f.store, task.ID); h.Actor != Actor || h.Action != "create" {
		t.Fatalf("history %+v", h)
	}
	op, err := f.store.OperationByID(sess.Items[0].Operation)
	if err != nil || !strings.Contains(op.Summary, sess.ID) || op.Actor != Actor {
		t.Fatalf("operation %+v %v", op, err)
	}
	if _, err := f.svc.Apply(ctx, sess.ID, nil); !errors.Is(err, ErrState) {
		t.Fatalf("applying twice: %v", err)
	}
}

func TestIntakeMultipleTasksWithDependenciesAutoAndUndo(t *testing.T) {
	f := newFixture(t, map[string]any{"items": []map[string]any{
		{"action": "create", "ref": "N1", "title": "整理需求", "due": "2026-10-07 15:00", "due_text": "明天下午"},
		{"action": "create", "ref": "N2", "title": "更新页面", "depends_on": []string{"N1"}, "due": "2026-10-07 15:00", "due_text": "明天下午"},
		{"action": "create", "ref": "N3", "title": "检查链接", "depends_on": []string{"N2"}, "tags": []string{"agent"}},
	}})
	f.setMode(ModeAuto)
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "明天下午前完成产品发布准备，先整理需求，再更新页面，最后让命令行智能体检查链接"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusApplied || sess.Result.Created != 3 || len(sess.Operations) != 1 {
		t.Fatalf("session %+v result %+v", sess, sess.Result)
	}
	ids := map[string]string{}
	for _, it := range sess.Items {
		if it.Status != ItemApplied || it.AppliedBy != "auto" {
			t.Fatalf("item %+v", it)
		}
		ids[*it.Fields.Title] = it.ResultTaskID
	}
	if d := f.get(ids["更新页面"]).DependsOn; !slices.Equal(d, []string{ids["整理需求"]}) {
		t.Fatalf("deps %v", d)
	}
	if d := f.get(ids["检查链接"]).DependsOn; !slices.Equal(d, []string{ids["更新页面"]}) {
		t.Fatalf("deps %v", d)
	}

	// Undo removes everything the session created, as one operation.
	sess, err = f.svc.Undo(sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusUndone || len(f.open()) != 0 {
		t.Fatalf("after undo: %s %v", sess.Status, titles(f.open()))
	}
	for _, it := range sess.Items {
		if it.Status != ItemUndone {
			t.Fatalf("item not undone: %+v", it)
		}
	}
	if _, err := f.svc.Undo(sess.ID, nil); !errors.Is(err, ErrState) {
		t.Fatalf("second undo: %v", err)
	}
}

func TestIntakeUsesSelectionToUpdateOrAddSubtasks(t *testing.T) {
	f := newFixture(t)
	report := f.task("写周报")
	f.task("别的任务")
	f.model.Push(func(req Request) any {
		ref := refOf(t, req, "写周报")
		return map[string]any{"items": []map[string]any{
			{"action": "update", "task": ref, "priority": "urgent", "reason": "用户说很急"},
			{"action": "subtask", "parent": ref, "title": "收集数据"},
			{"action": "create", "title": "订会议室"},
		}}
	})
	f.setMode(ModeAuto)
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "这个很急，先收集数据，另外订会议室", Selected: []string{report.ID}})
	if err != nil {
		t.Fatal(err)
	}
	user := f.model.LastRequest().Messages[1].Content
	if !strings.Contains(user, `"selected"`) {
		t.Fatalf("selection not sent: %s", user)
	}
	if sess.Result.Created != 2 || sess.Result.Updated != 1 || sess.Status != StatusApplied {
		t.Fatalf("result %+v items %+v", sess.Result, sess.Items)
	}
	if got := f.get(report.ID); got.Priority != core.PriorityUrgent {
		t.Fatalf("priority %v", got.Priority)
	}
	sub := f.get(sess.Items[1].ResultTaskID)
	if sub.ParentID != report.ID || sub.Title != "收集数据" {
		t.Fatalf("subtask %+v", sub)
	}
	if d := sess.Items[0].Diff; len(d) != 1 || d[0].Field != "priority" || d[0].Before != "none" || d[0].After != "urgent" {
		t.Fatalf("update diff %+v", d)
	}

	// Rewriting an existing title is an overwrite: confirmed even in auto mode.
	f.model.Push(func(req Request) any {
		return map[string]any{"items": []map[string]any{{"action": "update", "task": refOf(t, req, "写周报"), "title": "写月报"}}}
	})
	sess, err = f.svc.Intake(ctx, IntakeInput{Text: "改成写月报", Selected: []string{report.ID}})
	if err != nil {
		t.Fatal(err)
	}
	it := sess.Items[0]
	if it.Status != ItemPending || !it.NeedsConfirm || !slices.Contains(it.Categories, CatOverwrite) {
		t.Fatalf("overwrite item %+v", it)
	}
	if f.get(report.ID).Title != "写周报" {
		t.Fatal("title overwritten without confirmation")
	}
}

func TestIntakeAsksOnlyOnce(t *testing.T) {
	f := newFixture(t, map[string]any{
		"questions": []string{"报告的截止时间是什么时候？"},
		"items":     []map[string]any{{"action": "create", "title": "交报告"}, {"action": "create", "description": "没有标题"}},
	})
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "交报告，还有那个事"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != StatusNeedsInput || !sess.Asked || len(sess.Questions) != 2 || len(f.open()) != 0 {
		t.Fatalf("session %+v", sess)
	}
	if !strings.Contains(sess.Questions[1], "缺少标题") {
		t.Fatalf("missing title not asked: %v", sess.Questions)
	}
	if _, err := f.svc.Apply(ctx, sess.ID, nil); !errors.Is(err, ErrState) {
		t.Fatalf("apply before answering: %v", err)
	}

	// The model asks again after the answer: ignored, it continues with what
	// it has and never invents a deadline nobody stated.
	f.model.Push(map[string]any{
		"questions": []string{"还要别的吗？"},
		"items": []map[string]any{
			{"action": "create", "title": "交报告", "due": "2026-10-09 18:00", "due_text": "周五 18:00"},
			{"action": "create", "title": "回复邮件", "due": "2026-10-08 12:00", "due_text": "后天中午"},
		},
	})
	sess, err = f.svc.Answer(ctx, sess.ID, "周五 18:00 前；另一个是回复邮件")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastPrompt(), "Do not ask again") {
		t.Fatal("final round does not forbid further questions")
	}
	if sess.Status != StatusPending || len(sess.Items) != 2 {
		t.Fatalf("after answer: %+v", sess)
	}
	joined := strings.Join(sess.Warnings, "\n")
	if !strings.Contains(joined, "仅追问一次") || !strings.Contains(joined, "不编造截止时间") {
		t.Fatalf("warnings %v", sess.Warnings)
	}
	if sess.Items[0].Fields.Due == nil || sess.Items[1].Fields.Due != nil {
		t.Fatalf("due handling: %+v / %+v", sess.Items[0].Fields, sess.Items[1].Fields)
	}
	if _, err := f.svc.Answer(ctx, sess.ID, "again"); !errors.Is(err, ErrState) {
		t.Fatalf("second answer: %v", err)
	}
}

func TestIntakeEditItemsBeforeSaving(t *testing.T) {
	f := newFixture(t, map[string]any{"items": []map[string]any{
		{"action": "create", "title": "写周报"},
		{"action": "create", "title": "发邮件"},
		{"action": "create", "title": "订机票"},
	}})
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "写周报、发邮件、订机票"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err = f.svc.EditItem(sess.ID, 1, ItemEdit{Title: p("写周报（终稿）"), Due: p("明天 18:00"), Priority: p("高"), Tags: &[]string{"Work", "weekly"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := sess.Items[0].Fields; *d.Title != "写周报（终稿）" || *d.Priority != "high" || d.Due == nil {
		t.Fatalf("edited fields %+v", d)
	}
	if _, err := f.svc.EditItem(sess.ID, 2, ItemEdit{DependsOn: &[]string{"N1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.EditItem(sess.ID, 2, ItemEdit{Title: p("  ")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title accepted: %v", err)
	}
	if _, err := f.svc.EditItem(sess.ID, 2, ItemEdit{DependsOn: &[]string{"N9"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown ref accepted: %v", err)
	}
	if _, err := f.svc.Reject(sess.ID, []int{3}); err != nil {
		t.Fatal(err)
	}
	sess, err = f.svc.Apply(ctx, sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := sess.Result; r.Created != 2 || r.NotExecuted != 1 || sess.Status != StatusApplied {
		t.Fatalf("result %+v status %s", r, sess.Status)
	}
	first, second := f.get(sess.Items[0].ResultTaskID), f.get(sess.Items[1].ResultTaskID)
	want := time.Date(2026, 10, 7, 18, 0, 0, 0, cst)
	if first.Title != "写周报（终稿）" || first.Priority != core.PriorityHigh || !first.DueAt.Equal(want) ||
		!slices.Equal(first.Tags, []string{"Work", "weekly"}) {
		t.Fatalf("first %+v", first)
	}
	if !slices.Equal(second.DependsOn, []string{first.ID}) {
		t.Fatalf("second deps %v", second.DependsOn)
	}
	if _, err := f.svc.EditItem(sess.ID, 1, ItemEdit{Title: p("x")}); !errors.Is(err, ErrState) {
		t.Fatalf("editing an applied item: %v", err)
	}
}

func TestIntakeReportsCreatedUpdatedSkippedAndFailed(t *testing.T) {
	f := newFixture(t)
	gone := f.task("将被删除")
	f.model.Push(func(req Request) any {
		return map[string]any{"items": []map[string]any{
			{"action": "create", "title": "新任务"},
			{"action": "update", "task": refOf(t, req, "将被删除"), "priority": "high"},
			{"action": "update", "task": "T99", "priority": "low"},
		}}
	})
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "加个新任务，把那个设为高优先级"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Items[2].Status != ItemSkipped {
		t.Fatalf("unknown task ref not skipped: %+v", sess.Items[2])
	}
	if _, err := f.store.Delete(gone.ID); err != nil {
		t.Fatal(err)
	}
	sess, err = f.svc.Apply(ctx, sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := sess.Result
	if r.Created != 1 || r.Updated != 0 || r.Failed != 1 || r.NotExecuted != 1 || len(r.Messages) < 2 {
		t.Fatalf("result %+v", r)
	}
	if sess.Items[1].Status != ItemFailed || !strings.Contains(sess.Items[1].Message, "删除") {
		t.Fatalf("failed item %+v", sess.Items[1])
	}
	// The failed step was rolled back alone; the new task exists.
	if got := titles(f.open()); !slices.Equal(got, []string{"新任务"}) {
		t.Fatalf("tasks %v", got)
	}
}

func TestIntakeSuggestModeAndReject(t *testing.T) {
	f := newFixture(t,
		map[string]any{"items": []map[string]any{{"action": "create", "title": "A"}}},
		map[string]any{"items": []map[string]any{{"action": "create", "title": "B"}}})
	f.setMode(ModeAuto)
	sess, err := f.svc.Intake(ctx, IntakeInput{Text: "A", Mode: ModeSuggest})
	if err != nil {
		t.Fatal(err)
	}
	if it := sess.Items[0]; it.Status != ItemPending || !strings.Contains(it.ConfirmReason, "仅建议") || len(f.open()) != 0 {
		t.Fatalf("suggest mode applied: %+v", it)
	}
	if sess, err = f.svc.Apply(ctx, sess.ID, []int{1}); err != nil || sess.Result.Created != 1 {
		t.Fatalf("accept suggestion: %+v %v", sess, err)
	}

	sess, err = f.svc.Intake(ctx, IntakeInput{Text: "B", Mode: ModeConfirm})
	if err != nil {
		t.Fatal(err)
	}
	sess, err = f.svc.Reject(sess.ID, nil)
	if err != nil || sess.Status != StatusRejected || sess.Result.NotExecuted != 1 {
		t.Fatalf("reject: %+v %v", sess, err)
	}
	if got := titles(f.open()); !slices.Equal(got, []string{"A"}) {
		t.Fatalf("tasks %v", got)
	}
	list, err := f.svc.Sessions(KindIntake, 10)
	if err != nil || len(list) != 2 || list[0].Status != StatusRejected {
		t.Fatalf("sessions %+v %v", list, err)
	}
}

func TestIntakeWithoutModelLeavesTasksUsable(t *testing.T) {
	f := newFixture(t)
	delete(f.env, EnvAPIKey)
	_, err := f.svc.Intake(ctx, IntakeInput{Text: "写周报"})
	e, ok := AsError(err)
	if !errors.Is(err, ErrUnavailable) || !ok || e.Kind != KindNotConfigured || !strings.Contains(e.Error(), EnvAPIKey) {
		t.Fatalf("err %v", err)
	}
	if len(f.model.Requests) != 0 {
		t.Fatal("model called without configuration")
	}
	// FR-606: plain task management keeps working.
	f.task("手动任务")
	if len(f.open()) != 1 {
		t.Fatal("task management broken")
	}
	st := f.svc.Status()
	if st.Available || st.Model.Status != "not_configured" || st.Model.Problem == "" {
		t.Fatalf("status %+v", st)
	}
}
