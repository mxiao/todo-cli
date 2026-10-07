package llm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOptimizeSuggestionsApplyRejectUndoAndRestore(t *testing.T) {
	f := newFixture(t)
	decideSetup(f)
	f.setMode(ModeAuto) // suggestions still wait for the user
	// Some history to learn from: a rejected decision.
	f.model.Push(decideAnswer(t))
	dec, err := f.svc.Decide(ctx, DecideInput{Mode: ModeConfirm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reject(dec.ID, nil); err != nil {
		t.Fatal(err)
	}

	ranking := DefaultAssistConfig().Ranking
	ranking.MaxRecommendations = 3
	ranking.OverdueWeight = 150
	f.model.Push(map[string]any{"suggestions": []map[string]any{
		{"target": "ranking", "problem": "建议过多被拒绝", "proposal": "减少建议数量", "expected_impact": "更聚焦",
			"verification": "观察接受率", "rollback": "恢复 v0", "value": ranking},
		{"target": "prompts.decide", "problem": "删除建议被拒", "proposal": "禁止建议删除", "value": DefaultAssistConfig().Prompts.Decide + "\n不要建议删除任务。"},
		{"target": "permissions", "value": map[string]any{"mode": "auto"}},
		{"target": "routing", "value": []map[string]any{{"match": "tag:x"}}},
	}})
	sess, err := f.svc.Optimize(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.lastPrompt(), "rejected_examples") {
		t.Fatalf("statistics not sent:\n%s", f.lastPrompt())
	}
	if len(sess.Items) != 2 || len(sess.Warnings) != 2 {
		t.Fatalf("items %+v warnings %v", sess.Items, sess.Warnings)
	}
	for _, it := range sess.Items {
		if it.Status != ItemPending || !it.NeedsConfirm {
			t.Fatalf("suggestion applied without review: %+v", it)
		}
	}
	diff := map[string]Diff{}
	for _, d := range sess.Items[0].Diff {
		diff[d.Field] = d
	}
	if d := diff["ranking.max_recommendations"]; d.Before != float64(5) || d.After != float64(3) || len(diff) != 2 {
		t.Fatalf("ranking diff %+v", sess.Items[0].Diff)
	}
	lines := sess.Items[1].Diff[1].After.([]string)
	if lines[len(lines)-1] != "+ 不要建议删除任务。" {
		t.Fatalf("prompt line diff %v", lines)
	}

	sess, err = f.svc.Apply(ctx, sess.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	cfg, v, _ := f.svc.AssistConfig()
	if v != 1 || cfg.Ranking.MaxRecommendations != 3 || sess.Items[0].ConfigVersion != 1 {
		t.Fatalf("applied config v%d %+v", v, cfg.Ranking)
	}
	if sess, err = f.svc.Reject(sess.ID, []int{2}); err != nil || sess.Items[1].Status != ItemRejected {
		t.Fatalf("reject: %v", err)
	}
	if cfg, _, _ := f.svc.AssistConfig(); strings.Contains(cfg.Prompts.Decide, "不要建议删除") {
		t.Fatal("rejected suggestion applied")
	}

	// The new rule is used by later decisions.
	delete(f.env, EnvAPIKey)
	local, err := f.svc.Decide(ctx, DecideInput{})
	if err != nil || len(local.Recommendations) > 3 || local.ConfigVersion != 1 {
		t.Fatalf("local decision with v1: %+v %v", local.Recommendations, err)
	}

	sess, err = f.svc.Undo(sess.ID, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	cfg, v, _ = f.svc.AssistConfig()
	if v != 2 || cfg.Ranking.MaxRecommendations != 5 || sess.Items[0].Status != ItemUndone {
		t.Fatalf("after undo v%d %+v", v, cfg.Ranking)
	}
	versions, err := f.svc.AssistVersions()
	if err != nil || len(versions) != 3 || versions[1].Source != "suggestion" || versions[2].Source != "undo" {
		t.Fatalf("versions %+v %v", versions, err)
	}

	// Any version can be restored, and 恢复上一版本 goes back one step.
	if cv, err := f.svc.RestoreAssistVersion(1); err != nil || cv.Version != 3 || cv.Config.Ranking.MaxRecommendations != 3 {
		t.Fatalf("restore: %+v %v", cv, err)
	}
	if cv, err := f.svc.RollbackAssist(); err != nil || cv.Version != 4 || cv.Config.Ranking.MaxRecommendations != 5 {
		t.Fatalf("rollback: %+v %v", cv, err)
	}
}

func TestOptimizeUndoRefusesWhenTargetChangedSince(t *testing.T) {
	f := newFixture(t)
	f.model.Push(map[string]any{"suggestions": []map[string]any{
		{"target": "failure", "problem": "命令超时", "value": map[string]any{"max_retries": 2, "command_timeout_seconds": 300, "on_repeated_failure": "ask"}},
	}})
	sess, err := f.svc.Optimize(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Apply(ctx, sess.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAssistValue("failure", json.RawMessage(`{"max_retries":0,"command_timeout_seconds":60,"on_repeated_failure":"stop"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Undo(sess.ID, nil); !errors.Is(err, ErrState) {
		t.Fatalf("undo over a later change: %v", err)
	}
	if _, err := f.svc.SetAssistValue("failure", json.RawMessage(`{"max_retries":99}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid value accepted: %v", err)
	}
	if _, err := f.svc.SetAssistValue("permissions", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown target accepted: %v", err)
	}
}

func TestLineDiff(t *testing.T) {
	got := LineDiff("a\nb\nc", "a\nc\nd")
	want := []string{"  a", "- b", "  c", "+ d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}
