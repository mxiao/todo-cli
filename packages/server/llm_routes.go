package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/llm"
	"github.com/mxiao/todo-cli/packages/prompt"
)

// Model and prompt endpoints. Keys are never accepted or returned here:
// they come from the environment or the keychain (`todo llm set-key`).
func (s *Server) llmRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/llm/status", s.withLLM(s.handleLLMStatus))
	m.HandleFunc("POST /api/llm/test", s.withLLM(s.handleLLMTest))
	m.HandleFunc("POST /api/llm/use", s.withLLM(s.handleLLMUse))
	m.HandleFunc("POST /api/llm/mode", s.withLLM(s.handleLLMMode))
	m.HandleFunc("PUT /api/llm/confirm-list", s.withLLM(s.handleLLMConfirmList))
	m.HandleFunc("GET /api/llm/calls", s.withLLM(s.handleLLMCalls))
	m.HandleFunc("GET /api/llm/actions", s.withLLM(s.handleLLMActions))
	m.HandleFunc("GET /api/llm/sessions", s.withLLM(s.handleLLMSessions))
	m.HandleFunc("GET /api/llm/sessions/{sid}", s.withLLM(s.handleLLMSession))
	m.HandleFunc("POST /api/llm/intake", s.withLLM(s.handleLLMIntake))
	m.HandleFunc("POST /api/llm/decide", s.withLLM(s.handleLLMDecide))
	m.HandleFunc("POST /api/llm/optimize", s.withLLM(s.handleLLMOptimize))
	m.HandleFunc("POST /api/llm/sessions/{sid}/answer", s.withLLM(s.handleLLMAnswer))
	m.HandleFunc("POST /api/llm/sessions/{sid}/redecide", s.withLLM(s.handleLLMRedecide))
	m.HandleFunc("POST /api/llm/sessions/{sid}/apply", s.withLLM(s.handleLLMItems("apply")))
	m.HandleFunc("POST /api/llm/sessions/{sid}/reject", s.withLLM(s.handleLLMItems("reject")))
	m.HandleFunc("POST /api/llm/sessions/{sid}/undo", s.withLLM(s.handleLLMItems("undo")))
	m.HandleFunc("PATCH /api/llm/sessions/{sid}/items/{n}", s.withLLM(s.handleLLMEditItem))
	m.HandleFunc("GET /api/llm/config", s.withLLM(s.handleAssistConfig))
	m.HandleFunc("GET /api/llm/config/versions", s.withLLM(s.handleAssistVersions))
	m.HandleFunc("POST /api/llm/config/rollback", s.withLLM(s.handleAssistRollback))
	m.HandleFunc("POST /api/llm/config/versions/{v}/restore", s.withLLM(s.handleAssistRestore))

	m.HandleFunc("GET /api/prompts", s.withLLM(s.handlePromptList))
	m.HandleFunc("POST /api/prompts", s.withLLM(s.handlePromptCreate))
	m.HandleFunc("POST /api/prompts/summarize", s.withLLM(s.handlePromptSummarize))
	m.HandleFunc("GET /api/prompts/{pid}", s.withLLM(s.handlePromptGet))
	m.HandleFunc("PUT /api/prompts/{pid}", s.withLLM(s.handlePromptEdit))
	m.HandleFunc("DELETE /api/prompts/{pid}", s.withLLM(s.handlePromptDelete))
	m.HandleFunc("GET /api/prompts/{pid}/versions", s.withLLM(s.handlePromptVersions))
	m.HandleFunc("POST /api/prompts/{pid}/rollback", s.withLLM(s.handlePromptRollback))
	m.HandleFunc("POST /api/prompts/{pid}/copy", s.withLLM(s.handlePromptCopy))
	m.HandleFunc("GET /api/prompts/{pid}/export", s.withLLM(s.handlePromptExport))
	m.HandleFunc("POST /api/prompts/{pid}/render", s.withLLM(s.handlePromptRender))
	m.HandleFunc("POST /api/prompts/{pid}/task", s.withLLM(s.handlePromptTask))
}

