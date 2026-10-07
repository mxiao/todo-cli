package core

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestDependenciesPersistDiffAndValidate(t *testing.T) {
	s := openTest(t)
	a := mustCreate(t, s, NewTask{Title: "a"})
	b := mustCreate(t, s, NewTask{Title: "b", DependsOn: []string{a.ID}})
	got, err := s.Get(b.ID)
	if err != nil || !slices.Equal(got.DependsOn, []string{a.ID}) {
		t.Fatalf("deps: %v %v", got.DependsOn, err)
	}
	// a -> b would close a cycle.
	if _, err := s.Update(a.ID, TaskPatch{DependsOn: &[]string{b.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cycle accepted: %v", err)
	}
	if _, err := s.Update(a.ID, TaskPatch{DependsOn: &[]string{a.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self dependency accepted: %v", err)
	}
	if _, err := s.Update(a.ID, TaskPatch{DependsOn: &[]string{"missing"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing dependency accepted: %v", err)
	}
	c := mustCreate(t, s, NewTask{Title: "c"})
	if _, err := s.Update(b.ID, TaskPatch{DependsOn: &[]string{a.ID, c.ID}}); err != nil {
		t.Fatal(err)
	}
	hist, _ := s.History(b.ID)
	if ch, ok := hist[len(hist)-1].Changes["depends_on"]; !ok || ch.From == nil {
		t.Fatalf("history lacks depends_on change: %+v", hist[len(hist)-1])
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(b.ID)
	if !slices.Equal(got.DependsOn, []string{a.ID}) {
		t.Fatalf("undo did not restore deps: %v", got.DependsOn)
	}
	p, err := DecodePatch([]byte(`{"depends_on": null}`))
	if err != nil || p.DependsOn == nil || len(*p.DependsOn) != 0 {
		t.Fatalf("decode depends_on: %+v %v", p, err)
	}
}

func TestDoTryAndUndoOperation(t *testing.T) {
	s := openTest(t)
	keep := mustCreate(t, s, NewTask{Title: "keep"})
	llm := s.WithActor("llm")
	var created *Task
	var stepErr error
	op, err := llm.Do("llm_intake", "intake", func(tx *Tx) error {
		var err error
		if created, err = tx.Create(NewTask{Title: "new", DependsOn: []string{keep.ID}}); err != nil {
			return err
		}
		stepErr = tx.Try(func() error {
			if _, err := tx.Update(keep.ID, TaskPatch{Title: ptr("changed")}); err != nil {
				return err
			}
			_, err := tx.Create(NewTask{Title: "  "}) // invalid: rolls the whole step back
			return err
		})
		_, err = tx.Update(keep.ID, TaskPatch{Priority: ptr(PriorityHigh)})
		return err
	})
	if err != nil || op == 0 || !errors.Is(stepErr, ErrInvalid) {
		t.Fatalf("op %d err %v step %v", op, err, stepErr)
	}
	k, _ := s.Get(keep.ID)
	if k.Title != "keep" || k.Priority != PriorityHigh {
		t.Fatalf("failed step leaked or later step lost: %+v", k)
	}
	hist, _ := s.History(created.ID)
	if hist[0].Actor != "llm" {
		t.Fatalf("actor %q", hist[0].Actor)
	}
	info, err := s.OperationByID(op)
	if err != nil || info.Kind != "llm_intake" || len(info.TaskIDs) != 2 {
		t.Fatalf("operation info %+v %v", info, err)
	}

	// A later, still effective change blocks undoing the operation.
	if _, err := s.Update(keep.ID, TaskPatch{Notes: ptr("user")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoOperation(op); !errors.Is(err, ErrConflict) {
		t.Fatalf("undo over a later change: %v", err)
	}
	if _, err := s.Undo(); err != nil { // undo the user's note
		t.Fatal(err)
	}
	res, err := s.UndoOperation(op)
	if err != nil || len(res.Removed) != 1 || len(res.Restored) != 1 {
		t.Fatalf("undo op: %+v %v", res, err)
	}
	if _, err := s.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("created task still there: %v", err)
	}
	k, _ = s.Get(keep.ID)
	if k.Priority != PriorityNone {
		t.Fatalf("priority not restored: %+v", k)
	}
	if _, err := s.UndoOperation(op); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("second undo: %v", err)
	}
}

func TestMigrateModule(t *testing.T) {
	s := openTest(t)
	migs := []string{`CREATE TABLE mod_a (id INTEGER PRIMARY KEY)`}
	for range 2 {
		if err := s.MigrateModule("mod", migs); err != nil {
			t.Fatal(err)
		}
	}
	migs = append(migs, `ALTER TABLE mod_a ADD COLUMN name TEXT`, `BROKEN SQL`)
	if err := s.MigrateModule("mod", migs); err == nil {
		t.Fatal("broken migration accepted")
	}
	if _, err := s.DB().Exec(`INSERT INTO mod_a(id, name) VALUES (1, 'x')`); err == nil {
		t.Fatal("failed migration was not rolled back")
	}
	if err := s.MigrateModule("mod", migs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateModule("mod", nil); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer module schema: %v", err)
	}
}

func TestParseWhenRelativeDayWithClock(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, loc) // Tuesday
	for in, want := range map[string]time.Time{
		"明天 18:00":  time.Date(2026, 10, 7, 18, 0, 0, 0, loc),
		"fri 9:30":  time.Date(2026, 10, 9, 9, 30, 0, 0, loc),
		"下周一 08:00": time.Date(2026, 10, 12, 8, 0, 0, 0, loc),
	} {
		got, dateOnly, err := ParseWhen(in, now)
		if err != nil || dateOnly || !got.Equal(want) {
			t.Errorf("%s: %v %v %v", in, got, dateOnly, err)
		}
	}
	if _, _, err := ParseWhen("明天 25:00", now); err == nil {
		t.Error("bad clock accepted")
	}
}
