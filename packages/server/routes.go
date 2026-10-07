package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mxiao/todo-cli/packages/core"
)

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("/", s.handleStatic)
	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/facets", s.handleFacets)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("GET /api/tasks", s.handleList)
	m.HandleFunc("POST /api/tasks", s.handleCreate)
	m.HandleFunc("GET /api/tasks/{id}", s.handleGet)
	m.HandleFunc("PATCH /api/tasks/{id}", s.handlePatch)
	m.HandleFunc("DELETE /api/tasks/{id}", s.handleDelete)
	m.HandleFunc("POST /api/tasks/{id}/status", s.handleStatus)
	m.HandleFunc("POST /api/tasks/{id}/restore", s.handleRestore)
	m.HandleFunc("POST /api/tasks/{id}/move", s.handleMove)
	m.HandleFunc("GET /api/tasks/{id}/history", s.handleHistory)
	m.HandleFunc("GET /api/tasks/{id}/versions", s.handleVersions)
	m.HandleFunc("GET /api/tasks/{id}/versions/{version}", s.handleVersion)
	m.HandleFunc("POST /api/tasks/{id}/versions/{version}/revert", s.handleRevert)
	m.HandleFunc("GET /api/tasks/{id}/conflicts", s.handleTaskConflicts)
	m.HandleFunc("GET /api/conflicts/{cid}", s.handleConflict)
	m.HandleFunc("POST /api/conflicts/{cid}/resolve", s.handleResolve)
	m.HandleFunc("POST /api/batch", s.handleBatch)
	m.HandleFunc("POST /api/undo", s.handleUndo)
	m.HandleFunc("/api/", s.handleUnknown)
}

// handleUnknown answers unmatched API requests with a JSON 404, or 405 when
// the path exists for other methods.
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	var allowed []string
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := s.mux.Handler(probe); pattern != "/api/" && pattern != "" {
			allowed = append(allowed, method)
		}
	}
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" is not supported on "+r.URL.Path)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "no such API endpoint: "+r.Method+" "+r.URL.Path)
}

// revision reports the store revision after a write and wakes the event
// watcher so subscribers hear about the write right away.
func (s *Server) revision() int64 {
	s.hub.kickNow()
	rev, err := s.store.Revision()
	if err != nil {
		s.log.Warn("read revision", "err", err)
	}
	return rev
}

func (s *Server) taskID(r *http.Request) (string, error) {
	return s.store.ResolveID(r.PathValue("id"))
}

