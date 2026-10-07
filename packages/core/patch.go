package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// PatchFields lists the JSON fields DecodePatch accepts.
var PatchFields = []string{"title", "description", "notes", "category", "parent_id", "due_at", "priority",
	"tags", "add_tags", "remove_tags", "status", "version"}

// DecodePatch parses a JSON task edit as sent by the web UI and API clients:
//
//	{"version": 3, "title": "…", "due_at": "2026-10-09T18:00:00+08:00" | null, "priority": "high",
//	 "tags": ["a"], "add_tags": [], "remove_tags": [], "status": "done", …}
//
// Absent fields are left untouched; "due_at": null (or "") clears the due
// time; "version" becomes ExpectedVersion. Unknown fields are rejected.
func DecodePatch(raw []byte) (TaskPatch, error) {
	var p TaskPatch
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return p, fmt.Errorf("%w: patch must be a JSON object: %v", ErrInvalid, err)
	}
	if fields == nil {
		return p, fmt.Errorf("%w: patch must be a JSON object", ErrInvalid)
	}
	isNull := func(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }
	for name, v := range fields {
		var err error
		switch name {
		case "title", "description", "notes", "category", "parent_id":
			var str string
			if isNull(v) {
				if name == "title" {
					err = fmt.Errorf("title cannot be null")
					break
				}
			} else if err = json.Unmarshal(v, &str); err != nil {
				break
			}
			switch name {
			case "title":
				p.Title = &str
			case "description":
				p.Description = &str
			case "notes":
				p.Notes = &str
			case "category":
				p.Category = &str
			case "parent_id":
				p.ParentID = &str
			}
		case "due_at":
			var str string
			if !isNull(v) {
				if err = json.Unmarshal(v, &str); err != nil {
					break
				}
			}
			if strings.TrimSpace(str) == "" {
				p.ClearDue = true
				break
			}
			var due time.Time
			if due, err = ParseTimeInput(str, true); err == nil {
				p.DueAt = &due
			}
		case "priority":
			var pr Priority
			if err = json.Unmarshal(v, &pr); err == nil {
				p.Priority = &pr
			}
		case "tags":
			var tags []string
			if !isNull(v) {
				err = json.Unmarshal(v, &tags)
			}
			if tags == nil {
				tags = []string{}
			}
			p.Tags = &tags
		case "add_tags":
			err = json.Unmarshal(v, &p.AddTags)
		case "remove_tags":
			err = json.Unmarshal(v, &p.RemoveTags)
		case "status":
			var str string
			if err = json.Unmarshal(v, &str); err == nil {
				var st Status
				if st, err = ParseStatus(str); err == nil {
					p.Status = &st
				}
			}
		case "version":
			if err = json.Unmarshal(v, &p.ExpectedVersion); err == nil && p.ExpectedVersion < 1 {
				err = fmt.Errorf("must be a positive integer")
			}
		default:
			return p, fmt.Errorf("%w: unknown field %q (allowed: %s)", ErrInvalid, name, strings.Join(PatchFields, ", "))
		}
		if err != nil {
			if strings.HasPrefix(err.Error(), ErrInvalid.Error()) {
				return p, err
			}
			return p, fmt.Errorf("%w: field %q: %v", ErrInvalid, name, err)
		}
	}
	return p, nil
}

// PatchedFields names the task fields a patch touches, sorted.
func PatchedFields(p TaskPatch) []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("title", p.Title != nil)
	add("description", p.Description != nil)
	add("notes", p.Notes != nil)
	add("category", p.Category != nil)
	add("parent_id", p.ParentID != nil)
	add("due_at", p.DueAt != nil || p.ClearDue)
	add("priority", p.Priority != nil)
	add("tags", p.Tags != nil || len(p.AddTags) > 0 || len(p.RemoveTags) > 0)
	add("status", p.Status != nil)
	slices.Sort(out)
	return out
}

// ParseTimeInput accepts RFC 3339 and the local-time forms
// "2006-01-02T15:04", "2006-01-02 15:04" and "2006-01-02". A bare date
// means the end of that day when endOfDay is set, else its start.
func ParseTimeInput(s string, endOfDay bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC(), nil
		}
	}
	if d, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if endOfDay {
			d = time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, d.Location())
		}
		return d.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%w: invalid time %q (use RFC 3339 or YYYY-MM-DD[ HH:MM])", ErrInvalid, s)
}
