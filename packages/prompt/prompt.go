// Package prompt turns long prompts into structured, reusable templates
// (goal, context, constraints, steps, output format, variables) and keeps
// them in the task database with every version: the original prompt and
// each summary or edit stay viewable, edits never overwrite, and any
// version can be restored.
package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Variable is a placeholder ({{name}}) filled in on every use.
type Variable struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
}

// Summary is the structured form of a prompt.
type Summary struct {
	Goal         string     `json:"goal"`
	Context      string     `json:"context,omitempty"`
	Constraints  []string   `json:"constraints"`
	Steps        []string   `json:"steps"`
	OutputFormat string     `json:"output_format,omitempty"`
	Variables    []Variable `json:"variables"`
}

// ErrInvalid reports bad input.
var ErrInvalid = fmt.Errorf("invalid_input")

var (
	placeholderRE = regexp.MustCompile(`\{\{\s*([\p{L}_][\p{L}\p{N}_\-]*)\s*\}\}`)
	nameRE        = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_\-]*$`)
)

// Placeholders lists the variable names used in text, in order of first use.
func Placeholders(text string) []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Normalize trims the summary, validates variable names and declares every
// placeholder that is used but not listed.
func (s *Summary) Normalize() error {
	s.Goal = strings.TrimSpace(s.Goal)
	s.Context = strings.TrimSpace(s.Context)
	s.OutputFormat = strings.TrimSpace(s.OutputFormat)
	s.Constraints = cleanLines(s.Constraints)
	s.Steps = cleanLines(s.Steps)
	if s.Goal == "" {
		return fmt.Errorf("%w: a prompt summary needs a goal", ErrInvalid)
	}
	var vars []Variable
	for _, v := range s.Variables {
		v.Name = strings.Trim(strings.TrimSpace(v.Name), "{} ")
		if !nameRE.MatchString(v.Name) {
			return fmt.Errorf("%w: invalid variable name %q (letters, digits, _ and -)", ErrInvalid, v.Name)
		}
		if slices.ContainsFunc(vars, func(o Variable) bool { return o.Name == v.Name }) {
			continue
		}
		v.Description, v.Default = strings.TrimSpace(v.Description), strings.TrimSpace(v.Default)
		vars = append(vars, v)
	}
	for _, name := range Placeholders(s.text()) {
		if !slices.ContainsFunc(vars, func(o Variable) bool { return o.Name == name }) {
			vars = append(vars, Variable{Name: name})
		}
	}
	if vars == nil {
		vars = []Variable{}
	}
	s.Variables = vars
	return nil
}

func (s *Summary) text() string {
	return strings.Join(append(append([]string{s.Goal, s.Context, s.OutputFormat}, s.Constraints...), s.Steps...), "\n")
}

func cleanLines(in []string) []string {
	out := []string{}
	for _, l := range in {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Body renders the summary as the reusable prompt text, with the variable
// placeholders left in place. The variables themselves are metadata (see
// VariablesText), so a rendered prompt holds only the filled-in text.
func (s *Summary) Body() string {
	var b strings.Builder
	section := func(title, text string) {
		if text != "" {
			fmt.Fprintf(&b, "## %s\n%s\n\n", title, text)
		}
	}
	list := func(items []string, numbered bool) string {
		var l []string
		for i, it := range items {
			if numbered {
				l = append(l, fmt.Sprintf("%d. %s", i+1, it))
			} else {
				l = append(l, "- "+it)
			}
		}
		return strings.Join(l, "\n")
	}
	section("目标", s.Goal)
	section("上下文", s.Context)
	section("约束", list(s.Constraints, false))
	section("执行步骤", list(s.Steps, true))
	section("输出格式", s.OutputFormat)
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// MissingVariablesError lists variables without a value.
type MissingVariablesError struct{ Names []string }

func (e *MissingVariablesError) Error() string {
	return fmt.Sprintf("%v: missing values for variables: %s", ErrInvalid, strings.Join(e.Names, ", "))
}

func (e *MissingVariablesError) Unwrap() error { return ErrInvalid }

// Render replaces the placeholders of body with values (or the variables'
// defaults). Every placeholder needs a value; unknown names in values are
// an error, so typos do not pass silently.
func Render(body string, vars []Variable, values map[string]string) (string, error) {
	known := map[string]Variable{}
	for _, v := range vars {
		known[v.Name] = v
	}
	used := Placeholders(body)
	var unknown []string
	for k := range values {
		if _, ok := known[k]; !ok && !slices.Contains(used, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", fmt.Errorf("%w: unknown variables: %s (template has: %s)", ErrInvalid, strings.Join(unknown, ", "), strings.Join(used, ", "))
	}
	var missing []string
	out := placeholderRE.ReplaceAllStringFunc(body, func(m string) string {
		name := placeholderRE.FindStringSubmatch(m)[1]
		if v, ok := values[name]; ok {
			return v
		}
		if d := known[name].Default; d != "" {
			return d
		}
		if !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
		return m
	})
	if len(missing) > 0 {
		return "", &MissingVariablesError{Names: missing}
	}
	return out, nil
}

// Asker sends a JSON request to the configured model (llm.Service).
type Asker interface {
	AskJSON(ctx context.Context, purpose, profile, system, user string, v any) error
}

const summarizeFormat = `Return one JSON object only:
{"goal": "...", "context": "...", "constraints": ["..."], "steps": ["..."], "output_format": "...",
 "variables": [{"name": "...", "description": "...", "default": "..."}]}
