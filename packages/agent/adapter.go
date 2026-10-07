package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mxiao/todo-cli/packages/llm"
)

// Deps are the services adapters may use.
type Deps struct {
	LLM    *llm.Service
	Getenv func(string) string
}

// Job is one attempt as an adapter sees it.
type Job struct {
	RunID   string
	TaskID  string
	Attempt int
	Agent   string
	Spec    Spec
	// Input is the JSON input document (redacted); Prompt is the final
	// prompt, also contained in Input.
	Input  []byte
	Prompt string
	// Dir is the working directory; Env the complete environment of
	// processes (only whitelisted variables).
	Dir       string
	Env       []string
	KillGrace time.Duration
}

// Sink receives what an adapter observes. It is safe for concurrent use;
// everything passed to it is redacted before it is stored.
type Sink interface {
	// Emit appends an event to the run log.
	Emit(kind, stream, message string, data map[string]any)
	// Message handles one protocol message ({"type": "result", …}).
	Message(msg map[string]any)
	// Secret registers a value (e.g. a token read for a header) that must
	// be hidden from everything stored.
	Secret(value string)
}

// Exit is how an attempt ended from the adapter's point of view.
type Exit struct {
	// Code is the exit code; the http adapter reports 0 for 2xx and the
	// HTTP status otherwise.
	Code int
	// Err means the agent could not be run at all.
	Err     error
	ErrKind string
}

// Adapter executes one attempt. It must return when ctx is cancelled,
// stopping whatever it started.
type Adapter interface {
	Run(ctx context.Context, job *Job, sink Sink) Exit
}

// Pauser is implemented by adapters that can suspend a running attempt.
type Pauser interface {
	Pause() error
	Resume() error
}

const (
	maxLine       = 64 << 10
	maxBuffered   = 4 << 20
	maxEventBytes = 8 << 10
)

// Parser turns agent output into log events and protocol messages
// according to the output mode. Adapters feed it lines and call Finish
// once the output has ended.
type Parser struct {
	mode      string
	sink      Sink
	buf       bytes.Buffer
	truncated bool
}

// NewParser returns a parser for an output mode (jsonl, json, text).
func NewParser(mode string, sink Sink) *Parser { return &Parser{mode: mode, sink: sink} }

// Line handles one line of the agent's standard output.
func (p *Parser) Line(stream, line string) {
	line = strings.TrimRight(line, "\r")
	if p.mode != OutputJSONL {
		if p.buf.Len()+len(line) < maxBuffered {
			p.buf.WriteString(line)
			p.buf.WriteByte('\n')
		} else {
			p.truncated = true
		}
	}
	if p.mode == OutputJSONL {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "{") {
			var msg map[string]any
			if json.Unmarshal([]byte(t), &msg) == nil {
				if _, ok := msg["type"].(string); ok {
					p.sink.Message(msg)
					return
				}
			}
		}
	}
	if line != "" || p.mode == OutputText {
		p.sink.Emit(EventOutput, stream, line, nil)
	}
}

// Finish interprets the buffered output of the json and text modes.
func (p *Parser) Finish() {
	if p.truncated {
		p.sink.Emit(EventError, "", "输出超过 4MB，超出部分未解析", nil)
	}
	out := strings.TrimSpace(p.buf.String())
	switch p.mode {
	case OutputText:
		if out != "" {
			p.sink.Message(map[string]any{"type": "result", "result_type": ResultText, "text": out})
		}
	case OutputJSON:
		if out == "" {
			return
		}
		var doc any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			p.sink.Emit(EventError, "", "输出不是有效的 JSON："+err.Error(), nil)
			return
		}
		p.Document(doc)
	}
}

// Document handles a JSON document: one message, a list of messages, or
// {"status", "message", "events": [...], "results": [...]}.
func (p *Parser) Document(doc any) {
	switch v := doc.(type) {
	case []any:
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				p.Document(m)
			}
		}
	case map[string]any:
		if _, ok := v["type"].(string); ok {
			p.sink.Message(v)
			return
		}
		if evs, ok := v["events"].([]any); ok {
			p.Document(evs)
		}
		if rs, ok := v["results"].([]any); ok {
			for _, x := range rs {
				if m, ok := x.(map[string]any); ok {
					r := map[string]any{"type": "result"}
					for k, val := range m {
						if k == "type" {
							k = "result_type"
						}
						r[k] = val
					}
					p.sink.Message(r)
				}
			}
		}
		if st, ok := v["status"].(string); ok {
			msg, _ := v["message"].(string)
			p.sink.Message(map[string]any{"type": "status", "status": st, "message": msg})
		}
	}
}

// lineWriter splits a byte stream into lines.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	for len(w.buf) > maxLine {
		cut := maxLine
		for cut > 0 && !utf8.RuneStart(w.buf[cut]) {
			cut--
		}
		w.fn(string(w.buf[:cut]))
		w.buf = w.buf[cut:]
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn(string(w.buf))
		w.buf = nil
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated]"
}
