package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

// clipboardWrite copies text to the macOS clipboard; tests replace it.
var clipboardWrite = func(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func (a *app) prompts() (*prompt.Library, *llm.Service, error) {
	svc, err := a.llm()
	if err != nil {
		return nil, nil, err
	}
	lib, err := prompt.Open(svc.Store())
	if err != nil {
		return nil, nil, err
	}
	lib.Redact = svc.Redact
	return lib, svc, nil
}

func cmdPrompt(a *app, args []string) error {
	return a.runGroup("prompt", []subcommand{
		{"summarize", "[file|-] [--save [--name N]] [--local]", "Summarise a long prompt into goal, context, constraints, steps, output format and variables", promptSummarize},
		{"list", "", "List saved prompt templates", promptList},
		{"show", "<template> [--version N] [--original] [--raw]", "Show a template (current or a given version, or the original prompt)", promptShow},
		{"versions", "<template>", "List a template's versions", promptVersions},
		{"edit", "<template> [--goal G] [--context C] [--constraint X]... [--step S]... [--output O] [--body-file F]", "Edit a template again (stored as a new version)", promptEdit},
		{"rollback", "<template> [version]", "Restore an earlier version (default: the previous one) as a new version", promptRollback},
		{"copy", "<template> [--name N] [--version N] [--clipboard]", "Duplicate a template, or copy its text to the clipboard", promptCopy},
		{"export", "<template> [--format md|txt|json] [--version N] [-o FILE]", "Export a template", promptExport},
		{"render", "<template> [--var name=value]...", "Fill in the variables and print the final prompt", promptRender},
		{"task", "<template> [--var name=value]... [--title T]", "Create an agent task from a template (the final prompt is the description)", promptTask},
		{"rename", "<template> <name>", "Rename a template", promptRename},
		{"delete", "<template>", "Delete a template and its versions", promptDelete},
	}, "list", args)
}

// readText reads a file argument, "-" or piped stdin.
func (a *app) readText(pos []string) (string, error) {
	switch {
	case len(pos) == 1 && pos[0] != "-":
		b, err := os.ReadFile(pos[0])
		return string(b), err
	case len(pos) > 1:
		return "", usagef("pass one file, or the text on stdin")
	}
	b, err := io.ReadAll(a.env.Stdin)
	return string(b), err
}

type summarizeOut struct {
	*prompt.Summarized
	Body     string           `json:"body"`
	Template *prompt.Template `json:"template,omitempty"`
}

func promptSummarize(a *app, args []string) error {
	var save, local *bool
	var name, profile *string
	pos, err := a.subFlags("prompt", "summarize", "[file|-]", func(f *fset) {
		save = f.Bool("save", false, "save the original and the summary as a template")
		name = f.String("name", "", "template name (with --save)")
		local = f.Bool("local", false, "summarise with local rules, without the model")
		profile = f.String("profile", "", "model profile")
	}, args)
	if err != nil {
		return err
	}
	text, err := a.readText(pos)
	if err != nil {
		return err
	}
	lib, svc, err := a.prompts()
	if err != nil {
		return err
	}
	var res *prompt.Summarized
	if *local {
		sum, err := prompt.LocalSummary(text)
		if err != nil {
			return err
		}
		res = &prompt.Summarized{Summary: sum}
	} else {
		cfg, _, err := svc.AssistConfig()
		if err != nil {
			return err
		}
		ctx, cancel := a.ctx()
		defer cancel()
		if res, err = prompt.SummarizeWithFallback(ctx, svc, *profile, text, cfg.Prompts.Summarize, llm.ErrUnavailable); err != nil {
			return err
		}
	}
	out := summarizeOut{Summarized: res, Body: res.Summary.Body()}
	if *save {
		source := prompt.SourceModel
		if *local || res.Degraded {
			source = prompt.SourceManual
		}
		if out.Template, err = lib.Create(*name, text, *res.Summary, source); err != nil {
			return err
		}
	}
	if a.json {
		return a.emitJSON(out)
	}
	if res.Degraded {
		fmt.Fprintf(a.env.Stderr, "todo: 大模型不可用，已按本地规则整理（%s）\n", res.Reason)
	}
	a.printf("%s", out.Body)
	if vars := prompt.VariablesText(res.Summary.Variables); vars != "" {
		a.printf("\n## 变量\n%s\n", vars)
	}
	if out.Template != nil {
		a.printf("\n已保存模板 %s（%s，v%d）\n", out.Template.Name, out.Template.ID, out.Template.Current)
	}
	return nil
}

func promptList(a *app, args []string) error {
	if _, err := a.subFlags("prompt", "list", "", nil, args); err != nil {
		return err
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	list, err := lib.List()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"templates": list})
	}
	if len(list) == 0 {
		a.printf("没有提示词模板（todo prompt summarize <文件> --save）\n")
	}
	for _, t := range list {
		var vars []string
		for _, v := range t.Latest.Summary.Variables {
			vars = append(vars, v.Name)
		}
		a.printf("%s  %-24s v%-3d %s  变量：%s\n", shortID(t.ID), t.Name, t.Current, a.localTime(t.UpdatedAt), orNone(strings.Join(vars, ", ")))
	}
	return nil
}