// expectedVersion reads the optimistic-lock version from the body value or
// an If-Match header ("3" or "\"3\""); 0 means none was sent.
func expectedVersion(r *http.Request, body int64) (int64, error) {
	h := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/"), `"`)
	if h == "" {
		return body, nil
	}
	v, err := strconv.ParseInt(h, 10, 64)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("%w: If-Match must be a task version number", core.ErrInvalid)
	}
	if body != 0 && body != v {
		return 0, fmt.Errorf("%w: If-Match %d and body version %d disagree", core.ErrInvalid, v, body)
	}
	return v, nil
}

func setETag(w http.ResponseWriter, t *core.Task) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, t.Version))
}

// ---- read endpoints ----

// handleFallbackIndex is the landing page when no web UI assets are available.
func (s *Server) handleFallbackIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, indexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rev, err := s.store.Revision()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	schema, _ := s.store.SchemaVersion()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": s.opts.Version, "revision": rev, "schema_version": schema, "data_dir": s.store.DataDir(),
	})
}

// handleFacets lists the tags and categories in use, for filter menus.
func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.Facets()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	f, err := s.parseFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Read the revision first: a change racing with the list is then
	// reported again by the event stream rather than lost.
	rev, err := s.store.Revision()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tasks, err := s.store.List(f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "count": len(tasks), "revision": rev})
}

// parseFilter maps query parameters to a core.Filter:
//
//	q, status (csv or "all"), priority (csv), min_priority, tag (csv, repeatable; all must match),
//	category, parent (id or "none"), due_before, due_after, overdue, has_due,
//	include_archived, deleted (include|only), sort, reverse, limit
func (s *Server) parseFilter(q url.Values) (core.Filter, error) {
	var f core.Filter
	known := map[string]bool{"q": true, "status": true, "priority": true, "min_priority": true, "tag": true,
		"category": true, "parent": true, "due_before": true, "due_after": true, "overdue": true, "has_due": true,
		"include_archived": true, "deleted": true, "sort": true, "reverse": true, "limit": true}
	for k := range q {
		if !known[k] {
			return f, fmt.Errorf("%w: unknown query parameter %q", core.ErrInvalid, k)
		}
	}
	csv := func(key string) []string {
		var out []string
		for _, v := range q[key] {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
		}
		return out
	}
	boolean := func(key string) (bool, error) {
		switch strings.ToLower(q.Get(key)) {
		case "", "0", "false", "no":
			return false, nil
		case "1", "true", "yes":
			return true, nil
		}
		return false, fmt.Errorf("%w: %s must be true or false", core.ErrInvalid, key)
	}
	var err error
	f.Query = q.Get("q")
	for _, v := range csv("status") {
		if v == "all" {
			f.IncludeArchived = true
			continue
		}
		st, err := core.ParseStatus(v)
		if err != nil {
			return f, err
		}
		f.Statuses = append(f.Statuses, st)
	}
	for _, v := range csv("priority") {
		p, err := core.ParsePriority(v)
		if err != nil {
			return f, err
		}
		f.Priorities = append(f.Priorities, p)
	}
	if v := q.Get("min_priority"); v != "" {
		p, err := core.ParsePriority(v)
		if err != nil {
			return f, err
		}
		f.MinPriority = &p
	}
	f.Tags = csv("tag")
	f.Category = q.Get("category")
	if q.Has("parent") {
		parent := strings.TrimSpace(q.Get("parent"))
		if parent != "" && parent != "none" {
			if parent, err = s.store.ResolveID(parent); err != nil {
				return f, err
			}
		} else {
			parent = ""
		}
		f.ParentID = &parent
	}
	if v := q.Get("due_before"); v != "" {
		t, err := core.ParseTimeInput(v, true)
		if err != nil {
			return f, err
		}
		f.DueBefore = &t
	}
	if v := q.Get("due_after"); v != "" {
		t, err := core.ParseTimeInput(v, false)
		if err != nil {
			return f, err
		}
		f.DueAfter = &t
	}
	if f.Overdue, err = boolean("overdue"); err != nil {
		return f, err
	}
	if q.Has("has_due") {
		b, err := boolean("has_due")
		if err != nil {
			return f, err
		}
		f.HasDue = &b
	}
	if ia, err := boolean("include_archived"); err != nil {
		return f, err
	} else if ia {
		f.IncludeArchived = true
	}
	switch q.Get("deleted") {
	case "":
	case "include":
		f.IncludeDeleted = true
	case "only":
		f.OnlyDeleted = true
	default:
		return f, fmt.Errorf("%w: deleted must be include or only", core.ErrInvalid)
	}
	if f.Sort, err = core.ParseSortField(q.Get("sort")); err != nil {
		return f, err
	}
	if f.Reverse, err = boolean("reverse"); err != nil {
		return f, err
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 0 {
			return f, fmt.Errorf("%w: limit must be a non-negative integer", core.ErrInvalid)
		}
	}
	return f, nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.store.Get(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	subtasks, err := s.store.List(core.Filter{ParentID: &id, IncludeArchived: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hist, err := s.store.History(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	conflicts, err := s.store.Conflicts(id, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	setETag(w, t)
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "subtasks": subtasks, "history": hist, "conflicts": conflicts})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hist, err := s.store.History(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": id, "history": hist})
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vs, err := s.store.TaskVersions(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": id, "versions": vs})
}

func pathVersion(r *http.Request) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("%w: version must be a positive integer", core.ErrInvalid)
	}
	return v, nil
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := pathVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.store.GetVersion(id, v)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

// ---- writes ----

type createRequest struct {
	Title       string        `json:"title"`
	Description string        `json:"description"`
	DueAt       *string       `json:"due_at"`
	Priority    core.Priority `json:"priority"`
	Tags        []string      `json:"tags"`
	Category    string        `json:"category"`
	ParentID    string        `json:"parent_id"`
	Notes       string        `json:"notes"`
	Status      string        `json:"status"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	in := core.NewTask{Title: req.Title, Description: req.Description, Priority: req.Priority, Tags: req.Tags,
		Category: req.Category, Notes: req.Notes}
	if req.DueAt != nil && strings.TrimSpace(*req.DueAt) != "" {
		due, err := core.ParseTimeInput(*req.DueAt, true)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		in.DueAt = &due
	}
	if req.Status != "" {
		st, err := core.ParseStatus(req.Status)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		in.Status = st
	}
	if req.ParentID != "" {
		pid, err := s.store.ResolveID(req.ParentID)
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: parent: %v", core.ErrInvalid, err))
			return
		}
		in.ParentID = pid
	}
	t, err := s.store.Create(in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/tasks/"+t.ID)
	setETag(w, t)
	writeJSON(w, http.StatusCreated, map[string]any{"task": t, "revision": s.revision()})
}

