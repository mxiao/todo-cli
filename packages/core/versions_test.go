package core

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestConflictErrorAndExpectedVersions(t *testing.T) {
	s := openTest(t)
	a := mustCreate(t, s, NewTask{Title: "a"})
	b := mustCreate(t, s, NewTask{Title: "b"})

	_, err := s.Update(a.ID, TaskPatch{Title: ptr("x"), ExpectedVersion: 5})
	var ce *ConflictError
	if !errors.As(err, &ce) || !errors.Is(err, ErrConflict) || ce.TaskID != a.ID || ce.Expected != 5 || ce.Actual != 1 {
		t.Fatalf("conflict error: %#v", err)
	}
	// A stale version anywhere in a batch fails the whole batch.
	_, err = s.ApplyBatch(BatchRequest{Action: BatchComplete, IDs: []string{a.ID, b.ID}, Expect: map[string]int64{a.ID: 1, b.ID: 2}})
	if !errors.As(err, &ce) || ce.TaskID != b.ID {
		t.Fatalf("batch conflict: %v", err)
	}
	if got, _ := s.Get(a.ID); got.Status != StatusTodo {
		t.Fatal("batch partially applied")
	}
	res, err := s.ApplyBatch(BatchRequest{Action: BatchComplete, IDs: []string{a.ID, b.ID}, Expect: map[string]int64{a.ID: 1, b.ID: 1}})
	if err != nil || res.Changed != 2 {
		t.Fatalf("batch: %+v %v", res, err)
	}
	for _, c := range []struct {
		b    BatchRequest
		want Status
	}{
		{BatchRequest{Action: BatchReopen, IDs: []string{a.ID}}, StatusTodo},
		{BatchRequest{Action: BatchStart, IDs: []string{a.ID}}, StatusInProgress},
		{BatchRequest{Action: BatchArchive, IDs: []string{a.ID}}, StatusArchived},
	} {
		if _, err := s.ApplyBatch(c.b); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.Get(a.ID); got.Status != c.want {
			t.Fatalf("%s: %s", c.b.Action, got.Status)
		}
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchPriority, IDs: []string{a.ID}, Priority: PriorityHigh}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchMove, IDs: []string{a.ID}, Move: MoveTarget{Category: ptr("c")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchDelete, IDs: []string{a.ID}, Expect: map[string]int64{a.ID: 1}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	cur, _ := s.Get(a.ID)
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchDelete, IDs: []string{a.ID}, Expect: map[string]int64{a.ID: cur.Version}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchRestore, IDs: []string{a.ID}, Expect: map[string]int64{a.ID: cur.Version}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale restore: %v", err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchRestore, IDs: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: "nope", IDs: []string{a.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err := s.ApplyBatch(BatchRequest{Action: BatchPriority, IDs: []string{a.ID}, Priority: 9}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad priority: %v", err)
	}
	if _, err := s.Reorder(a.ID, Placement{Top: true, ExpectedVersion: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reorder: %v", err)
	}
	for st, want := range map[Status]string{StatusTodo: BatchReopen, StatusInProgress: BatchStart, StatusDone: BatchComplete, StatusArchived: BatchArchive} {
		if got, _ := StatusAction(st); got != want {
			t.Errorf("StatusAction(%s) = %s", st, got)
		}
	}
	if _, err := StatusAction("x"); err == nil {
		t.Error("bad status accepted")
	}
}