// withLLM answers 503 when the model module could not be opened; task
// endpoints are unaffected (FR-606).
func (s *Server) withLLM(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.llm == nil || s.prompts == nil {
			writeError(w, http.StatusServiceUnavailable, "llm_unavailable", "model features are not available: "+s.llmErr)
			return
		}
		h(w, r)
	}
}

// failLLM maps model, session and prompt errors; others go to fail.
func (s *Server) failLLM(w http.ResponseWriter, r *http.Request, err error) {
	if e, ok := llm.AsError(err); ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": e.Kind,
			"message": s.llm.Redact(e.Message), "hint": e.Hint, "retryable": e.Retryable, "profiles": e.Profiles}})
		return
	}
	var me *prompt.MissingVariablesError
	switch {
	case errors.As(err, &me):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "missing_variables",
			"message": err.Error(), "variables": me.Names}})
	case errors.Is(err, llm.ErrState):
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, llm.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, llm.ErrInvalid), errors.Is(err, prompt.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) handleLLMStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.llm.Status())
}

func (s *Server) handleLLMTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile string `json:"profile"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.llm.TestConnection(r.Context(), in.Profile)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request, fn func(*llm.Settings) error) {
	if _, err := s.llm.UpdateSettings(fn); err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.llm.Status())
}

func (s *Server) handleLLMUse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile string `json:"profile"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.updateSettings(w, r, func(st *llm.Settings) error {
		if _, err := st.Profile(in.Profile); err != nil {
			return err
		}
		st.Active = in.Profile
		return nil
	})
}

func (s *Server) handleLLMMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := llm.ParseMode(in.Mode)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	s.updateSettings(w, r, func(st *llm.Settings) error { st.Mode = m; return nil })
}

func (s *Server) handleLLMConfirmList(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Entries []string `json:"entries"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	entries := []string{}
	for _, e := range in.Entries {
		v, err := llm.ValidateConfirmEntry(e)
		if err != nil {
			s.failLLM(w, r, err)
			return
		}
		entries = append(entries, v)
	}
	s.updateSettings(w, r, func(st *llm.Settings) error { st.ConfirmList = entries; return nil })
}

func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func (s *Server) handleLLMCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := s.llm.Calls(limitParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": calls})
}

func (s *Server) handleLLMActions(w http.ResponseWriter, r *http.Request) {
	acts, err := s.llm.Actions(limitParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": acts})
}

func (s *Server) handleLLMSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.llm.Sessions(r.URL.Query().Get("kind"), limitParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) handleLLMSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.llm.Session(r.PathValue("sid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// sessionResult answers with the session and wakes the event stream, since
// model sessions may have changed tasks.
func (s *Server) sessionResult(w http.ResponseWriter, r *http.Request, sess *llm.Session, err error) {
	if sess == nil {
		s.failLLM(w, r, err)
		return
	}
	body := map[string]any{"session": sess, "revision": s.revision()}
	if err != nil {
		body["error"] = map[string]string{"code": "partial_failure", "message": err.Error()}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) resolveIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		full, err := s.store.ResolveID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

func parseMode(v string) (llm.Mode, error) {
	if v == "" {
		return "", nil
	}
	return llm.ParseMode(v)
}

func (s *Server) handleLLMIntake(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string   `json:"text"`
		Selected []string `json:"selected"`
		Mode     string   `json:"mode"`
		Profile  string   `json:"profile"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	mode, err := parseMode(in.Mode)
	if err == nil {
		in.Selected, err = s.resolveIDs(in.Selected)
	}
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	sess, err := s.llm.Intake(r.Context(), llm.IntakeInput{Text: in.Text, Selected: in.Selected, Mode: mode, Profile: in.Profile})
	s.sessionResult(w, r, sess, err)
}

func (s *Server) handleLLMDecide(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Selected []string `json:"selected"`
		Mode     string   `json:"mode"`
		Profile  string   `json:"profile"`
		Feedback string   `json:"feedback"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	mode, err := parseMode(in.Mode)
	if err == nil {
		in.Selected, err = s.resolveIDs(in.Selected)
	}
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	sess, err := s.llm.Decide(r.Context(), llm.DecideInput{Selected: in.Selected, Mode: mode, Profile: in.Profile, Feedback: in.Feedback})
	s.sessionResult(w, r, sess, err)
}

func (s *Server) handleLLMOptimize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile string `json:"profile"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	sess, err := s.llm.Optimize(r.Context(), in.Profile)
	s.sessionResult(w, r, sess, err)
}