// handlePatch edits a task. The version the client edited is mandatory
// (body "version" or If-Match): a stale version is rejected with 409 and the
// rejected edit is stored as a conflict so it can be applied later.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	raw, err := readBody(w, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := core.DecodePatch(raw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if p.ExpectedVersion, err = expectedVersion(r, p.ExpectedVersion); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.ExpectedVersion == 0 {
		s.fail(w, r, fmt.Errorf("%w: send the task version you edited (body \"version\" or If-Match) so concurrent changes are not overwritten", errVersionRequired))
		return
	}
	if p.ParentID != nil && *p.ParentID != "" {
		pid, err := s.store.ResolveID(*p.ParentID)
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: parent: %v", core.ErrInvalid, err))
			return
		}
		p.ParentID = &pid
	}
	t, err := s.store.Update(id, p)
	if err != nil {
		s.writeConflictOr(w, r, err, id, p.ExpectedVersion, raw)
		return
	}
	setETag(w, t)
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "revision": s.revision()})
}

type statusRequest struct {
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req statusRequest
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := core.ParseStatus(req.Status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	action, _ := core.StatusAction(st)
	ver, err := expectedVersion(r, req.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.store.ApplyBatch(core.BatchRequest{Action: action, IDs: []string{id}, Expect: map[string]int64{id: ver}})
	if err != nil {
		// A status change is an edit too: keep it as a recoverable patch.
		patch, _ := json.Marshal(map[string]any{"status": string(st)})
		s.writeConflictOr(w, r, err, id, ver, patch)
		return
	}
	setETag(w, &res.Tasks[0])
	writeJSON(w, http.StatusOK, map[string]any{"task": res.Tasks[0], "changed": res.Changed,
		"operation_id": res.OperationID, "revision": s.revision()})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.singleAction(w, r, core.BatchDelete)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	s.singleAction(w, r, core.BatchRestore)
}

// singleAction runs delete/restore on one task; the version is optional
// (body "version", ?version= or If-Match) and checked when given.
func (s *Server) singleAction(w http.ResponseWriter, r *http.Request, action string) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Version int64 `json:"version"`
	}
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if v := r.URL.Query().Get("version"); v != "" && req.Version == 0 {
		if req.Version, err = strconv.ParseInt(v, 10, 64); err != nil {
			s.fail(w, r, fmt.Errorf("%w: version must be an integer", core.ErrInvalid))
			return
		}
	}
	ver, err := expectedVersion(r, req.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.store.ApplyBatch(core.BatchRequest{Action: action, IDs: []string{id}, Expect: map[string]int64{id: ver}})
	if err != nil {
		s.writeConflictOr(w, r, err, id, ver, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": res.Tasks[0], "changed": res.Changed,
		"operation_id": res.OperationID, "revision": s.revision()})
}

type moveRequest struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Top     bool   `json:"top"`
	Bottom  bool   `json:"bottom"`
	Version int64  `json:"version"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req moveRequest
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	pl := core.Placement{Top: req.Top, Bottom: req.Bottom}
	for _, ref := range []struct {
		in  string
		out *string
	}{{req.Before, &pl.Before}, {req.After, &pl.After}} {
		if ref.in == "" {
			continue
		}
		if *ref.out, err = s.store.ResolveID(ref.in); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if pl.ExpectedVersion, err = expectedVersion(r, req.Version); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.store.Reorder(id, pl)
	if err != nil {
		s.writeConflictOr(w, r, err, id, pl.ExpectedVersion, nil)
		return
	}
	setETag(w, t)
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "revision": s.revision()})
}

func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := pathVersion(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req struct {
		Version int64 `json:"version"`
	}
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ver, err := expectedVersion(r, req.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ver == 0 {
		s.fail(w, r, fmt.Errorf("%w: send the current task version (body \"version\" or If-Match)", errVersionRequired))
		return
	}
	t, err := s.store.Revert(id, v, ver)
	if err != nil {
		s.writeConflictOr(w, r, err, id, ver, nil)
		return
	}
	setETag(w, t)
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "revision": s.revision()})
}

type batchItem struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type batchRequest struct {
	Action   string         `json:"action"`
	IDs      []string       `json:"ids"`
	Items    []batchItem    `json:"items"`
	Priority *core.Priority `json:"priority"`
	Category *string        `json:"category"`
	ParentID *string        `json:"parent_id"`
}

// handleBatch applies one action to many tasks atomically. Tasks are given
// as "ids" and/or "items" ([{id, version}]); versions are checked when sent.
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "done":
		action = core.BatchComplete
	case "rm":
		action = core.BatchDelete
	}
	b := core.BatchRequest{Action: action, Expect: map[string]int64{}}
	items := req.Items
	for _, id := range req.IDs {
		items = append(items, batchItem{ID: id})
	}
	if len(items) == 0 {
		s.fail(w, r, fmt.Errorf("%w: no tasks given (ids or items)", core.ErrInvalid))
		return
	}
	if len(items) > 10000 {
		s.fail(w, r, fmt.Errorf("%w: at most 10000 tasks per batch", core.ErrInvalid))
		return
	}
	for _, it := range items {
		id, err := s.store.ResolveID(it.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		b.IDs = append(b.IDs, id)
		if it.Version != 0 {
			b.Expect[id] = it.Version
		}
	}
	switch action {
	case core.BatchPriority:
		if req.Priority == nil {
			s.fail(w, r, fmt.Errorf("%w: priority is required for the priority action", core.ErrInvalid))
			return
		}
		b.Priority = *req.Priority
	case core.BatchMove:
		b.Move = core.MoveTarget{Category: req.Category, ParentID: req.ParentID}
		if p := req.ParentID; p != nil && *p != "" && *p != "none" {
			pid, err := s.store.ResolveID(*p)
			if err != nil {
				s.fail(w, r, fmt.Errorf("%w: parent: %v", core.ErrInvalid, err))
				return
			}
			b.Move.ParentID = &pid
		} else if p != nil {
			empty := ""
			b.Move.ParentID = &empty
		}
	}
	res, err := s.store.ApplyBatch(b)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operation_id": res.OperationID, "changed": res.Changed,
		"tasks": res.Tasks, "revision": s.revision()})
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.Undo()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"undo": res, "revision": s.revision()})
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>todo-cli</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family: -apple-system, system-ui, sans-serif; max-width: 40rem; margin: 3rem auto; line-height: 1.6">
<h1>todo-cli 本地服务</h1>
<p>本地服务已启动，与命令行共用同一份任务数据。</p>
<ul>
<li><a href="/api/tasks">/api/tasks</a> — 任务列表（REST API）</li>
<li><a href="/api/health">/api/health</a> — 运行状态</li>
<li>/api/events — 实时变更事件（SSE）</li>
</ul>
</body>
</html>
`
