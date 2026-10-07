package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

// fakeClock advances one second per call so timestamps are distinct.
func fakeClock() func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return t0.Add(time.Duration(n) * time.Second)
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	return openDir(t, t.TempDir())
}

func openDir(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(Options{DataDir: dir, Now: fakeClock()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, in NewTask) *Task {
	t.Helper()
	task, err := s.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func ptr[T any](v T) *T { return &v }

func titles(ts []Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Title
	}
	return out
}

func TestCreateAndGetAllFields(t *testing.T) {
	s := openTest(t)
	parent := mustCreate(t, s, NewTask{Title: "parent"})
	due := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	task := mustCreate(t, s, NewTask{
		Title: "  写周报 ", Description: "desc", DueAt: &due, Priority: PriorityHigh,
		Tags: []string{"work", "#weekly", "work", " "}, Category: "office", ParentID: parent.ID, Notes: "n1",
	})
	got, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "写周报" || got.Description != "desc" || got.Notes != "n1" || got.Category != "office" ||
		got.ParentID != parent.ID || got.Priority != PriorityHigh || got.Status != StatusTodo || got.Version != 1 {
		t.Fatalf("unexpected task: %+v", got)
	}
	if !slices.Equal(got.Tags, []string{"weekly", "work"}) {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.DueAt == nil || !got.DueAt.Equal(due) {
		t.Fatalf("due = %v", got.DueAt)
	}
	if got.CreatedAt.IsZero() || !got.CreatedAt.Equal(got.UpdatedAt) || got.CompletedAt != nil {
		t.Fatalf("timestamps: %+v", got)
	}
	if len(got.ID) != 36 || got.ID == parent.ID {
		t.Fatalf("bad id %q", got.ID)
	}
	hist, _ := s.History(task.ID)
	if len(hist) != 1 || hist[0].Action != "create" || hist[0].Actor != "cli" || hist[0].Changes["title"].To != "写周报" {
		t.Fatalf("history = %+v", hist)
	}
}

func TestCreateValidation(t *testing.T) {
	s := openTest(t)
	if _, err := s.Create(NewTask{Title: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title: %v", err)
	}
	if _, err := s.Create(NewTask{Title: "x", ParentID: "missing"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing parent: %v", err)
	}
	if _, err := s.Create(NewTask{Title: "x", Priority: 9}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad priority: %v", err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// Failed writes must not leave rows behind.
	if all, _ := s.List(Filter{IncludeArchived: true, IncludeDeleted: true}); len(all) != 0 {
		t.Fatalf("leftover rows: %v", titles(all))
	}
}

func TestUpdatePatchAndOptimisticLock(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "a", Tags: []string{"x", "y"}, DueAt: ptr(t0)})
	up, err := s.Update(task.ID, TaskPatch{
		Title: ptr("b"), Priority: ptr(PriorityUrgent), AddTags: []string{"z"}, RemoveTags: []string{"x"},
		ClearDue: true, Notes: ptr("more"), ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Title != "b" || up.Priority != PriorityUrgent || !slices.Equal(up.Tags, []string{"y", "z"}) ||
		up.DueAt != nil || up.Notes != "more" || up.Version != 2 || !up.UpdatedAt.After(up.CreatedAt) {
		t.Fatalf("unexpected update: %+v", up)
	}
	if _, err := s.Update(task.ID, TaskPatch{Title: ptr("c"), ExpectedVersion: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version should conflict, got %v", err)
	}
	// A no-op patch does not bump the version or add history.
	same, err := s.Update(task.ID, TaskPatch{Title: ptr("b")})
	if err != nil || same.Version != 2 {
		t.Fatalf("no-op update: %+v %v", same, err)
	}
	hist, _ := s.History(task.ID)
	if len(hist) != 2 {
		t.Fatalf("history len = %d", len(hist))
	}
	ch := hist[1].Changes
	if ch["title"].From != "a" || ch["title"].To != "b" || ch["priority"].To != "urgent" {
		t.Fatalf("changes = %+v", ch)
	}
}

func TestParentCycleRejected(t *testing.T) {
	s := openTest(t)
	a := mustCreate(t, s, NewTask{Title: "a"})
	b := mustCreate(t, s, NewTask{Title: "b", ParentID: a.ID})
	if _, err := s.Update(a.ID, TaskPatch{ParentID: &b.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := s.Update(a.ID, TaskPatch{ParentID: &a.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self parent: %v", err)
	}
}

func TestStatusLifecycle(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "a"})
	res, err := s.Complete(task.ID)
	if err != nil || res.Changed != 1 {
		t.Fatal(res, err)
	}
	done := res.Tasks[0]
	if done.Status != StatusDone || done.CompletedAt == nil {
		t.Fatalf("complete: %+v", done)
	}
	// Completing again is a no-op and records nothing.
	again, _ := s.Complete(task.ID)
	if again.Changed != 0 || again.OperationID != 0 {
		t.Fatalf("idempotent complete: %+v", again)
	}
	res, _ = s.Reopen(task.ID)
	if r := res.Tasks[0]; r.Status != StatusTodo || r.CompletedAt != nil {
		t.Fatalf("reopen: %+v", r)
	}
	res, _ = s.Start(task.ID)
	if res.Tasks[0].Status != StatusInProgress {
		t.Fatalf("start: %+v", res.Tasks[0])
	}
	res, _ = s.Archive(task.ID)
	if r := res.Tasks[0]; r.Status != StatusArchived || r.ArchivedAt == nil {
		t.Fatalf("archive: %+v", r)
	}
	if l, _ := s.List(Filter{}); len(l) != 0 {
		t.Fatal("archived tasks must be hidden by default")
	}
	if l, _ := s.List(Filter{IncludeArchived: true}); len(l) != 1 {
		t.Fatal("archived tasks visible with IncludeArchived")
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "a"})
	if _, err := s.Delete(task.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(task.ID)
	if err != nil || got.DeletedAt == nil {
		t.Fatalf("soft-deleted row must remain: %+v %v", got, err)
	}
	if l, _ := s.List(Filter{IncludeArchived: true}); len(l) != 0 {
		t.Fatal("deleted task listed")
	}
	if l, _ := s.List(Filter{OnlyDeleted: true}); len(l) != 1 {
		t.Fatal("deleted task missing from OnlyDeleted")
	}
	if _, err := s.Update(task.ID, TaskPatch{Title: ptr("x")}); !errors.Is(err, ErrDeleted) {
		t.Fatalf("editing deleted task: %v", err)
	}
	if _, err := s.Restore(task.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(Filter{}); len(l) != 1 {
		t.Fatal("restored task not listed")
	}
}

func TestResolveID(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "a"})
	if id, err := s.ResolveID(task.ID[:6]); err != nil || id != task.ID {
		t.Fatalf("prefix: %q %v", id, err)
	}
	if id, err := s.ResolveID(strings.ToUpper(task.ID)); err != nil || id != task.ID {
		t.Fatalf("full id: %q %v", id, err)
	}
	if _, err := s.ResolveID("zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	mustCreate(t, s, NewTask{Title: "b"})
	if _, err := s.ResolveID("%"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LIKE wildcard must be escaped: %v", err)
	}
}

func seedFilterData(t *testing.T, s *Store) map[string]*Task {
	day := func(d int) *time.Time { v := t0.AddDate(0, 0, d); return &v }
	m := map[string]*Task{}
	m["report"] = mustCreate(t, s, NewTask{Title: "write report", Priority: PriorityHigh, DueAt: day(2), Tags: []string{"work"}, Category: "office"})
	m["milk"] = mustCreate(t, s, NewTask{Title: "buy milk", Priority: PriorityLow, Tags: []string{"home"}, Notes: "lactose free"})
	m["tax"] = mustCreate(t, s, NewTask{Title: "file taxes", Priority: PriorityUrgent, DueAt: day(-1), Tags: []string{"home", "money"}})
	m["deck"] = mustCreate(t, s, NewTask{Title: "slides", Priority: PriorityMedium, DueAt: day(5), Tags: []string{"work"}, Category: "office", ParentID: m["report"].ID})
	m["old"] = mustCreate(t, s, NewTask{Title: "old thing", Status: StatusDone})
	return m
}

func TestListFilters(t *testing.T) {
	s := openTest(t)
	m := seedFilterData(t, s)
	cases := []struct {
		name string
		f    Filter
		want []string
	}{
		{"default excludes nothing but archived", Filter{}, []string{"write report", "buy milk", "file taxes", "slides", "old thing"}},
		{"status", Filter{Statuses: []Status{StatusDone}}, []string{"old thing"}},
		{"priority", Filter{Priorities: []Priority{PriorityHigh, PriorityUrgent}}, []string{"write report", "file taxes"}},
		{"min priority", Filter{MinPriority: ptr(PriorityMedium)}, []string{"write report", "file taxes", "slides"}},
		{"tag", Filter{Tags: []string{"home"}}, []string{"buy milk", "file taxes"}},
		{"tags all", Filter{Tags: []string{"home", "#money"}}, []string{"file taxes"}},
		{"category", Filter{Category: "office"}, []string{"write report", "slides"}},
		{"parent", Filter{ParentID: &m["report"].ID}, []string{"slides"}},
		{"top level", Filter{ParentID: ptr("")}, []string{"write report", "buy milk", "file taxes", "old thing"}},
		{"due before", Filter{DueBefore: ptr(t0.AddDate(0, 0, 3))}, []string{"write report", "file taxes"}},
		{"due after", Filter{DueAfter: ptr(t0)}, []string{"write report", "slides"}},
		{"overdue", Filter{Overdue: true}, []string{"file taxes"}},
		{"no due", Filter{HasDue: ptr(false)}, []string{"buy milk", "old thing"}},
		{"search title", Filter{Query: "REPORT"}, []string{"write report"}},
		{"search notes", Filter{Query: "lactose"}, []string{"buy milk"}},
		{"search tag", Filter{Query: "money"}, []string{"file taxes"}},
		{"search terms AND", Filter{Query: "file tax"}, []string{"file taxes"}},
		{"search no match", Filter{Query: "zzz"}, []string{}},
		{"limit", Filter{Limit: 2}, []string{"write report", "buy milk"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.List(c.f)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(titles(got), c.want) {
				t.Fatalf("got %v, want %v", titles(got), c.want)
			}
		})
	}
}

func TestListSorting(t *testing.T) {
	s := openTest(t)
	m := seedFilterData(t, s)
	if _, err := s.Update(m["milk"].ID, TaskPatch{Notes: ptr("touched")}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    Filter
		want []string
	}{
		{Filter{Sort: SortDue}, []string{"file taxes", "write report", "slides", "buy milk", "old thing"}},
		{Filter{Sort: SortDue, Reverse: true}, []string{"slides", "write report", "file taxes", "buy milk", "old thing"}},
		{Filter{Sort: SortPriority}, []string{"file taxes", "write report", "slides", "buy milk", "old thing"}},
		{Filter{Sort: SortCreated}, []string{"old thing", "slides", "file taxes", "buy milk", "write report"}},
		{Filter{Sort: SortCreated, Reverse: true}, []string{"write report", "buy milk", "file taxes", "slides", "old thing"}},
		{Filter{Sort: SortUpdated}, []string{"buy milk", "old thing", "slides", "file taxes", "write report"}},
		{Filter{Sort: SortManual}, []string{"write report", "buy milk", "file taxes", "slides", "old thing"}},
	}
	for _, c := range cases {
		got, _ := s.List(c.f)
		if !slices.Equal(titles(got), c.want) {
			t.Errorf("sort %s reverse=%v: got %v, want %v", c.f.Sort, c.f.Reverse, titles(got), c.want)
		}
	}

	// Manual reordering.
	if _, err := s.Reorder(m["old"].ID, Placement{Top: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorder(m["report"].ID, Placement{Bottom: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorder(m["deck"].ID, Placement{Before: m["milk"].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reorder(m["tax"].ID, Placement{After: m["old"].ID}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(Filter{Sort: SortManual})
	want := []string{"old thing", "file taxes", "slides", "buy milk", "write report"}
	if !slices.Equal(titles(got), want) {
		t.Fatalf("manual order: got %v, want %v", titles(got), want)
	}
	if _, err := s.Reorder(m["tax"].ID, Placement{Top: true, Bottom: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ambiguous placement: %v", err)
	}
	if _, err := ParseSortField("weird"); !errors.Is(err, ErrInvalid) {
		t.Fatal("bad sort accepted")
	}
}

func TestBatchOperations(t *testing.T) {
	s := openTest(t)
	a := mustCreate(t, s, NewTask{Title: "a"})
	b := mustCreate(t, s, NewTask{Title: "b"})
	c := mustCreate(t, s, NewTask{Title: "c", Category: "old"})
	parent := mustCreate(t, s, NewTask{Title: "p"})

	res, err := s.Complete(a.ID, b.ID, a.ID)
	if err != nil || res.Changed != 2 || len(res.Tasks) != 2 {
		t.Fatalf("batch complete: %+v %v", res, err)
	}
	res, err = s.SetPriority(PriorityHigh, a.ID, b.ID, c.ID)
	if err != nil || res.Changed != 3 {
		t.Fatalf("batch priority: %+v %v", res, err)
	}
	res, err = s.Move(MoveTarget{Category: ptr("new"), ParentID: &parent.ID}, b.ID, c.ID)
	if err != nil || res.Changed != 2 {
		t.Fatalf("batch move: %+v %v", res, err)
	}
	if l, _ := s.List(Filter{Category: "new", ParentID: &parent.ID, IncludeArchived: true}); len(l) != 2 {
		t.Fatalf("moved tasks: %v", titles(l))
	}
	if _, err := s.Move(MoveTarget{}, a.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty move target: %v", err)
	}
	res, err = s.Archive(a.ID, b.ID, c.ID)
	if err != nil || res.Changed != 3 {
		t.Fatalf("batch archive: %+v %v", res, err)
	}

	// A batch is atomic: one bad id rolls back every change.
	if _, err := s.Reopen(a.ID, "missing", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("batch with missing id: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got, _ := s.Get(id); got.Status != StatusArchived {
			t.Fatalf("partial batch write leaked: %+v", got)
		}
	}
}

func TestUndo(t *testing.T) {
	s := openTest(t)
	if _, err := s.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("empty undo: %v", err)
	}
	a := mustCreate(t, s, NewTask{Title: "a", Priority: PriorityLow})
	b := mustCreate(t, s, NewTask{Title: "b"})

	if _, err := s.Delete(a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	u, err := s.Undo()
	if err != nil || u.Operation.Kind != "delete" || len(u.Restored) != 2 {
		t.Fatalf("undo delete: %+v %v", u, err)
	}
	if l, _ := s.List(Filter{}); len(l) != 2 {
		t.Fatal("undo did not restore deleted tasks")
	}

	if _, err := s.SetPriority(PriorityUrgent, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(a.ID)
	if got.Priority != PriorityLow {
		t.Fatalf("priority not restored: %v", got.Priority)
	}
	if got.Version <= 3 {
		t.Fatalf("undo must keep versions monotonic, got %d", got.Version)
	}
	hist, _ := s.History(a.ID)
	if last := hist[len(hist)-1]; last.Action != "undo_priority" || last.Changes["priority"].To != "low" {
		t.Fatalf("undo history: %+v", last)
	}

	// Further undo steps walk back: next is the creation of b.
	u, err = s.Undo()
	if err != nil || !slices.Equal(u.Removed, []string{b.ID}) {
		t.Fatalf("undo create: %+v %v", u, err)
	}
	if _, err := s.Get(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("created task should be gone: %v", err)
	}
	if _, err := s.Undo(); err != nil { // creation of a
		t.Fatal(err)
	}
	if _, err := s.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("history exhausted: %v", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	src := openTest(t)
	m := seedFilterData(t, src)
	src.Delete(m["milk"].ID)
	src.Archive(m["old"].ID)

	var buf bytes.Buffer
	if err := src.ExportJSON(&buf); err != nil {
		t.Fatal(err)
	}
	doc, err := ReadExport(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != ExportFormat || doc.SchemaVersion != LatestSchemaVersion() || len(doc.Tasks) != 5 || len(doc.History) == 0 {
		t.Fatalf("export doc: %+v", doc)
	}

	dst := openTest(t)
	res, err := dst.Import(doc, ImportOptions{})
	if err != nil || res.Inserted != 5 {
		t.Fatalf("import: %+v %v", res, err)
	}
	all := Filter{IncludeArchived: true, IncludeDeleted: true}
	want, _ := src.List(all)
	got, _ := dst.List(all)
	if len(got) != len(want) {
		t.Fatalf("task count %d != %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.ID != g.ID || w.Title != g.Title || w.ParentID != g.ParentID || !slices.Equal(w.Tags, g.Tags) ||
			w.Status != g.Status || !timeEq(w.DueAt, g.DueAt) || !timeEq(w.DeletedAt, g.DeletedAt) || w.Priority != g.Priority {
			t.Fatalf("task mismatch:\n%+v\n%+v", w, g)
		}
	}
	hist, _ := dst.History(m["report"].ID)
	if len(hist) < 2 || hist[0].Action != "create" || hist[len(hist)-1].Action != "import" {
		t.Fatalf("imported history: %+v", hist)
	}

	// Re-importing the same file changes nothing.
	res, _ = dst.Import(doc, ImportOptions{})
	if res.Skipped != 5 || res.OperationID != 0 {
		t.Fatalf("idempotent import: %+v", res)
	}
	// The whole import is undoable.
	if _, err := dst.Undo(); err != nil {
		t.Fatal(err)
	}
	if l, _ := dst.List(all); len(l) != 0 {
		t.Fatalf("undo import left %v", titles(l))
	}
}

func TestImportMergeAndReplace(t *testing.T) {
	s := openTest(t)
	keep := mustCreate(t, s, NewTask{Title: "keep"})
	local := mustCreate(t, s, NewTask{Title: "local only"})
	doc, _ := s.Export()

	// Newer copy in the file wins; older copy is skipped.
	doc.Tasks = slices.DeleteFunc(doc.Tasks, func(t Task) bool { return t.ID == local.ID })
	doc.Tasks[0].Title = "keep (edited elsewhere)"
	doc.Tasks[0].UpdatedAt = t0.Add(time.Hour)
	doc.Tasks = append(doc.Tasks, Task{ID: "new-1", Title: "from file", Status: StatusTodo, CreatedAt: t0, UpdatedAt: t0, Version: 1})

	res, err := s.Import(doc, ImportOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || res.Inserted != 1 || res.Deleted != 1 {
		t.Fatalf("result: %+v", res)
	}
	got, _ := s.Get(keep.ID)
	if got.Title != "keep (edited elsewhere)" || got.Version != 2 {
		t.Fatalf("merged task: %+v", got)
	}
	if got, _ := s.Get(local.ID); got.DeletedAt == nil {
		t.Fatal("replace should soft-delete tasks missing from file")
	}

	stale := *doc
	stale.Tasks = []Task{doc.Tasks[0]}
	stale.Tasks[0].Title = "stale"
	stale.Tasks[0].UpdatedAt = t0
	res, _ = s.Import(&stale, ImportOptions{})
	if res.Skipped != 1 {
		t.Fatalf("stale import should skip: %+v", res)
	}
}

func TestImportRejectsInvalidFiles(t *testing.T) {
	s := openTest(t)
	bad := []string{
		`not json`,
		`{"format":"other","format_version":1}`,
		`{"format":"todo-cli/export","format_version":99}`,
		`{"format":"todo-cli/export","format_version":1,"tasks":[{"id":"a","title":"","status":"todo","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
		`{"format":"todo-cli/export","format_version":1,"tasks":[{"id":"a","title":"x","status":"weird","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
		`{"format":"todo-cli/export","format_version":1,"tasks":[{"id":"a","title":"x","status":"todo","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},{"id":"a","title":"y","status":"todo","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`,
	}
	for _, in := range bad {
		if _, err := ReadExport(strings.NewReader(in)); !errors.Is(err, ErrInvalidImportFile) {
			t.Errorf("accepted %s: %v", in, err)
		}
	}
	// Dangling parent is caught inside the transaction and rolled back.
	doc, err := ReadExport(strings.NewReader(`{"format":"todo-cli/export","format_version":1,"tasks":[
		{"id":"a","title":"x","status":"todo","parent_id":"ghost","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(doc, ImportOptions{}); !errors.Is(err, ErrInvalidImportFile) {
		t.Fatalf("dangling parent: %v", err)
	}
	if l, _ := s.List(Filter{IncludeDeleted: true, IncludeArchived: true}); len(l) != 0 {
		t.Fatal("failed import left rows")
	}
}

func TestMigrationBacksUpAndUpgrades(t *testing.T) {
	dir := t.TempDir()
	old, err := openAt(Options{DataDir: dir, Now: fakeClock()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := old.SchemaVersion(); v != 1 {
		t.Fatalf("v1 setup: %d", v)
	}
	if old.LastMigration == nil || old.LastMigration.BackupPath != "" {
		t.Fatalf("fresh database needs no backup: %+v", old.LastMigration)
	}
	task := mustCreate(t, old, NewTask{Title: "survives upgrade", Tags: []string{"x"}})
	old.Close()

	s := openDir(t, dir)
	m := s.LastMigration
	if m == nil || m.From != 1 || m.To != LatestSchemaVersion() || m.BackupPath == "" {
		t.Fatalf("migration report: %+v", m)
	}
	if v, _ := s.SchemaVersion(); v != LatestSchemaVersion() {
		t.Fatalf("schema version %d", v)
	}
	got, err := s.Get(task.ID)
	if err != nil || got.Title != "survives upgrade" || !slices.Equal(got.Tags, []string{"x"}) {
		t.Fatalf("data after migration: %+v %v", got, err)
	}
	// The backup is a usable v1 database holding the same data.
	backup, err := openAt(Options{DataDir: t.TempDir()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var title string
	var ver int
	if _, err := backup.db.Exec(`ATTACH DATABASE ? AS b`, m.BackupPath); err != nil {
		t.Fatal(err)
	}
	backup.db.QueryRow(`SELECT title FROM b.tasks`).Scan(&title)
	backup.db.QueryRow(`SELECT max(version) FROM b.schema_migrations`).Scan(&ver)
	if title != "survives upgrade" || ver != 1 {
		t.Fatalf("backup content: %q v%d", title, ver)
	}
	if list, _ := s.Backups(); len(list) != 1 || list[0] != m.BackupPath {
		t.Fatalf("backups: %v", list)
	}

	// Reopening an up-to-date database does nothing.
	s.Close()
	again := openDir(t, dir)
	if again.LastMigration != nil {
		t.Fatalf("unexpected migration: %+v", again.LastMigration)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	old, err := openAt(Options{DataDir: dir, Now: fakeClock()}, 1)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, old, NewTask{Title: "keep me"})
	old.Close()

	saved := migrations
	migrations = append(slices.Clone(saved), `CREATE TABLE extra(x); THIS IS NOT SQL;`)
	defer func() { migrations = saved }()

	if _, err := Open(Options{DataDir: dir, Now: fakeClock()}); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected migration failure, got %v", err)
	}
	migrations = saved
	s := openDir(t, dir) // v1 -> latest succeeds now: proves nothing from the failed run stuck
	if v, _ := s.SchemaVersion(); v != LatestSchemaVersion() {
		t.Fatalf("version %d", v)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'extra'`).Scan(&n)
	if n != 0 {
		t.Fatal("failed migration left partial schema")
	}
	if l, _ := s.List(Filter{}); len(l) != 1 {
		t.Fatal("data lost after failed migration")
	}
	if b, _ := s.Backups(); len(b) != 2 {
		t.Fatalf("each upgrade attempt should back up first, got %v", b)
	}
}

func TestSchemaTooNewIsRejected(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir)
	if _, err := s.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (99, 'x')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(Options{DataDir: dir}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("expected ErrSchemaTooNew, got %v", err)
	}
}

func TestDataFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := openDir(t, dir)
	p, err := s.Backup("manual")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, s.DBPath(): 0o600, p: 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", path, fi.Mode().Perm(), want)
		}
	}
}

func TestStats(t *testing.T) {
	s := openTest(t)
	m := seedFilterData(t, s)
	s.Delete(m["milk"].ID)
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Deleted != 1 || st.ByStatus[StatusTodo] != 3 || st.ByStatus[StatusDone] != 1 || st.Overdue != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestParsers(t *testing.T) {
	for in, want := range map[string]Priority{"h": PriorityHigh, "URGENT": PriorityUrgent, "2": PriorityMedium, "": PriorityNone} {
		if got, err := ParsePriority(in); err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %v, %v", in, got, err)
		}
	}
	for in, want := range map[string]Status{"doing": StatusInProgress, "completed": StatusDone, "todo": StatusTodo} {
		if got, err := ParseStatus(in); err != nil || got != want {
			t.Errorf("ParseStatus(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseStatus("nope"); !errors.Is(err, ErrInvalid) {
		t.Error("bad status accepted")
	}
}

func TestManyTasksStayFast(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	s := openTest(t)
	doc := &ExportDocument{Format: ExportFormat, FormatVersion: 1}
	for i := 0; i < 10000; i++ {
		doc.Tasks = append(doc.Tasks, Task{ID: newID(), Title: "task", Status: StatusTodo, Priority: Priority(i % 5),
			Tags: []string{"t" + string(rune('a'+i%5))}, Position: float64(i), CreatedAt: t0, UpdatedAt: t0, Version: 1})
	}
	if _, err := s.Import(doc, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := s.List(Filter{Tags: []string{"ta"}, Query: "task", Sort: SortPriority})
	if err != nil || len(got) != 2000 {
		t.Fatalf("list: %d %v", len(got), err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("filtered list over 10k tasks took %v (NFR-012: < 1s)", d)
	}
}
