package core

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

const (
	// ExportFormat tags todo-cli export documents.
	ExportFormat = "todo-cli/export"
	// ExportFormatVersion is the export document layout version.
	ExportFormatVersion = 1
)

// ExportDocument is the JSON export/import layout. It carries every task,
// including soft-deleted ones, plus the full change history.
type ExportDocument struct {
	Format        string         `json:"format"`
	FormatVersion int            `json:"format_version"`
	SchemaVersion int            `json:"schema_version"`
	ExportedAt    string         `json:"exported_at"`
	Tasks         []Task         `json:"tasks"`
	History       []HistoryEntry `json:"history"`
}

// Export builds an export document of the whole store.
func (s *Store) Export() (*ExportDocument, error) {
	tasks, err := s.List(Filter{IncludeArchived: true, IncludeDeleted: true, Sort: SortManual})
	if err != nil {
		return nil, err
	}
	hist, err := s.history("")
	if err != nil {
		return nil, err
	}
	v, err := s.SchemaVersion()
	if err != nil {
		return nil, err
	}
	return &ExportDocument{
		Format: ExportFormat, FormatVersion: ExportFormatVersion, SchemaVersion: v,
		ExportedAt: s.clock().Format("2006-01-02T15:04:05Z07:00"), Tasks: tasks, History: hist,
	}, nil
}

// ExportJSON writes the export document as indented JSON.
func (s *Store) ExportJSON(w io.Writer) error {
	doc, err := s.Export()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// ImportOptions controls Import.
type ImportOptions struct {
	// Replace soft-deletes local tasks that are absent from the document,
	// making the store mirror it. Without Replace, the import merges.
	Replace bool
}

// ImportResult summarizes an import.
type ImportResult struct {
	OperationID int64 `json:"operation_id,omitempty"`
	Inserted    int   `json:"inserted"`
	Updated     int   `json:"updated"`
	Skipped     int   `json:"skipped"`
	Deleted     int   `json:"deleted"`
}

// ReadExport decodes and validates an export document.
func ReadExport(r io.Reader) (*ExportDocument, error) {
	var doc ExportDocument
	dec := json.NewDecoder(r)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidImportFile, err)
	}
	if doc.Format != ExportFormat {
		return nil, fmt.Errorf("%w: format %q is not %q", ErrInvalidImportFile, doc.Format, ExportFormat)
	}
	if doc.FormatVersion < 1 || doc.FormatVersion > ExportFormatVersion {
		return nil, fmt.Errorf("%w: format_version %d is not supported (max %d)", ErrInvalidImportFile, doc.FormatVersion, ExportFormatVersion)
	}
	seen := map[string]bool{}
	for i := range doc.Tasks {
		t := &doc.Tasks[i]
		if t.ID == "" {
			return nil, fmt.Errorf("%w: task #%d has no id", ErrInvalidImportFile, i+1)
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("%w: duplicate task id %s", ErrInvalidImportFile, t.ID)
		}
		seen[t.ID] = true
		if err := validateTask(t); err != nil {
			return nil, fmt.Errorf("%w: task %s: %v", ErrInvalidImportFile, t.ID, err)
		}
		if t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() {
			return nil, fmt.Errorf("%w: task %s is missing created_at/updated_at", ErrInvalidImportFile, t.ID)
		}
		if t.Version < 1 {
			t.Version = 1
		}
	}
	return &doc, nil
}

// Import merges (or, with Replace, mirrors) an export document into the
// store in one transaction. Existing tasks are overwritten only when the
// imported copy is newer. The whole import is a single undoable operation.
func (s *Store) Import(doc *ExportDocument, opts ImportOptions) (*ImportResult, error) {
	res := &ImportResult{}
	inDoc := map[string]bool{}
	for _, t := range doc.Tasks {
		inDoc[t.ID] = true
	}
	opID, err := s.write("import", fmt.Sprintf("import %d task(s)", len(doc.Tasks)), func(x *txn) error {
		inserted := map[string]bool{}
		for i := range doc.Tasks {
			in := doc.Tasks[i]
			in.Tags = slices.Clone(in.Tags)
			cur, err := x.get(in.ID)
			switch {
			case err == nil:
				if !in.UpdatedAt.After(cur.UpdatedAt) {
					res.Skipped++
					continue
				}
				x.remember(in.ID, cur)
				in.Version = max(in.Version, cur.Version) + 1
				if err := x.put(&in); err != nil {
					return err
				}
				if err := x.history(in.ID, "import", diffTasks(cur, &in)); err != nil {
					return err
				}
				res.Updated++
			case isNotFound(err):
				x.remember(in.ID, nil)
				if err := x.put(&in); err != nil {
					return err
				}
				inserted[in.ID] = true
				res.Inserted++
			default:
				return err
			}
		}
		// Carry over the change log of tasks that are new to this store.
		for _, h := range doc.History {
			if !inserted[h.TaskID] {
				continue
			}
			b, err := json.Marshal(h.Changes)
			if err != nil {
				return err
			}
			if _, err := x.tx.Exec(`INSERT INTO task_history(task_id, action, actor, changes, created_at) VALUES (?, ?, ?, ?, ?)`,
				h.TaskID, h.Action, h.Actor, string(b), fmtTime(h.CreatedAt)); err != nil {
				return err
			}
		}
		for id := range inserted {
			if err := x.history(id, "import", nil); err != nil {
				return err
			}
		}
		if opts.Replace {
			rows, err := x.tx.Query(`SELECT id FROM tasks WHERE deleted_at IS NULL`)
			if err != nil {
				return err
			}
			var stale []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				if !inDoc[id] {
					stale = append(stale, id)
				}
			}
			rows.Close()
			for _, id := range stale {
				t, err := x.get(id)
				if err != nil {
					return err
				}
				before := t.clone()
				t.DeletedAt = &x.now
				if _, err := x.save("import", before, t); err != nil {
					return err
				}
				res.Deleted++
			}
		}
		return x.checkParents()
	})
	if err != nil {
		return nil, err
	}
	res.OperationID = opID
	return res, nil
}

// checkParents verifies every parent reference resolves and is acyclic.
func (x *txn) checkParents() error {
	rows, err := x.tx.Query(`SELECT id, parent_id FROM tasks`)
	if err != nil {
		return err
	}
	parent := map[string]string{}
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		parent[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, p := range parent {
		steps := 0
		for cur := p; cur != ""; cur = parent[cur] {
			if _, ok := parent[cur]; !ok {
				return fmt.Errorf("%w: task %s references missing parent %s", ErrInvalidImportFile, id, cur)
			}
			if cur == id || steps > len(parent) {
				return fmt.Errorf("%w: parent cycle involving task %s", ErrInvalidImportFile, id)
			}
			steps++
		}
	}
	return nil
}
