package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/agent"
)

// Agent endpoints (FR-501…FR-511). Runs started here execute in the
// background of this server; any entry point can control them.
func (s *Server) agentRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/agents", s.withAgents(s.handleAgents))
	m.HandleFunc("PUT /api/agents/{name}", s.withAgents(s.handlePutAgent))
	m.HandleFunc("DELETE /api/agents/{name}", s.withAgents(s.handleDeleteAgent))
	m.HandleFunc("POST /api/tasks/{id}/agent-runs", s.withAgents(s.handleStartRun))
	m.HandleFunc("GET /api/tasks/{id}/results", s.withAgents(s.handleTaskResults))
	m.HandleFunc("POST /api/tasks/{id}/results", s.withAgents(s.handleWriteback))
	m.HandleFunc("GET /api/agent-runs", s.withAgents(s.handleRuns))
	m.HandleFunc("GET /api/agent-runs/{rid}", s.withAgents(s.handleRun))
	m.HandleFunc("GET /api/agent-runs/{rid}/events", s.withAgents(s.handleRunEvents))
	m.HandleFunc("GET /api/agent-runs/{rid}/prompt", s.withAgents(s.handleRunPrompt))
	for _, verb := range []string{"pause", "resume", "cancel", "retry", "confirm", "reject"} {
		m.HandleFunc("POST /api/agent-runs/{rid}/"+verb, s.withAgents(s.handleRunControl(verb)))
	}
}

// openAgents prepares the agent manager once the model service is open.
func (s *Server) openAgents() {
	if s.llm == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := agent.Open(s.store, agent.Options{LLM: s.llm, Prompts: s.prompts, Origin: "web", Async: true, Context: ctx})
	if err != nil {
		cancel()
		s.llmErr = err.Error()
		s.log.Error("open agent runs; task endpoints still work", "err", err)
		return
	}
	s.agents, s.stopAgents = m, cancel
	s.llm.SetAgentLauncher(m)
}

func (s *Server) withAgents(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.agents == nil {
			writeError(w, http.StatusServiceUnavailable, "agents_unavailable", "agent runs are not available: "+s.llmErr)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	list, err := s.agents.Agents()
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list, "adapters": agent.Adapters()})
}

func (s *Server) handlePutAgent(w http.ResponseWriter, r *http.Request) {
	var sp agent.Spec
	if _, err := decodeBody(w, r, &sp); err != nil {
		s.fail(w, r, err)
		return
	}
	sp.Name = r.PathValue("name")
	sp, err := s.agents.PutAgent(sp)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": sp})
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.agents.RemoveAgent(r.PathValue("name")); err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("name")})
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent             string            `json:"agent"`
		Context           string            `json:"context"`
		Constraints       []string          `json:"constraints"`
		OutputFormat      string            `json:"output_format"`
		Template          string            `json:"template"`
		Vars              map[string]string `json:"vars"`
		CompleteOnSuccess bool              `json:"complete_on_success"`
		MaxRetries        *int              `json:"max_retries"`
		DryRun            bool              `json:"dry_run"`
	}
	if _, err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := s.store.ResolveID(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	run, err := s.agents.Start(r.Context(), agent.StartRequest{TaskID: id, Agent: in.Agent, Context: in.Context, Constraints: in.Constraints,
		OutputFormat: in.OutputFormat, Template: in.Template, Vars: in.Vars, CompleteOnSuccess: in.CompleteOnSuccess,
		MaxRetries: in.MaxRetries, Initiator: "web", DryRun: in.DryRun})
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	if in.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{"run": run})
		return
	}
	s.agents.Background(run.ID)
	s.hub.kickNow()
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := agent.ListFilter{}
	if t := q.Get("task"); t != "" {
		id, err := s.store.ResolveID(t)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		f.TaskID = id
	}
	if st := q.Get("status"); st != "" {
		f.Statuses = strings.Split(st, ",")
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	runs, err := s.agents.List(f)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.agents.Get(r.PathValue("rid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// handleRunEvents returns the log after an event id, for live views that
// poll (?after=<last id>).
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := agent.EventFilter{}
	f.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	f.Attempt, _ = strconv.Atoi(q.Get("attempt"))
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if k := q.Get("kind"); k != "" {
		f.Kinds = strings.Split(k, ",")
	}
	evs, err := s.agents.Events(r.PathValue("rid"), f)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	run, err := s.agents.Get(r.PathValue("rid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "status": run.Status, "done": agent.Terminal(run.Status)})
}

func (s *Server) handleRunPrompt(w http.ResponseWriter, r *http.Request) {
	run, err := s.agents.Get(r.PathValue("rid"))
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "agent": run.Agent, "prompt": run.Prompt, "input": run.Input})
}

func (s *Server) handleRunControl(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Context string `json:"context"`
		}
		if _, err := decodeBody(w, r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
		id := r.PathValue("rid")
		var run *agent.Run
		var err error
		switch verb {
		case "pause":
			run, err = s.agents.Pause(id)
		case "resume":
			run, err = s.agents.Resume(id)
		case "cancel":
			run, err = s.agents.Cancel(id)
		case "retry":
			run, err = s.agents.Retry(id, agent.RetryRequest{Context: in.Context})
		case "confirm":
			run, err = s.agents.Confirm(id)
		case "reject":
			run, err = s.agents.Reject(id)
		}
		if err != nil {
			s.failLLM(w, r, err)
			return
		}
		if run.Status == agent.StatusQueued {
			s.agents.Background(run.ID)
		}
		s.hub.kickNow()
		writeJSON(w, http.StatusOK, map[string]any{"run": run})
	}
}

func (s *Server) handleTaskResults(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.ResolveID(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rs, err := s.agents.TaskResults(id)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": rs})
}

// handleWriteback stores a result written back by an external system or
// the user: {"result_type": …, "run_id"?, "source"?, …}. It is idempotent:
// the same content again answers 200 with the stored result.
func (s *Server) handleWriteback(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		s.fail(w, r, errBadJSON)
		return
	}
	id, err := s.store.ResolveID(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in := agent.ResultInput{TaskID: id, Agent: "web", Raw: raw}
	if v, ok := raw["run_id"].(string); ok && v != "" {
		if in.RunID, err = s.agents.ResolveRun(v); err != nil {
			s.failLLM(w, r, err)
			return
		}
	}
	if v, ok := raw["source"].(string); ok && strings.TrimSpace(v) != "" {
		in.Agent = strings.TrimSpace(v)
	}
	res, err := s.agents.SaveResult(in)
	if err != nil {
		s.failLLM(w, r, err)
		return
	}
	s.hub.kickNow()
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"result": res})
}
