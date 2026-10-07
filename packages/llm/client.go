package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one chat completion call.
type Request struct {
	// Purpose labels the call in the audit log (intake, decide, …).
	Purpose  string
	Messages []Message
	// JSON asks for a JSON object response.
	JSON      bool
	MaxTokens int
}

// Response is the model's answer.
type Response struct {
	Content string `json:"content"`
	Model   string `json:"model"`
}

// Client is an OpenAI-compatible chat model.
type Client interface {
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Error kinds; they map to user-facing messages and decide retries.
const (
	KindNotConfigured = "llm_not_configured"
	KindAuth          = "llm_auth_failed"
	KindNotFound      = "llm_not_found"
	KindRateLimited   = "llm_rate_limited"
	KindServer        = "llm_server_error"
	KindTimeout       = "llm_timeout"
	KindUnreachable   = "llm_unreachable"
	KindBadRequest    = "llm_bad_request"
	KindBadResponse   = "llm_bad_response"
)

// ErrUnavailable matches every model error with errors.Is: model features
// are unavailable, task management is not affected (FR-606).
var ErrUnavailable = errors.New("llm_unavailable")

// Error is a classified, user-understandable model failure (FR-602).
type Error struct {
	Kind    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Status  int    `json:"http_status,omitempty"`
	// Retryable means retrying the same profile may succeed.
	Retryable bool `json:"retryable"`
	// Profiles lists other profiles to switch to.
	Profiles []string `json:"profiles,omitempty"`
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return e.Message + "；" + e.Hint
	}
	return e.Message
}

func (e *Error) Unwrap() error { return ErrUnavailable }

// AsError extracts a model error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

const switchHint = "可重试，或用 `todo llm use <配置名>` 切换模型配置"

// NotConfigured builds the error for a profile that cannot be used.
func NotConfigured(r *Resolved) *Error {
	msg := "大模型未配置"
	if r != nil && r.Problem != "" {
		msg += "：" + r.Problem
	}
	return &Error{Kind: KindNotConfigured, Message: msg,
		Hint: "普通任务管理不受影响；配置后运行 `todo llm test` 检查连接"}
}

// OpenAI calls {BaseURL}/chat/completions.
type OpenAI struct {
	BaseURL    string
	Model      string
	APIKey     string
	HTTP       *http.Client
	MaxRetries int
	// Backoff is the wait before the first retry; it doubles each time.
	Backoff time.Duration
}