func versionFlag(v **int) func(*fset) {
	return func(f *fset) { *v = f.Int("version", 0, "version number (default: current)") }
}

func promptShow(a *app, args []string) error {
	var version *int
	var original, raw *bool
	pos, err := a.subFlags("prompt", "show", "<template>", func(f *fset) {
		versionFlag(&version)(f)
		original = f.Bool("original", false, "show the original prompt")
		raw = f.Bool("raw", false, "print only the text (for pipes)")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt show <template>")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, err := lib.Get(pos[0])
	if err != nil {
		return err
	}
	ver := t.Latest
	if *version != 0 {
		if ver, err = lib.Version(t.ID, *version); err != nil {
			return err
		}
	}
	if a.json {
		return a.emitJSON(map[string]any{"template": t, "version": ver})
	}
	text := ver.Body
	if *original {
		text = t.Original
	}
	if *raw {
		a.printf("%s", text)
		return nil
	}
	label := fmt.Sprintf("v%d (%s)", ver.Version, ver.Source)
	if *original {
		label = "原始提示词"
	}
	a.printf("%s  %s  %s\n\n%s", t.Name, t.ID, label, text)
	if !strings.HasSuffix(text, "\n") {
		a.printf("\n")
	}
	if vars := prompt.VariablesText(ver.Summary.Variables); vars != "" && !*original {
		a.printf("\n## 变量\n%s\n", vars)
	}
	return nil
}

func promptVersions(a *app, args []string) error {
	pos, err := a.subFlags("prompt", "versions", "<template>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt versions <template>")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	vs, err := lib.Versions(pos[0])
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"versions": vs})
	}
	for _, v := range vs {
		a.printf("v%-3d %-9s %s  %s\n", v.Version, v.Source, a.localTime(v.CreatedAt), v.Note)
	}
	return nil
}

func promptEdit(a *app, args []string) error {
	var goal, context, output, bodyFile, note *string
	var constraints, steps *rawList
	var fs *fset
	pos, err := a.subFlags("prompt", "edit", "<template>", func(f *fset) {
		fs = f
		goal = f.String("goal", "", "goal")
		context = f.String("context", "", "context")
		output = f.String("output", "", "output format")
		constraints, steps = new(rawList), new(rawList)
		f.Var(constraints, "constraint", "constraints (replace all; repeatable)")
		f.Var(steps, "step", "steps (replace all; repeatable)")
		bodyFile = f.String("body-file", "", "use this file (or -) as the template text; {{name}} placeholders become variables")
		note = f.String("note", "", "what changed")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt edit <template> [flags]")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, err := lib.Get(pos[0])
	if err != nil {
		return err
	}
	sum := t.Latest.Summary
	if flagSet(fs.FlagSet, "goal") {
		sum.Goal = *goal
	}
	if flagSet(fs.FlagSet, "context") {
		sum.Context = *context
	}
	if flagSet(fs.FlagSet, "output") {
		sum.OutputFormat = *output
	}
	if flagSet(fs.FlagSet, "constraint") {
		sum.Constraints = *constraints
	}
	if flagSet(fs.FlagSet, "step") {
		sum.Steps = *steps
	}
	var body *string
	if *bodyFile != "" {
		text, err := a.readText([]string{*bodyFile})
		if err != nil {
			return err
		}
		body = &text
	}
	if body == nil && !anySet(fs.FlagSet, "goal", "context", "output", "constraint", "step") {
		return usagef("prompt edit: nothing to change (use --goal, --step, --body-file …)")
	}
	t, err = lib.Edit(t.ID, sum, body, prompt.SourceManual, *note)
	if err != nil {
		return err
	}
	return a.printTemplateDone(t, "已保存新版本")
}

func (a *app) printTemplateDone(t *prompt.Template, msg string) error {
	if a.json {
		return a.emitJSON(t)
	}
	a.printf("%s：%s v%d（%s）\n", msg, t.Name, t.Current, t.ID)
	return nil
}

func promptRollback(a *app, args []string) error {
	pos, err := a.subFlags("prompt", "rollback", "<template> [version]", nil, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("usage: todo prompt rollback <template> [version]")
	}
	v := 0
	if len(pos) == 2 {
		if v, err = strconv.Atoi(strings.TrimPrefix(pos[1], "v")); err != nil {
			return usagef("prompt rollback: %q is not a version", pos[1])
		}
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, err := lib.Rollback(pos[0], v)
	if err != nil {
		return err
	}
	return a.printTemplateDone(t, "已恢复（"+t.Latest.Note+"）")
}

func promptCopy(a *app, args []string) error {
	var name *string
	var version *int
	var clip *bool
	pos, err := a.subFlags("prompt", "copy", "<template>", func(f *fset) {
		name = f.String("name", "", "name of the copy (default: \"<name> 副本\")")
		versionFlag(&version)(f)
		clip = f.Bool("clipboard", false, "copy the template text to the clipboard instead")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt copy <template>")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	if *clip {
		text, err := lib.Export(pos[0], *version, prompt.FormatText)
		if err != nil {
			return err
		}
		if err := clipboardWrite(string(text)); err != nil {
			return fmt.Errorf("copy to clipboard: %w", err)
		}
		if a.json {
			return a.emitJSON(map[string]any{"copied": true, "bytes": len(text)})
		}
		a.printf("已复制到剪贴板（%d 字节）\n", len(text))
		return nil
	}
	t, err := lib.Copy(pos[0], *version, *name)
	if err != nil {
		return err
	}
	return a.printTemplateDone(t, "已复制为新模板")
}

func promptExport(a *app, args []string) error {
	var format, out *string
	var version *int
	pos, err := a.subFlags("prompt", "export", "<template>", func(f *fset) {
		format = f.String("format", "md", "md, txt or json (json keeps the original and every version)")
		out = f.String("o", "", "write to this file instead of stdout")
		versionFlag(&version)(f)
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt export <template> [--format md|txt|json] [-o FILE]")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	b, err := lib.Export(pos[0], *version, *format)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = a.env.Stdout.Write(b)
		return err
	}
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"path": *out, "bytes": len(b)})
	}
	a.printf("已导出到 %s\n", *out)
	return nil
}

// rawList is a repeatable flag that keeps each value verbatim.
type rawList []string

func (l *rawList) String() string     { return strings.Join(*l, " ") }
func (l *rawList) Set(v string) error { *l = append(*l, v); return nil }

func varsFlag(v **rawList) func(*fset) {
	return func(f *fset) {
		p := new(rawList)
		f.Var(p, "var", "variable value name=value (repeatable; the value is kept verbatim)")
		*v = p
	}
}

// parseVars reads name=value pairs.
func parseVars(list []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, usagef("--var %q: want name=value", kv)
		}
		out[k] = v
	}
	return out, nil
}

