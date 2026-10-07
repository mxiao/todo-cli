package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
	"github.com/mxiao/todo-cli/packages/llm"
)

const longPrompt = "你是资深数据分析师。请分析 {{产品}} 上周的销售数据。\n" +
	"数据文件在 /data/sales_2026.csv，字段包含 `region`、`amount`。\n" +
	"1. 读取数据并清洗异常值\n2. 按地区汇总\n3. 找出下降最多的地区\n" +
	"不要编造数据，必须注明数据来源。\n" +
	"输出 Markdown 表格，并附 3 条结论。"

func modelSummary() map[string]any {
	return map[string]any{
		"goal":          "分析 {{产品}} 在 {{周期}} 的销售数据",
		"context":       "数据文件：{{数据文件}}，字段 `region`、`amount`",
		"constraints":   []string{"不要编造数据", "必须注明数据来源", " "},
		"steps":         []string{"读取数据并清洗异常值", "按地区汇总", "找出下降最多的地区"},
		"output_format": "Markdown 表格 + 3 条结论",
		"variables": []map[string]any{
			{"name": "产品", "description": "产品名"},
			{"name": "周期", "default": "上周"},
			{"name": "{{数据文件}}", "description": "CSV 路径"},
		},
	}
}

func newLLM(t *testing.T, store *core.Store, env map[string]string, responses ...any) (*llm.Service, *llm.Scripted) {
	t.Helper()
	model := llm.NewScripted(responses...)
	svc, err := llm.Open(store, llm.Options{
		Getenv:    func(k string) string { return env[k] },
		Secrets:   &llm.MemorySecrets{},
		NewClient: func(*llm.Resolved) (llm.Client, error) { return model, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, model
}

func openStore(t *testing.T) *core.Store {
	t.Helper()
	tick := 0
	s, err := core.Open(core.Options{DataDir: t.TempDir(), Now: func() time.Time {
		tick++
		return time.Date(2026, 10, 6, 2, 0, 0, tick*1000, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSummarizeAndTemplateLifecycle(t *testing.T) {
	store := openStore(t)
	env := map[string]string{llm.EnvAPIKey: "sk-prompt-secret-123456"}
	svc, model := newLLM(t, store, env, modelSummary())
	lib, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	lib.Redact = svc.Redact

	original := longPrompt + "\nAPI key: sk-prompt-secret-123456"
	res, err := SummarizeWithFallback(context.Background(), svc, "", original, "整理为模板", llm.ErrUnavailable)
	if err != nil || res.Degraded {
		t.Fatalf("%+v %v", res, err)
	}
	sum := res.Summary
	if strings.Contains(model.LastRequest().Messages[1].Content, "sk-prompt-secret") {
		t.Fatal("key sent to the model")
	}
	if len(sum.Constraints) != 2 || len(sum.Steps) != 3 {
		t.Fatalf("summary %+v", sum)
	}
	var names []string
	for _, v := range sum.Variables {
		names = append(names, v.Name)
	}
	if !slices.Equal(names, []string{"产品", "周期", "数据文件"}) {
		t.Fatalf("variables %v", names)
	}

	// Save: original and summary are both kept.
	tpl, err := lib.Create("销售分析", original, *sum, SourceModel)
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Current != 1 || !strings.Contains(tpl.Original, "/data/sales_2026.csv") || strings.Contains(tpl.Original, "sk-prompt-secret") {
		t.Fatalf("template %+v", tpl)
	}
	for _, sec := range []string{"## 目标", "## 约束", "## 执行步骤", "## 输出格式", "{{产品}}", "`region`"} {
		if !strings.Contains(tpl.Latest.Body, sec) {
			t.Errorf("body lacks %q:\n%s", sec, tpl.Latest.Body)
		}
	}
	if _, err := lib.Create("销售分析", original, *sum, SourceModel); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate name: %v", err)
	}

	// Read by name or id prefix.
	if got, err := lib.Get(tpl.ID[:6]); err != nil || got.Name != "销售分析" {
		t.Fatalf("get by prefix: %+v %v", got, err)
	}

	// Edit again: a new version, the old one stays.
	edited := *sum
	edited.Steps = append(edited.Steps, "给出改进建议")
	tpl, err = lib.Edit("销售分析", edited, nil, SourceManual, "加一步")
	if err != nil || tpl.Current != 2 || !strings.Contains(tpl.Latest.Body, "4. 给出改进建议") {
		t.Fatalf("edit: %+v %v", tpl, err)
	}
	body := "请为 {{产品}} 写 {{字数}} 字总结。"
	tpl, err = lib.Edit("销售分析", edited, &body, SourceManual, "手写正文")
	if err != nil || tpl.Latest.Body != body || !slices.ContainsFunc(tpl.Latest.Summary.Variables, func(v Variable) bool { return v.Name == "字数" }) {
		t.Fatalf("manual body: %+v %v", tpl.Latest, err)
	}

	// Variable substitution.
	if _, err := Render(tpl.Latest.Body, tpl.Latest.Summary.Variables, map[string]string{"产品": "A"}); err == nil {
		t.Fatal("missing variable accepted")
	} else if me := (*MissingVariablesError)(nil); !errors.As(err, &me) || me.Names[0] != "字数" {
		t.Fatalf("missing error %v", err)
	}
	if _, err := Render(tpl.Latest.Body, tpl.Latest.Summary.Variables, map[string]string{"产品": "A", "字数": "1", "typo": "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown variable: %v", err)
	}
	out, err := Render(tpl.Latest.Body, tpl.Latest.Summary.Variables, map[string]string{"产品": "<FlowOS & \"Pro\">", "字数": "300"})
	if err != nil || out != "请为 <FlowOS & \"Pro\"> 写 300 字总结。" {
		t.Fatalf("render %q %v", out, err)
	}

	// Roll back to v1 (as a new version v4), all versions stay viewable.
	tpl, err = lib.Rollback(tpl.ID, 1)
	if err != nil || tpl.Current != 4 || tpl.Latest.Source != SourceRollback || !strings.Contains(tpl.Latest.Body, "## 目标") {
		t.Fatalf("rollback %+v %v", tpl, err)
	}
	vs, err := lib.Versions(tpl.ID)
	if err != nil || len(vs) != 4 {
		t.Fatalf("versions %d %v", len(vs), err)
	}
	if tpl, err = lib.Rollback(tpl.ID, 0); err != nil || tpl.Latest.Body != body {
		t.Fatalf("rollback to previous: %v", err)
	}

	// Defaults fill in, then copy and export.
	v1, _ := lib.Version(tpl.ID, 1)
	rendered, err := Render(v1.Body, v1.Summary.Variables, map[string]string{"产品": "B", "数据文件": "/tmp/x.csv"})
	if err != nil || !strings.Contains(rendered, "上周") {
		t.Fatalf("defaults: %v\n%s", err, rendered)
	}
	cp, err := lib.Copy("销售分析", 1, "")
	if err != nil || cp.Name != "销售分析 副本" || cp.Current != 1 || cp.Latest.Body != v1.Body || cp.Original != tpl.Original || cp.Latest.Source != SourceCopy {
		t.Fatalf("copy %+v %v", cp, err)
	}
	if _, err := lib.Copy("销售分析", 0, "销售分析 副本"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("copy name clash: %v", err)
	}
	md, err := lib.Export("销售分析", 1, FormatMarkdown)
	if err != nil || !strings.HasPrefix(string(md), "# 销售分析") || !strings.Contains(string(md), "v1") {
		t.Fatalf("md %s %v", md, err)
	}
	txt, _ := lib.Export("销售分析", 0, FormatText)
	if string(txt) != body {
		t.Fatalf("txt %q", txt)
	}
	raw, err := lib.Export("销售分析", 0, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Format   string   `json:"format"`
		Template Template `json:"template"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Format != "todo-cli/prompt" || len(doc.Template.Versions) != 5 ||
		doc.Template.Original != tpl.Original {
		t.Fatalf("json export %v %+v", err, doc)
	}
	if _, err := lib.Export("销售分析", 0, "pdf"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad format: %v", err)
	}

	// Create an agent task from the template.
	nt := AgentTask(tpl, tpl.Latest, rendered, "")
	task, err := store.Create(nt)
	if err != nil || task.Description != rendered || !slices.Contains(task.Tags, "agent") || !strings.Contains(task.Notes, tpl.ID) {
		t.Fatalf("agent task %+v %v", task, err)
	}

	list, err := lib.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list %d %v", len(list), err)
	}
	if _, err := lib.Rename(cp.ID, "销售分析"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename clash: %v", err)
	}
	if err := lib.Delete(cp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Get(cp.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted template: %v", err)
	}
}

func TestSummarizeFallsBackWithoutModel(t *testing.T) {
	store := openStore(t)
	svc, model := newLLM(t, store, map[string]string{}) // no key
	res, err := SummarizeWithFallback(context.Background(), svc, "", longPrompt, "", llm.ErrUnavailable)
	if err != nil || !res.Degraded || res.Reason == "" || len(model.Requests) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	s := res.Summary
	if s.Goal != "你是资深数据分析师。请分析 {{产品}} 上周的销售数据。" || len(s.Steps) != 3 || len(s.Constraints) != 1 ||
		!strings.Contains(s.OutputFormat, "Markdown") || !strings.Contains(s.Context, "`region`") || s.Variables[0].Name != "产品" {
		t.Fatalf("local summary %+v", s)
	}
	// Other errors are not hidden by the fallback.
	if _, err := SummarizeWithFallback(context.Background(), svc, "", "  ", "", llm.ErrUnavailable); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty prompt: %v", err)
	}
}

func TestNormalizeRejectsBadSummaries(t *testing.T) {
	if err := (&Summary{}).Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatal("summary without goal accepted")
	}
	if err := (&Summary{Goal: "x", Variables: []Variable{{Name: "a b"}}}).Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad variable name accepted")
	}
	if got := Placeholders("{{ a }} {{b_1}} {{a}} {x} {{中文}}"); !slices.Equal(got, []string{"a", "b_1", "中文"}) {
		t.Fatalf("placeholders %v", got)
	}
}
