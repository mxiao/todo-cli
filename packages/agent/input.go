package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Input is the JSON document an agent receives (on stdin or as an
// argument, see Spec.Input). Prompt is the same content as one text.
type Input struct {
	Protocol string   `json:"protocol"`
	RunID    string   `json:"run_id"`
	Attempt  int      `json:"attempt"`
	Agent    string   `json:"agent"`
	Task     TaskInfo `json:"task"`
	Context  Context  `json:"context"`
	// Constraints the agent must respect.
	Constraints []string `json:"constraints"`
	// OutputFormat describes how to report progress and results.
	OutputFormat string   `json:"output_format"`
	ResultTypes  []string `json:"result_types"`
	// IdempotencyKey prefixes the keys of this run's results
	// (task id:run id); the result type completes it.
	IdempotencyKey string `json:"idempotency_key"`
	Workdir        string `json:"workdir"`
	Prompt         string `json:"prompt"`
}

// TaskInfo is the task as passed to an agent.
type TaskInfo struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	Status      string     `json:"status"`
	Priority    string     `json:"priority"`
	Tags        []string   `json:"tags,omitempty"`
	Category    string     `json:"category,omitempty"`
	DueAt       *time.Time `json:"due_at,omitempty"`
}

// TaskRef is a related task.
type TaskRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// ResultRef is a result already written back for the task; agents use it
// to skip work that is done (a retry must not repeat side effects).
type ResultRef struct {
	Type    string `json:"type"`
	Key     string `json:"key"`
	Version int    `json:"version"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
}

// Context is what the agent should know besides the task itself.
type Context struct {
	Parent          *TaskRef    `json:"parent,omitempty"`
	DependsOn       []TaskRef   `json:"depends_on,omitempty"`
	Subtasks        []TaskRef   `json:"subtasks,omitempty"`
	PreviousResults []ResultRef `json:"previous_results,omitempty"`
	// Instructions come from a prompt template or a model session.
	Instructions string `json:"instructions,omitempty"`
	// User is extra context given when the run was started or retried.
	User      string `json:"user,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// BaseConstraints apply to every agent.
var BaseConstraints = []string{
	"以当前 macOS 用户的权限运行，不得使用 sudo、su 等方式提权，不得访问未授权的目录。",
	"只在工作目录内读写文件，除非任务明确授权了其他路径。",
	"未实际完成时不要声称已完成；失败或只完成一部分时如实报告。",
	"不要在输出中打印密钥、令牌、密码等敏感信息。",
	"重试时先检查已有结果（previous_results），不要重复已完成的提交、写入或发送。",
}

// OutputFormatJSONL describes the protocol of the jsonl output mode.
const OutputFormatJSONL = `标准输出使用 JSON Lines（每行一个 JSON 对象），其他行视为普通输出：
{"type":"progress","stage":"阶段名","percent":50,"message":"说明"}
{"type":"log","level":"info","message":"日志"}
{"type":"command","argv":["git","status"],"exit_code":0}
{"type":"error","message":"错误原因","retryable":false}
{"type":"result","result_type":"text","text":"结果正文","summary":"摘要"}
{"type":"result","result_type":"file","path":"相对或绝对路径","summary":"文件摘要"}
{"type":"result","result_type":"commit","repo":"仓库","branch":"分支","commit":"提交哈希","message":"说明"}
{"type":"result","result_type":"command_output","argv":["make","test"],"exit_code":0,"stdout":"…"}
{"type":"result","result_type":"data","data":{}}
{"type":"result","result_type":"task_status","status":"外部状态","url":"关联地址","local_status":"done"}
{"type":"status","status":"succeeded|partial|failed","message":"总结"}
同一类型有多个结果时加 "key" 区分。退出码 0 表示成功。`

const (
	outputFormatJSON = `结束时在标准输出写一个 JSON 对象：{"status":"succeeded|partial|failed","message":"总结","results":[{"type":"text|file|commit|command_output|data|task_status", …}]}。退出码 0 表示成功。`
	outputFormatText = `直接输出结果正文，全部标准输出会作为文本结果回写。退出码 0 表示成功。`
)

func outputFormat(sp Spec, extra string) string {
	f := OutputFormatJSONL
	switch sp.Output {
	case OutputJSON:
		f = outputFormatJSON
	case OutputText:
		f = outputFormatText
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		f += "\n输出要求：" + extra
	}
	return f
}

// buildInput assembles the agent input for a task.
func (m *Manager) buildInput(t *core.Task, sp Spec, req StartRequest, runID, dir string) (*Input, error) {
	in := &Input{Protocol: Protocol, RunID: runID, Agent: sp.Name, ResultTypes: ResultTypes, Workdir: dir,
		IdempotencyKey: t.ID + ":" + runID,
		Task: TaskInfo{ID: t.ID, Title: t.Title, Description: t.Description, Notes: t.Notes, Status: string(t.Status),
			Priority: t.Priority.String(), Tags: t.Tags, Category: t.Category, DueAt: t.DueAt}}
	if t.ParentID != "" {
		if p, err := m.store.Get(t.ParentID); err == nil {
			in.Context.Parent = &TaskRef{ID: p.ID, Title: p.Title, Status: string(p.Status)}
		}
	}
	for _, id := range t.DependsOn {
		if d, err := m.store.Get(id); err == nil {
			in.Context.DependsOn = append(in.Context.DependsOn, TaskRef{ID: d.ID, Title: d.Title, Status: string(d.Status)})
		}
	}
	if subs, err := m.store.List(core.Filter{ParentID: &t.ID, Statuses: core.Statuses}); err == nil {
		for _, s := range subs {
			in.Context.Subtasks = append(in.Context.Subtasks, TaskRef{ID: s.ID, Title: s.Title, Status: string(s.Status)})
		}
	}
	if rs, err := m.TaskResults(t.ID); err == nil {
		for _, r := range rs {
			in.Context.PreviousResults = append(in.Context.PreviousResults, ResultRef{Type: r.Type, Key: r.Key, Version: r.Version, Agent: r.Agent, Summary: r.Summary})
		}
	}
	in.Context.User, in.Context.SessionID = strings.TrimSpace(req.Context), req.SessionID
	if req.Template != "" {
		if m.opts.Prompts == nil {
			return nil, fmt.Errorf("%w: prompt templates are not available", ErrInvalid)
		}
		text, err := m.renderTemplate(req.Template, req.Vars)
		if err != nil {
			return nil, err
		}
		in.Context.Instructions = text
	}
	in.Constraints = append(append(append([]string{}, BaseConstraints...), sp.Constraints...), req.Constraints...)
	in.OutputFormat = outputFormat(sp, req.OutputFormat)
	in.Prompt = renderPrompt(in)
	if strings.TrimSpace(req.Prompt) != "" {
		in.Prompt = req.Prompt
	}
	return in, nil
}

// renderPrompt is the final prompt: task, context, constraints and output
// format in one text, shown to the user before and after the start.
func renderPrompt(in *Input) string {
	var b strings.Builder
	if in.Context.Instructions != "" {
		b.WriteString("# 指令\n" + in.Context.Instructions + "\n\n")
	}
	t := in.Task
	b.WriteString("# 任务\n")
	fmt.Fprintf(&b, "标题：%s\n", t.Title)
	if t.Description != "" {
		fmt.Fprintf(&b, "描述：%s\n", t.Description)
	}
	if t.Notes != "" {
		fmt.Fprintf(&b, "备注：%s\n", t.Notes)
	}
	fmt.Fprintf(&b, "状态：%s；优先级：%s\n", t.Status, t.Priority)
	if t.DueAt != nil {
		fmt.Fprintf(&b, "截止：%s\n", t.DueAt.Local().Format("2006-01-02 15:04"))
	}
	if len(t.Tags) > 0 {
		fmt.Fprintf(&b, "标签：%s\n", strings.Join(t.Tags, ", "))
	}
	if t.Category != "" {
		fmt.Fprintf(&b, "分类：%s\n", t.Category)
	}
	fmt.Fprintf(&b, "任务 ID：%s\n", t.ID)

	c := in.Context
	var ctx []string
	if c.Parent != nil {
		ctx = append(ctx, fmt.Sprintf("父任务：%s（%s）", c.Parent.Title, c.Parent.Status))
	}
	for _, d := range c.DependsOn {
		ctx = append(ctx, fmt.Sprintf("依赖：%s（%s）", d.Title, d.Status))
	}
	for _, s := range c.Subtasks {
		ctx = append(ctx, fmt.Sprintf("子任务：%s（%s）", s.Title, s.Status))
	}
	for _, r := range c.PreviousResults {
		ctx = append(ctx, fmt.Sprintf("已有结果：[%s] %s（来源 %s，幂等键 %s）", r.Type, r.Summary, r.Agent, r.Key))
	}
	if c.User != "" {
		ctx = append(ctx, "补充说明："+c.User)
	}
	if len(ctx) > 0 {
		b.WriteString("\n# 上下文\n- " + strings.Join(ctx, "\n- ") + "\n")
	}
	b.WriteString("\n# 约束\n")
	for _, s := range in.Constraints {
		b.WriteString("- " + s + "\n")
	}
	if in.Workdir != "" {
		fmt.Fprintf(&b, "- 工作目录：%s\n", in.Workdir)
	}
	b.WriteString("\n# 输出格式\n" + in.OutputFormat + "\n")
	return b.String()
}
