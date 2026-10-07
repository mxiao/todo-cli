package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Operation is one undoable write (a single command, possibly a batch).
type Operation struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Summary string `json:"summary"`
}

// UndoResult reports what Undo reverted.
type UndoResult struct {
	Operation Operation `json:"operation"`
	// Restored holds tasks returned to their pre-operation state.
	Restored []Task `json:"restored"`
	// Removed lists tasks that the operation had created and are now gone.
	Removed []string `json:"removed"`
}

// Undo reverts the most recent operation that has not been undone yet.
// Calling it again steps further back.
func (s *Store) Undo() (*UndoResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var op Operation
	var raw string
	err = tx.QueryRow(`SELECT id, kind, actor, summary, snapshots FROM operations
		WHERE undone_at IS NULL AND snapshots != '[]' ORDER BY id DESC LIMIT 1`).
		Scan(&op.ID, &op.Kind, &op.Actor, &op.Summary, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNothingToUndo
	}
	if err != nil {
		return nil, err
	}
	var snaps []snapshot
	if err := json.Unmarshal([]byte(raw), &snaps); err != nil {
		return nil, fmt.Errorf("operation %d: corrupt snapshot: %w", op.ID, err)
	}
	res, err := s.undoSnapshots(tx, op, snaps)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// undoSnapshots restores the pre-operation state of every task an
// operation touched and marks it undone, inside tx.
func (s *Store) undoSnapshots(tx *sql.Tx, op Operation, snaps []snapshot) (*UndoResult, error) {
	x := &txn{s: s, tx: tx, now: s.clock(), opID: op.ID}
	res := &UndoResult{Operation: op, Restored: []Task{}, Removed: []string{}}
	action := "undo_" + op.Kind
	for _, sn := range snaps {
		cur, err := x.get(sn.TaskID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if sn.Before == nil {
			if _, err := tx.Exec(`DELETE FROM tasks WHERE id = ?`, sn.TaskID); err != nil {
				return nil, err
			}
			if err := x.history(sn.TaskID, action, nil); err != nil {
				return nil, err
			}
			res.Removed = append(res.Removed, sn.TaskID)
			continue
		}
		t := sn.Before.clone()
		t.UpdatedAt = x.now
		t.Version = sn.Before.Version + 1
		if cur != nil && cur.Version >= t.Version {
			t.Version = cur.Version + 1
		}
		if err := x.put(t); err != nil {
			return nil, err
		}
		if err := x.history(t.ID, action, diffTasks(cur, t)); err != nil {
			return nil, err
		}
		res.Restored = append(res.Restored, *t)
	}
	if _, err := tx.Exec(`UPDATE operations SET undone_at = ? WHERE id = ?`, fmtTime(x.now), op.ID); err != nil {
		return nil, err
	}
	return res, nil
}