func (s *Server) handleLLMAnswer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Answers string `json:"answers"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	sess, err := s.llm.Answer(r.Context(), r.PathValue("sid"), in.Answers)
	s.sessionResult(w, r, sess, err)
}

func (s *Server) handleLLMRedecide(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Feedback string `json:"feedback"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	sess, err := s.llm.Redecide(r.Context(), r.PathValue("sid"), in.Feedback)
	s.sessionResult(w, r, sess, err)
}

// handleLLMItems accepts, rejects or undoes session items ({"items": [1, 2]};
// no items = all pending / applied ones).
func (s *Server) handleLLMItems(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Items []int `json:"items"`
		}
		if _, err := decodeBody(w, r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		id := r.PathValue("sid")
		var sess *llm.Session
		var err error
		switch action {
		case "apply":
			sess, err = s.llm.Apply(r.Context(), id, in.Items)
		case "reject":
			sess, err = s.llm.Reject(id, in.Items)
		default:
			sess, err = s.llm.Undo(id, in.Items)
		}
		s.sessionResult(w, r, sess, err)
	}
}

func (s *Server) handleLLMEditItem(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "item must be a number")
		return
	}
	var e llm.ItemEdit
	if _, err := decodeBody(w, r, &e); err != nil {
		s.fail(w, r, err)
		return
	}
	sess, err := s.llm.EditItem(r.PathValue("sid"), n, e)
	s.sessionResult(w, r, sess, err)
}

func (s *Server) handleAssistConfig(w http.ResponseWriter, r *http.Request) {
	cfg, v, err := s.llm.AssistConfig()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v, "config": cfg})
}

func (s *Server) handleAssistVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.llm.AssistVersions()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (s *Server) handleAssistRollback(w http.ResponseWriter, r *http.Request) {
	cv, err := s.llm.RollbackAssist()
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cv)
}

func (s *Server) handleAssistRestore(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(strings.TrimPrefix(r.PathValue("v"), "v"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "version must be a number")
		return
	}
	cv, err := s.llm.RestoreAssistVersion(v)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cv)
}

// ---- prompts ----

func (s *Server) handlePromptList(w http.ResponseWriter, r *http.Request) {
	list, err := s.prompts.List()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list})
}