func (a *app) renderTemplate(lib *prompt.Library, ref string, version int, vars []string) (*prompt.Template, *prompt.Version, string, error) {
	t, err := lib.Get(ref)
	if err != nil {
		return nil, nil, "", err
	}
	ver := t.Latest
	if version != 0 {
		if ver, err = lib.Version(t.ID, version); err != nil {
			return nil, nil, "", err
		}
	}
	values, err := parseVars(vars)
	if err != nil {
		return nil, nil, "", err
	}
	text, err := prompt.Render(ver.Body, ver.Summary.Variables, values)
	return t, ver, text, err
}

func promptRender(a *app, args []string) error {
	var vars *rawList
	var version *int
	pos, err := a.subFlags("prompt", "render", "<template>", func(f *fset) {
		varsFlag(&vars)(f)
		versionFlag(&version)(f)
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt render <template> --var name=value …")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	_, _, text, err := a.renderTemplate(lib, pos[0], *version, *vars)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"prompt": text})
	}
	a.printf("%s", text)
	return nil
}

func promptTask(a *app, args []string) error {
	var vars *rawList
	var version *int
	var title *string
	pos, err := a.subFlags("prompt", "task", "<template>", func(f *fset) {
		varsFlag(&vars)(f)
		versionFlag(&version)(f)
		title = f.String("title", "", "task title (default: 智能体任务：<template>)")
	}, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt task <template> --var name=value …")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, ver, text, err := a.renderTemplate(lib, pos[0], *version, *vars)
	if err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	task, err := s.Create(prompt.AgentTask(t, ver, text, *title))
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task": task, "prompt": text})
	}
	a.printf("已创建智能体任务 %s\n最终提示词：\n%s", a.line(task), text)
	if !strings.HasSuffix(text, "\n") {
		a.printf("\n")
	}
	return nil
}

func promptRename(a *app, args []string) error {
	pos, err := a.subFlags("prompt", "rename", "<template> <name>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("usage: todo prompt rename <template> <name>")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, err := lib.Rename(pos[0], strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	return a.printTemplateDone(t, "已重命名")
}

func promptDelete(a *app, args []string) error {
	pos, err := a.subFlags("prompt", "delete", "<template>", nil, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: todo prompt delete <template>")
	}
	lib, _, err := a.prompts()
	if err != nil {
		return err
	}
	t, err := lib.Get(pos[0])
	if err != nil {
		return err
	}
	if err := lib.Delete(t.ID); err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"deleted": t.ID})
	}
	a.printf("已删除模板 %s\n", t.Name)
	return nil
}