Use {{name}} placeholders in the other fields for everything that changes from one use to the next
(inputs, names, dates, files, numbers). Keep code, paths and special characters exactly. Answer in the prompt's language.`

// Summarize asks the model to structure a long prompt. guidance is the
// (self-optimisable) instruction from the assistant configuration.
func Summarize(ctx context.Context, a Asker, profile, text, guidance string) (*Summary, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: the prompt is empty", ErrInvalid)
	}
	user, _ := json.Marshal(map[string]string{"prompt": text})
	var s Summary
	if err := a.AskJSON(ctx, "summarize", profile, guidance+"\n\n"+summarizeFormat, string(user), &s); err != nil {
		return nil, err
	}
	if err := s.Normalize(); err != nil {
		return nil, fmt.Errorf("the model's summary is unusable: %w", err)
	}
	return &s, nil
}

var (
	stepLineRE       = regexp.MustCompile(`^(?:\d+[.、)）]|第[一二三四五六七八九十\d]+步[:：、]?|步骤\s*\d*[:：、]?|step\s*\d+[:.)]?)\s*`)
	bulletRE         = regexp.MustCompile(`^(?:[-*•·]|#+)\s*`)
	constraintWordRE = regexp.MustCompile(`(?i)必须|不要|不得|禁止|避免|只能|务必|不能|限制|\bmust\b|\bshould\b|\bnever\b|\bdon't\b|\bdo not\b|\bavoid\b|\bonly\b`)
	outputWordRE     = regexp.MustCompile(`(?i)输出|格式|返回|\boutput\b|\bformat\b|\brespond\b|\breturn\b`)
)

// LocalSummary structures a prompt without a model, by line rules: numbered
// lines are steps, lines with must/不要/禁止… are constraints, lines about
// 输出/格式 describe the output, the first other line is the goal and the
// rest is context. It is the fallback when no model is available.
func LocalSummary(text string) (*Summary, error) {
	s := &Summary{}
	var rest []string
	for _, raw := range strings.Split(strings.TrimSpace(text), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case stepLineRE.MatchString(line):
			s.Steps = append(s.Steps, strings.TrimSpace(stepLineRE.ReplaceAllString(line, "")))
		case constraintWordRE.MatchString(line):
			s.Constraints = append(s.Constraints, bulletRE.ReplaceAllString(line, ""))
		case outputWordRE.MatchString(line):
			if s.OutputFormat != "" {
				s.OutputFormat += "\n"
			}
			s.OutputFormat += bulletRE.ReplaceAllString(line, "")
		default:
			rest = append(rest, bulletRE.ReplaceAllString(line, ""))
		}
	}
	if len(rest) > 0 {
		s.Goal, s.Context = rest[0], strings.Join(rest[1:], "\n")
	} else if len(s.Steps) > 0 {
		s.Goal = s.Steps[0]
	} else {
		s.Goal = strings.TrimSpace(text)
	}
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return s, nil
}

// Summarized is a summary and how it was made.
type Summarized struct {
	Summary *Summary `json:"summary"`
	// Degraded means the model was unavailable and LocalSummary answered.
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"degraded_reason,omitempty"`
}

// SummarizeWithFallback summarises with the model and falls back to
// LocalSummary when the model fails with an error matching unavailable
// (llm.ErrUnavailable), so prompt work never depends on the model.
func SummarizeWithFallback(ctx context.Context, a Asker, profile, text, guidance string, unavailable error) (*Summarized, error) {
	sum, err := Summarize(ctx, a, profile, text, guidance)
	if err == nil {
		return &Summarized{Summary: sum}, nil
	}
	if unavailable == nil || !errors.Is(err, unavailable) {
		return nil, err
	}
	local, lerr := LocalSummary(text)
	if lerr != nil {
		return nil, lerr
	}
	return &Summarized{Summary: local, Degraded: true, Reason: err.Error()}, nil
}

// VariablesText lists the variables for display, one per line.
func VariablesText(vars []Variable) string {
	var l []string
	for _, v := range vars {
		line := "- {{" + v.Name + "}}"
		if v.Description != "" {
			line += "：" + v.Description
		}
		if v.Default != "" {
			line += "（默认：" + v.Default + "）"
		}
		l = append(l, line)
	}
	return strings.Join(l, "\n")
}