// handlePromptSummarize structures a prompt ({"text", "save", "name",
// "local"}); without a model it falls back to local rules (degraded).
func (s *Server) handlePromptSummarize(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text    string `json:"text"`
		Save    bool   `json:"save"`
		Name    string `json:"name"`
		Local   bool   `json:"local"`
		Profile string `json:"profile"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	var res *prompt.Summarized
	if in.Local {
		sum, err := prompt.LocalSummary(in.Text)
		if err != nil {
			s.failLLM(w, r, err)
			return
		}
		res = &prompt.Summarized{Summary: sum}
	} else {
		cfg, _, err := s.llm.AssistConfig()
		if err == nil {
			res, err = prompt.SummarizeWithFallback(r.Context(), s.llm, in.Profile, in.Text, cfg.Prompts.Summarize, llm.ErrUnavailable)
		}
		if err != nil {
			s.failLLM(w, r, err)
			return
		}
	}
	body := map[string]any{"summary": res.Summary, "body": res.Summary.Body(), "degraded": res.Degraded, "degraded_reason": res.Reason}
	if in.Save {
		source := prompt.SourceModel
		if in.Local || res.Degraded {
			source = prompt.SourceManual
		}
		t, err := s.prompts.Create(in.Name, in.Text, *res.Summary, source)
		if err != nil {
			s.failLLM(w, r, err)
			return
		}
		body["template"] = t
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string         `json:"name"`
		Original string         `json:"original"`
		Summary  prompt.Summary `json:"summary"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.prompts.Create(in.Name, in.Original, in.Summary, prompt.SourceManual)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.prompts.Get(r.PathValue("pid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handlePromptEdit stores a new version: {"summary": {...}, "body": "..."}.
func (s *Server) handlePromptEdit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Summary *prompt.Summary `json:"summary"`
		Body    *string         `json:"body"`
		Name    string          `json:"name"`
		Note    string          `json:"note"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.prompts.Get(r.PathValue("pid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	if in.Name != "" && in.Name != t.Name {
		if t, err = s.prompts.Rename(t.ID, in.Name); err != nil {
			s.failLLM(w, r, err)
			return
		}
	}
	if in.Summary != nil || in.Body != nil {
		sum := t.Latest.Summary
		if in.Summary != nil {
			sum = *in.Summary
		}
		if t, err = s.prompts.Edit(t.ID, sum, in.Body, prompt.SourceManual, in.Note); err != nil {
			s.failLLM(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	t, err := s.prompts.Get(r.PathValue("pid"))
	if err == nil {
		err = s.prompts.Delete(t.ID)
	}
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": t.ID})
}

func (s *Server) handlePromptVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.prompts.Versions(r.PathValue("pid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (s *Server) handlePromptRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version int `json:"version"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.prompts.Rollback(r.PathValue("pid"), in.Version)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handlePromptCopy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.prompts.Copy(r.PathValue("pid"), in.Version, in.Name)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handlePromptExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, _ := strconv.Atoi(q.Get("version"))
	format := q.Get("format")
	b, err := s.prompts.Export(r.PathValue("pid"), v, format)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	ct := "text/markdown; charset=utf-8"
	switch format {
	case prompt.FormatJSON:
		ct = "application/json; charset=utf-8"
	case prompt.FormatText, "text":
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

type renderInput struct {
	Values  map[string]string `json:"values"`
	Version int               `json:"version"`
	Title   string            `json:"title"`
}

func (s *Server) render(w http.ResponseWriter, r *http.Request) (*prompt.Template, *prompt.Version, string, *renderInput, bool) {
	var in renderInput
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return nil, nil, "", nil, false
	}
	t, err := s.prompts.Get(r.PathValue("pid"))
	if err != nil {
		s.failLLM(w, r, err)
		return nil, nil, "", nil, false
	}
	ver := t.Latest
	if in.Version != 0 {
		if ver, err = s.prompts.Version(t.ID, in.Version); err != nil {
			s.failLLM(w, r, err)
			return nil, nil, "", nil, false
		}
	}
	text, err := prompt.Render(ver.Body, ver.Summary.Variables, in.Values)
	if err != nil {
		s.failLLM(w, r, err)
		return nil, nil, "", nil, false
	}
	return t, ver, text, &in, true
}

func (s *Server) handlePromptRender(w http.ResponseWriter, r *http.Request) {
	if _, _, text, _, ok := s.render(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"prompt": text})
	}
}

// handlePromptTask creates an agent task whose description is the final
// prompt, so it can be reviewed before an agent runs it.
func (s *Server) handlePromptTask(w http.ResponseWriter, r *http.Request) {
	t, ver, text, in, ok := s.render(w, r)
	if !ok {
		return
	}
	task, err := s.store.Create(prompt.AgentTask(t, ver, text, in.Title))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	setETag(w, task)
	writeJSON(w, http.StatusCreated, map[string]any{"task": task, "prompt": text, "revision": s.revision()})
}

// openLLM prepares the model and prompt modules for New.
func (s *Server) openLLM() {
	svc := s.opts.LLM
	var err error
	if svc == nil {
		if svc, err = llm.Open(s.store, llm.Options{Origin: "web"}); err != nil {
			s.llmErr = err.Error()
			s.log.Error("open model features; task endpoints still work", "err", err)
			return
		}
	}
	lib, err := prompt.Open(s.store)
	if err != nil {
		s.llmErr = err.Error()
		s.log.Error("open prompt templates", "err", err)
		return
	}
	lib.Redact = svc.Redact
	s.llm, s.prompts = svc, lib
}
