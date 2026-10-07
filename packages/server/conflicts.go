package server

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"

	"github.com/mxiao/todo-cli/packages/core"
)

// conflictView explains a rejected edit: the version the client edited
// (base), the version that won (current), what the client's edit would
// have produced (yours), what resolving with "mine" produces (merged: the
// edit applied on top of current), and a per-field three-way comparison.
type conflictView struct {
	core.Conflict
	Open    bool        `json:"open"`
	Base    *core.Task  `json:"base"`
	Current *core.Task  `json:"current"`
	Yours   *core.Task  `json:"yours"`
	Merged  *core.Task  `json:"merged"`
	Fields  []fieldDiff `json:"fields"`
}

type fieldDiff struct {
	Field           string `json:"field"`
	Base            any    `json:"base"`
	Current         any    `json:"current"`
	Yours           any    `json:"yours"`
	ChangedByOthers bool   `json:"changed_by_others"`
	ChangedByYou    bool   `json:"changed_by_you"`
	// Conflicting means both sides changed the field to different values.
	Conflicting bool `json:"conflicting"`
}

func (s *Server) conflictView(c *core.Conflict) (*conflictView, error) {
	v := &conflictView{Conflict: *c, Open: c.ResolvedAt == nil, Fields: []fieldDiff{}}
	cur, err := s.store.Get(c.TaskID)
	if err != nil {
		return nil, err
	}
	v.Current = cur
	if base, err := s.store.GetVersion(c.TaskID, c.BaseVersion); err == nil {
		v.Base = base
	} else if !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	patch, err := core.DecodePatch(c.Patch)
	if err != nil {
		return nil, err
	}
	from := v.Base
	if from == nil {
		from = cur // the edited version is unknown: compare against the current one
	}
	yours, err := patched(from, patch, c)
	if err != nil {
		return nil, err
	}
	if v.Merged, err = patched(cur, patch, c); err != nil {
		return nil, err
	}
	v.Yours = yours

	bv, cv, yv := core.FieldValues(from), core.FieldValues(cur), core.FieldValues(yours)
	theirs := core.DiffTasks(from, cur)
	mine := core.DiffTasks(from, yours)
	var fields []string
	for f := range theirs {
		fields = append(fields, f)
	}
	for f := range mine {
		if _, ok := theirs[f]; !ok {
			fields = append(fields, f)
		}
	}
	for _, f := range core.PatchedFields(patch) {
		if !slices.Contains(fields, f) {
			fields = append(fields, f)
		}
	}
	slices.Sort(fields)
	for _, f := range fields {
		if f == "position" {
			continue
		}
		_, byThem := theirs[f]
		_, byYou := mine[f]
		v.Fields = append(v.Fields, fieldDiff{
			Field: f, Base: bv[f], Current: cv[f], Yours: yv[f],
			ChangedByOthers: byThem, ChangedByYou: byYou,
			Conflicting: byThem && byYou && !reflect.DeepEqual(cv[f], yv[f]),
		})
	}
	return v, nil
}

func patched(t *core.Task, p core.TaskPatch, c *core.Conflict) (*core.Task, error) {
	out := *t
	out.Tags = slices.Clone(t.Tags)
	if err := core.ApplyPatch(&out, p, c.CreatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

// writeConflictOr answers a failed write. Version conflicts become a 409
// that includes the current task and, when the rejected edit can be
// expressed as a patch, a stored conflict the user can resolve later.
func (s *Server) writeConflictOr(w http.ResponseWriter, r *http.Request, err error, id string, base int64, patch []byte) {
	var ce *core.ConflictError
	if !errors.As(err, &ce) {
		s.fail(w, r, err)
		return
	}
	body := map[string]any{"error": apiError{Code: "version_conflict", Message: err.Error()}}
	if patch != nil {
		c, rerr := s.store.RecordConflict(id, base, ce.Actual, patch)
		if rerr != nil {
			s.fail(w, r, rerr)
			return
		}
		view, verr := s.conflictView(c)
		if verr != nil {
			s.fail(w, r, verr)
			return
		}
		body["conflict"] = view
		body["current"] = view.Current
	} else if cur, gerr := s.store.Get(id); gerr == nil {
		body["current"] = cur
	}
	writeJSON(w, http.StatusConflict, body)
}

func (s *Server) conflictByPath(r *http.Request) (*core.Conflict, error) {
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || id < 1 {
		return nil, fmt.Errorf("%w: conflict id must be a positive integer", core.ErrInvalid)
	}
	return s.store.Conflict(id)
}

func (s *Server) handleConflict(w http.ResponseWriter, r *http.Request) {
	c, err := s.conflictByPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.conflictView(c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conflict": v})
}

func (s *Server) handleTaskConflicts(w http.ResponseWriter, r *http.Request) {
	id, err := s.taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	all, err := strconv.ParseBool(r.URL.Query().Get("all"))
	if err != nil && r.URL.Query().Get("all") != "" {
		s.fail(w, r, fmt.Errorf("%w: all must be true or false", core.ErrInvalid))
		return
	}
	cs, err := s.store.Conflicts(id, all)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := []*conflictView{}
	for i := range cs {
		v, err := s.conflictView(&cs[i])
		if err != nil {
			s.fail(w, r, err)
			return
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": id, "conflicts": views})
}

type resolveRequest struct {
	Resolution string `json:"resolution"`
	// Version is the current task version the user reviewed; it defaults to
	// the version that won the conflict.
	Version int64 `json:"version"`
}

// handleResolve applies ("mine") or discards ("theirs") a stored rejected
// edit. Applying it is itself version-checked, so a task that changed again
// since the review yields another 409 instead of a silent overwrite.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	c, err := s.conflictByPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req resolveRequest
	if _, err := decodeBody(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if c.ResolvedAt != nil {
		s.fail(w, r, fmt.Errorf("%w: conflict %d was already resolved (%s)", core.ErrConflictResolved, c.ID, c.Resolution))
		return
	}
	switch req.Resolution {
	case core.ResolveMine:
		p, err := core.DecodePatch(c.Patch)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		p.ExpectedVersion = req.Version
		if p.ExpectedVersion == 0 {
			p.ExpectedVersion = c.CurrentVersion
		}
		if _, err := s.store.Update(c.TaskID, p); err != nil {
			var ce *core.ConflictError
			if errors.As(err, &ce) {
				body := map[string]any{"error": apiError{Code: "version_conflict", Message: err.Error()}}
				if v, verr := s.conflictView(c); verr == nil {
					body["conflict"] = v
					body["current"] = v.Current
				}
				writeJSON(w, http.StatusConflict, body)
				return
			}
			s.fail(w, r, err)
			return
		}
	case core.ResolveTheirs:
	default:
		s.fail(w, r, fmt.Errorf("%w: resolution must be %q or %q", core.ErrInvalid, core.ResolveMine, core.ResolveTheirs))
		return
	}
	resolved, err := s.store.MarkConflictResolved(c.ID, req.Resolution)
	if err != nil && !errors.Is(err, core.ErrConflictResolved) {
		s.fail(w, r, err)
		return
	}
	cur, err := s.store.Get(c.TaskID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	setETag(w, cur)
	writeJSON(w, http.StatusOK, map[string]any{"conflict": resolved, "task": cur, "revision": s.revision()})
}