func TestVersionSnapshotsAndRevert(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "v1", Tags: []string{"a"}})
	if _, err := s.Update(task.ID, TaskPatch{Title: ptr("v2"), Tags: &[]string{"b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID); err != nil {
		t.Fatal(err)
	}
	vs, err := s.TaskVersions(task.ID)
	if err != nil || len(vs) != 3 {
		t.Fatalf("versions: %+v %v", vs, err)
	}
	if vs[0].Task.Title != "v1" || !slices.Equal(vs[0].Task.Tags, []string{"a"}) || vs[2].Task.Status != StatusDone || vs[1].Actor != "cli" {
		t.Fatalf("snapshots: %+v", vs)
	}
	old, err := s.GetVersion(task.ID, 1)
	if err != nil || old.Title != "v1" || old.Version != 1 {
		t.Fatalf("v1: %+v %v", old, err)
	}
	if _, err := s.GetVersion(task.ID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version: %v", err)
	}
	if _, err := s.Revert(task.ID, 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revert: %v", err)
	}
	got, err := s.Revert(task.ID, 1, 3)
	if err != nil || got.Title != "v1" || got.Status != StatusTodo || got.CompletedAt != nil || !slices.Equal(got.Tags, []string{"a"}) || got.Version != 4 {
		t.Fatalf("revert: %+v %v", got, err)
	}
	// Undo also produces a new, snapshotted version.
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	vs, _ = s.TaskVersions(task.ID)
	if last := vs[len(vs)-1]; last.Version != 5 || last.Task.Title != "v2" {
		t.Fatalf("after undo: %+v", last)
	}
	if _, err := s.TaskVersions("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestVersionsOfTasksFromBeforeSchemaV3(t *testing.T) {
	dir := t.TempDir()
	old, err := openAt(Options{DataDir: dir, Now: fakeClock()}, 2)
	if err != nil {
		t.Fatal(err)
	}
	task := mustCreate(t, old, NewTask{Title: "legacy"})
	old.Close()
	s := openDir(t, dir)
	vs, err := s.TaskVersions(task.ID)
	if err != nil || len(vs) != 1 || vs[0].Version != 1 || vs[0].Task.Title != "legacy" {
		t.Fatalf("live version of a legacy task: %+v %v", vs, err)
	}
	if v, err := s.GetVersion(task.ID, 1); err != nil || v.Title != "legacy" {
		t.Fatalf("GetVersion: %+v %v", v, err)
	}
}

func TestRevisionAndChangesSince(t *testing.T) {
	dir := t.TempDir()
	s := openDir(t, dir)
	other := openDir(t, dir)
	r0, err := s.Revision()
	if err != nil || r0 != 0 {
		t.Fatalf("empty revision: %d %v", r0, err)
	}
	a := mustCreate(t, s, NewTask{Title: "a"})
	r1, _ := s.Revision()
	if _, err := other.Complete(a.ID); err != nil {
		t.Fatal(err)
	}
	r2, _ := s.Revision()
	if !(r0 < r1 && r1 < r2) {
		t.Fatalf("revision must grow with every write from any connection: %d %d %d", r0, r1, r2)
	}
	ch, err := s.ChangesSince(r0, 0)
	if err != nil || len(ch) != 2 || ch[0].Action != "create" || ch[1].Action != "complete" || ch[1].ID != r2 {
		t.Fatalf("changes: %+v %v", ch, err)
	}
	if ch, _ := s.ChangesSince(r0, 1); len(ch) != 1 {
		t.Fatalf("limit: %d", len(ch))
	}
	if ch, _ := s.ChangesSince(r2, 0); len(ch) != 0 {
		t.Fatalf("nothing new: %+v", ch)
	}
}

func TestConflictRecords(t *testing.T) {
	s := openTest(t)
	task := mustCreate(t, s, NewTask{Title: "a"})
	if _, err := s.RecordConflict(task.ID, 1, 2, json.RawMessage(`{bad`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid patch: %v", err)
	}
	c, err := s.RecordConflict(task.ID, 1, 2, json.RawMessage(`{"title":"x"}`))
	if err != nil || c.ID == 0 || c.Actor != "cli" || c.ResolvedAt != nil || string(c.Patch) != `{"title":"x"}` {
		t.Fatalf("record: %+v %v", c, err)
	}
	if cs, _ := s.Conflicts(task.ID, false); len(cs) != 1 {
		t.Fatalf("open: %+v", cs)
	}
	if _, err := s.MarkConflictResolved(c.ID, "maybe"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad resolution: %v", err)
	}
	done, err := s.MarkConflictResolved(c.ID, ResolveTheirs)
	if err != nil || done.Resolution != ResolveTheirs || done.ResolvedAt == nil {
		t.Fatalf("resolve: %+v %v", done, err)
	}
	if _, err := s.MarkConflictResolved(c.ID, ResolveMine); !errors.Is(err, ErrConflictResolved) {
		t.Fatalf("double resolve: %v", err)
	}
	if cs, _ := s.Conflicts(task.ID, false); len(cs) != 0 {
		t.Fatalf("still open: %+v", cs)
	}
	if cs, _ := s.Conflicts(task.ID, true); len(cs) != 1 {
		t.Fatalf("all: %+v", cs)
	}
	if _, err := s.Conflict(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestDecodePatch(t *testing.T) {
	p, err := DecodePatch([]byte(`{"version": 3, "title": "t", "description": null, "due_at": "2026-10-09",
		"priority": "high", "tags": ["#a", "b"], "add_tags": ["c"], "remove_tags": ["b"], "status": "doing",
		"category": "c", "parent_id": null, "notes": "n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpectedVersion != 3 || *p.Title != "t" || *p.Description != "" || *p.ParentID != "" || *p.Priority != PriorityHigh ||
		*p.Status != StatusInProgress || p.DueAt == nil || p.DueAt.In(time.Local).Hour() != 23 || len(*p.Tags) != 2 {
		t.Fatalf("decoded: %+v", p)
	}
	if got := PatchedFields(p); !slices.Equal(got, []string{"category", "description", "due_at", "notes", "parent_id", "priority", "status", "tags", "title"}) {
		t.Fatalf("fields: %v", got)
	}
	task := Task{Title: "old", Tags: []string{"x"}, Status: StatusTodo}
	if err := ApplyPatch(&task, p, t0); err != nil || task.Title != "t" || !slices.Equal(task.Tags, []string{"a", "c"}) || task.Status != StatusInProgress {
		t.Fatalf("applied: %+v %v", task, err)
	}
	if p, err := DecodePatch([]byte(`{"due_at": null}`)); err != nil || !p.ClearDue {
		t.Fatalf("clear due: %+v %v", p, err)
	}
	for _, bad := range []string{`[]`, `null`, `{"title": null}`, `{"title": 3}`, `{"priority": "x"}`, `{"status": "x"}`,
		`{"due_at": "someday"}`, `{"version": 0}`, `{"version": "3"}`, `{"colour": "red"}`, `{"tags": "a"}`} {
		if _, err := DecodePatch([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for in, want := range map[string]string{
		"2026-10-09T18:00:00+08:00": "2026-10-09T10:00:00Z",
		"2026-10-09T10:00:00Z":      "2026-10-09T10:00:00Z",
	} {
		got, err := ParseTimeInput(in, true)
		if err != nil || got.Format(time.RFC3339) != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if got, _ := ParseTimeInput("2026-10-09 18:30", false); got.In(time.Local).Format("15:04") != "18:30" {
		t.Errorf("local time: %v", got)
	}
	if got, _ := ParseTimeInput("2026-10-09", false); got.In(time.Local).Format("15:04") != "00:00" {
		t.Errorf("start of day: %v", got)
	}
}

func TestFieldValues(t *testing.T) {
	due := t0
	v := FieldValues(&Task{Title: "t", Priority: PriorityLow, Status: StatusDone, Tags: []string{"a"}, DueAt: &due})
	if v["title"] != "t" || v["priority"] != "low" || v["status"] != "done" || v["due_at"] != "2026-10-06T09:00:00Z" ||
		v["notes"] != nil || !slices.Equal(v["tags"].([]string), []string{"a"}) {
		t.Fatalf("values: %v", v)
	}
}
