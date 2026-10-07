package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/llm"
)

// httpAdapter connects applications and systems that have their own HTTP
// interface (FR-510). It sends the JSON input and reads either a JSON Lines
// stream (application/x-ndjson, application/jsonl) or one JSON document
// {status, message, events, results}. Header values come from environment
// variables named in header_env and are never stored.
type httpAdapter struct {
	spec   Spec
	getenv func(string) string
}

func newHTTPAdapter(spec Spec, deps Deps) (Adapter, error) {
	return &httpAdapter{spec: spec, getenv: deps.Getenv}, nil
}

func (a *httpAdapter) Run(ctx context.Context, job *Job, sink Sink) Exit {
	req, err := http.NewRequestWithContext(ctx, a.spec.Method, a.spec.URL, bytes.NewReader(job.Input))
	if err != nil {
		return Exit{Code: -1, Err: err, ErrKind: KindStartFailed}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson, application/json;q=0.9, text/plain;q=0.5")
	req.Header.Set("X-Todo-Agent-Protocol", Protocol)
	req.Header.Set("X-Todo-Run-Id", job.RunID)
	req.Header.Set("X-Todo-Attempt", strconv.Itoa(job.Attempt))
	// The same key on every attempt lets the system recognise a retry of
	// work it may already have done (NFR-018).
	req.Header.Set("Idempotency-Key", job.TaskID+":"+job.RunID)
	for h, env := range a.spec.HeaderEnv {
		v := a.getenv(env)
		if v == "" {
			return Exit{Code: -1, Err: fmt.Errorf("环境变量 %s 未设置（请求头 %s 需要它）", env, h), ErrKind: KindStartFailed}
		}
		sink.Secret(v)
		req.Header.Set(h, v)
	}
	sink.Emit(EventSystem, "", fmt.Sprintf("HTTP %s %s", a.spec.Method, llm.RedactURL(a.spec.URL)),
		map[string]any{"method": a.spec.Method, "url": llm.RedactURL(a.spec.URL)})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Exit{Code: -1}
		}
		return Exit{Code: -1, Err: fmt.Errorf("无法连接 %s：%w", llm.RedactURL(a.spec.URL), err), ErrKind: KindUnreachable}
	}
	defer resp.Body.Close()
	sink.Emit(EventSystem, "", "HTTP 状态 "+resp.Status, map[string]any{"status": resp.StatusCode})
	code := 0
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code = resp.StatusCode
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	parser := NewParser(a.spec.Output, sink)
	switch {
	case mt == "application/x-ndjson" || mt == "application/jsonl" || mt == "application/json-lines":
		parser = NewParser(OutputJSONL, sink)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			parser.Line("stdout", sc.Text())
		}
	case mt == "application/json" && a.spec.Output != OutputText:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBuffered))
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			sink.Emit(EventError, "", "响应不是有效的 JSON："+err.Error(), nil)
			break
		}
		if code != 0 {
			sink.Emit(EventError, "", "接口返回错误："+clip(strings.TrimSpace(string(body)), 500), nil)
		}
		parser.Document(doc)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBuffered))
		for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
			parser.Line("stdout", l)
		}
		parser.Finish()
	}
	if ctx.Err() != nil {
		return Exit{Code: -1}
	}
	return Exit{Code: code}
}

// llmAdapter is a prompt agent: the final prompt goes to a model profile
// and the answer is written back (as text by default).
type llmAdapter struct {
	spec Spec
	svc  *llm.Service
}

func newLLMAdapter(spec Spec, deps Deps) (Adapter, error) {
	return &llmAdapter{spec: spec, svc: deps.LLM}, nil
}

const promptAgentSystem = "你是执行任务的智能体。根据提示词完成任务并直接给出结果；" +
	"不要声称执行了你没有实际执行的操作；信息不足时说明缺少什么。"

func (a *llmAdapter) Run(ctx context.Context, job *Job, sink Sink) Exit {
	if a.svc == nil {
		return Exit{Code: -1, Err: fmt.Errorf("模型服务不可用"), ErrKind: KindAdapterUnavailable}
	}
	profile := a.spec.Profile
	sink.Emit(EventSystem, "", "调用模型（配置 "+orDefault(profile, "当前")+"）", map[string]any{"profile": profile})
	answer, err := a.svc.Ask(ctx, "agent", profile, promptAgentSystem, job.Prompt)
	if err != nil {
		if ctx.Err() != nil {
			return Exit{Code: -1}
		}
		kind := KindStartFailed
		if e, ok := llm.AsError(err); ok {
			kind = e.Kind
		}
		return Exit{Code: -1, Err: err, ErrKind: kind}
	}
	parser := NewParser(a.spec.Output, sink)
	for _, l := range strings.Split(answer, "\n") {
		parser.Line("stdout", l)
	}
	parser.Finish()
	return Exit{Code: 0}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