// NewClient builds the client for a resolved profile.
func NewClient(r *Resolved) (Client, error) {
	if !r.Configured() {
		return nil, NotConfigured(r)
	}
	timeout := 60 * time.Second
	if r.TimeoutSeconds > 0 {
		timeout = time.Duration(r.TimeoutSeconds) * time.Second
	}
	retries := 1
	if r.MaxRetries != nil {
		retries = max(*r.MaxRetries, 0)
	}
	return &OpenAI{BaseURL: r.BaseURL, Model: r.Model, APIKey: r.APIKey, HTTP: &http.Client{Timeout: timeout},
		MaxRetries: retries, Backoff: 500 * time.Millisecond}, nil
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []Message         `json:"messages"`
	Temperature    float64           `json:"temperature"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends the request, retrying transient failures.
func (c *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	body := chatRequest{Model: c.Model, Messages: req.Messages, Temperature: 0.2, MaxTokens: req.MaxTokens}
	if req.JSON {
		body.ResponseFormat = map[string]string{"type": "json_object"}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	wait := c.Backoff
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, payload)
		e, ok := AsError(err)
		if err == nil || !ok || !e.Retryable || attempt >= c.MaxRetries {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (c *OpenAI) once(ctx context.Context, payload []byte) (*Response, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Kind: KindBadRequest, Message: "接口地址无效：" + RedactURL(c.BaseURL)}
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(hr)
	if err != nil {
		return nil, c.transportError(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, c.transportError(err)
	}
	if res.StatusCode/100 != 2 {
		return nil, c.statusError(res.StatusCode, raw)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return nil, &Error{Kind: KindBadResponse, Message: "模型服务返回了无法解析的响应（不是 OpenAI 兼容格式）",
			Hint: "检查接口地址是否以 /v1 结尾；" + switchHint, Retryable: true}
	}
	return &Response{Content: cr.Choices[0].Message.Content, Model: cr.Model}, nil
}

func (c *OpenAI) transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindTimeout, Message: "连接模型服务超时", Hint: switchHint, Retryable: true}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: KindTimeout, Message: "模型请求已取消"}
	}
	return &Error{Kind: KindUnreachable, Message: "无法连接模型服务 " + RedactURL(c.BaseURL) + "：" + Redact(rootCause(err), c.APIKey),
		Hint: "检查网络和接口地址；" + switchHint, Retryable: true}
}

func rootCause(err error) string {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err.Error()
		}
		err = u
	}
}

func (c *OpenAI) statusError(status int, raw []byte) error {
	var cr chatResponse
	detail := ""
	if json.Unmarshal(raw, &cr) == nil && cr.Error != nil {
		detail = cr.Error.Message
	}
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	detail = Redact(detail, c.APIKey)
	e := &Error{Status: status}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind, e.Message, e.Hint = KindAuth, fmt.Sprintf("模型服务拒绝了访问凭据（HTTP %d）", status),
			"检查 API Key 是否正确、是否过期（todo llm set-key 或环境变量 "+EnvAPIKey+"）"
	case status == http.StatusNotFound:
		e.Kind, e.Message, e.Hint = KindNotFound, fmt.Sprintf("模型或接口不存在（HTTP 404）：模型 %q @ %s", c.Model, RedactURL(c.BaseURL)),
			"检查模型名称和接口地址；"+switchHint
	case status == http.StatusTooManyRequests:
		e.Kind, e.Message, e.Hint, e.Retryable = KindRateLimited, "模型服务限流或额度不足（HTTP 429）", "稍后重试，或切换模型配置", true
	case status >= 500:
		e.Kind, e.Message, e.Hint, e.Retryable = KindServer, fmt.Sprintf("模型服务暂时不可用（HTTP %d）", status), switchHint, true
	default:
		e.Kind, e.Message, e.Hint = KindBadRequest, fmt.Sprintf("模型服务拒绝了请求（HTTP %d）", status), "检查模型名称是否支持该接口；"+switchHint
	}
	if detail != "" {
		e.Message += "：" + detail
	}
	return e
}

// Func adapts a function to Client (mock model responses in tests).
type Func func(ctx context.Context, req Request) (*Response, error)

func (f Func) Complete(ctx context.Context, req Request) (*Response, error) { return f(ctx, req) }

// Scripted is a mock model that answers with queued responses and records
// every request. A queued error is returned instead of a response.
type Scripted struct {
	mu        sync.Mutex
	responses []any
	Requests  []Request
}

// NewScripted queues responses: strings (content), values (marshalled to
// JSON), errors, or func(Request) any computing one of those from the
// request.
func NewScripted(responses ...any) *Scripted { return &Scripted{responses: responses} }

// Push queues more responses.
func (s *Scripted) Push(responses ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, responses...)
}

func (s *Scripted) Complete(_ context.Context, req Request) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, req)
	if len(s.responses) == 0 {
		return nil, &Error{Kind: KindBadResponse, Message: "mock model: no response queued"}
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	if fn, ok := next.(func(Request) any); ok {
		next = fn(req)
	}
	switch v := next.(type) {
	case error:
		return nil, v
	case string:
		return &Response{Content: v, Model: "mock"}, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return &Response{Content: string(b), Model: "mock"}, nil
	}
}

// LastRequest returns the most recent request.
func (s *Scripted) LastRequest() Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Requests) == 0 {
		return Request{}
	}
	return s.Requests[len(s.Requests)-1]
}

// decodeJSON parses a model's JSON answer, tolerating code fences and
// surrounding prose.
func decodeJSON(content string, v any) error {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return &Error{Kind: KindBadResponse, Message: "模型没有返回有效的 JSON：" + err.Error(), Hint: "可重试或切换模型", Retryable: true}
	}
	return nil
}
