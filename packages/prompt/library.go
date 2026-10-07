package prompt

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

var migrations = []string{
	`
CREATE TABLE prompt_templates (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL UNIQUE,
	original        TEXT NOT NULL,
	current_version INTEGER NOT NULL,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE TABLE prompt_versions (
	template_id TEXT NOT NULL REFERENCES prompt_templates(id) ON DELETE CASCADE,
	version     INTEGER NOT NULL,
	summary     TEXT NOT NULL,
	body        TEXT NOT NULL,
	source      TEXT NOT NULL,
	note        TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	PRIMARY KEY (template_id, version)
);
`,
}

// Version sources.
const (
	SourceModel    = "llm"      // summarised by the model
	SourceManual   = "manual"   // written or edited by the user
	SourceRollback = "rollback" // copy of an earlier version
	SourceImport   = "import"
	SourceCopy     = "copy" // duplicated from another template
)

// Version is one immutable state of a template.
type Version struct {
	Version   int       `json:"version"`
	Summary   Summary   `json:"summary"`
	Body      string    `json:"body"`
	Source    string    `json:"source"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Template is a saved prompt with its original text and versions.
type Template struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Original string `json:"original"`
	// Current is the version in use; Latest is its content.
	Current   int       `json:"current_version"`
	Latest    *Version  `json:"latest,omitempty"`
	Versions  []Version `json:"versions,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Library stores templates in the task database.
type Library struct {
	store *core.Store
	// Redact hides secrets before anything is stored.
	Redact func(string) string
}

// Open prepares the prompt tables.
func Open(store *core.Store) (*Library, error) {
	if err := store.MigrateModule("prompt", migrations); err != nil {
		return nil, err
	}
	return &Library{store: store, Redact: func(s string) string { return s }}, nil
}

func (l *Library) db() *sql.DB { return l.store.DB() }

func (l *Library) now() string { return l.store.Now().Format(time.RFC3339Nano) }

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "p" + hex.EncodeToString(b[:])
}

// Create saves a new template: the original prompt plus version 1.
func (l *Library) Create(name, original string, sum Summary, source string) (*Template, error) {
	if err := sum.Normalize(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultName(sum.Goal)
	}
	original = l.Redact(original)
	sum = l.redactSummary(sum)
	id := newID()
	now := l.now()
	tx, err := l.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taken int
	if err := tx.QueryRow(`SELECT count(*) FROM prompt_templates WHERE name = ?`, name).Scan(&taken); err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, fmt.Errorf("%w: a template named %q already exists (use --name or edit it)", ErrInvalid, name)
	}
	if _, err := tx.Exec(`INSERT INTO prompt_templates(id, name, original, current_version, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, original, now, now); err != nil {
		return nil, err
	}
	if err := insertVersion(tx, id, 1, sum, sum.Body(), source, "", now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.Get(id)
}

func defaultName(goal string) string {
	r := []rune(strings.Join(strings.Fields(goal), " "))
	if len(r) > 30 {
		r = r[:30]
	}
	return string(r) + " " + time.Now().UTC().Format("0102-150405")
}

func (l *Library) redactSummary(s Summary) Summary {
	s.Goal, s.Context, s.OutputFormat = l.Redact(s.Goal), l.Redact(s.Context), l.Redact(s.OutputFormat)
	for i := range s.Constraints {
		s.Constraints[i] = l.Redact(s.Constraints[i])
	}
	for i := range s.Steps {
		s.Steps[i] = l.Redact(s.Steps[i])
	}
	return s
}

func insertVersion(tx *sql.Tx, id string, v int, sum Summary, body, source, note, now string) error {
	b, err := json.Marshal(sum)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO prompt_versions(template_id, version, summary, body, source, note, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, v, string(b), body, source, note, now)
	return err
}

// Get finds a template by id, unique id prefix or exact name, with its
// current version.
func (l *Library) Get(ref string) (*Template, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: empty template reference", ErrInvalid)
	}
	rows, err := l.db().Query(`SELECT id, name, original, current_version, created_at, updated_at FROM prompt_templates
		WHERE id = ? OR name = ? OR id LIKE ? ORDER BY (id = ? OR name = ?) DESC LIMIT 2`, ref, ref, ref+"%", ref, ref)
	if err != nil {
		return nil, err
	}
	var found []Template
	for rows.Next() {
		var t Template
		var created, updated string
		if err := rows.Scan(&t.ID, &t.Name, &t.Original, &t.Current, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		found = append(found, t)
	}
	rows.Close()
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: prompt template %q", core.ErrNotFound, ref)
	}
	if len(found) > 1 && found[0].ID != ref && found[0].Name != ref {
		return nil, fmt.Errorf("%w: %q matches several templates", core.ErrAmbiguousID, ref)
	}
	t := found[0]
	v, err := l.Version(t.ID, t.Current)
	if err != nil {
		return nil, err
	}
	t.Latest = v
	return &t, nil
}

// List returns every template with its current version, newest first.
func (l *Library) List() ([]Template, error) {
	rows, err := l.db().Query(`SELECT id FROM prompt_templates ORDER BY updated_at DESC, name`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Template{}
	for _, id := range ids {
		t, err := l.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

// Version returns one version of a template (0 = current).
func (l *Library) Version(id string, v int) (*Version, error) {
	if v == 0 {
		t, err := l.Get(id)
		if err != nil {
			return nil, err
		}
		return t.Latest, nil
	}
	var ver Version
	var sum, created string
	err := l.db().QueryRow(`SELECT version, summary, body, source, note, created_at FROM prompt_versions WHERE template_id = ? AND version = ?`, id, v).
		Scan(&ver.Version, &sum, &ver.Body, &ver.Source, &ver.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: template %s has no version %d", core.ErrNotFound, id, v)
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sum), &ver.Summary); err != nil {
		return nil, err
	}
	ver.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &ver, nil
}

// Versions lists every version, oldest first.
func (l *Library) Versions(id string) ([]Version, error) {
	t, err := l.Get(id)
	if err != nil {
		return nil, err
	}
	out := []Version{}
	for v := 1; ; v++ {
		ver, err := l.Version(t.ID, v)
		if errors.Is(err, core.ErrNotFound) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *ver)
	}
}

// Edit stores a new version. body overrides the text generated from the
// summary (manual wording); its placeholders become variables.
func (l *Library) Edit(ref string, sum Summary, body *string, source, note string) (*Template, error) {
	t, err := l.Get(ref)
	if err != nil {
		return nil, err
	}
	if err := sum.Normalize(); err != nil {
		return nil, err
	}
	sum = l.redactSummary(sum)
	text := sum.Body()
	if body != nil && strings.TrimSpace(*body) != "" {
		text = l.Redact(*body)
		for _, name := range Placeholders(text) {
			if !containsVar(sum.Variables, name) {
				sum.Variables = append(sum.Variables, Variable{Name: name})
			}
		}
	}
	return l.addVersion(t, sum, text, source, note)
}

func containsVar(vars []Variable, name string) bool {
	for _, v := range vars {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (l *Library) addVersion(t *Template, sum Summary, body, source, note string) (*Template, error) {
	tx, err := l.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var last int
	if err := tx.QueryRow(`SELECT max(version) FROM prompt_versions WHERE template_id = ?`, t.ID).Scan(&last); err != nil {
		return nil, err
	}
	now := l.now()
	if err := insertVersion(tx, t.ID, last+1, sum, body, source, note, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE prompt_templates SET current_version = ?, updated_at = ? WHERE id = ?`, last+1, now, t.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.Get(t.ID)
}

// Rollback makes an earlier version current again as a new version, so the
// versions in between stay available.
func (l *Library) Rollback(ref string, v int) (*Template, error) {
	t, err := l.Get(ref)
	if err != nil {
		return nil, err
	}
	if v == 0 {
		v = t.Current - 1
	}
	if v < 1 {
		return nil, fmt.Errorf("%w: template %s has no earlier version", ErrInvalid, t.Name)
	}
	old, err := l.Version(t.ID, v)
	if err != nil {
		return nil, err
	}
	return l.addVersion(t, old.Summary, old.Body, SourceRollback, fmt.Sprintf("rollback to v%d", v))
}

// Rename changes a template's name.
func (l *Library) Rename(ref, name string) (*Template, error) {
	t, err := l.Get(ref)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrInvalid)
	}
	if _, err := l.db().Exec(`UPDATE prompt_templates SET name = ?, updated_at = ? WHERE id = ?`, name, l.now(), t.ID); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: a template named %q already exists", ErrInvalid, name)
		}
		return nil, err
	}
	return l.Get(t.ID)
}

// Delete removes a template and all its versions.
func (l *Library) Delete(ref string) error {
	t, err := l.Get(ref)
	if err != nil {
		return err
	}
	_, err = l.db().Exec(`DELETE FROM prompt_templates WHERE id = ?`, t.ID)
	return err
}

// Export formats.
const (
	FormatMarkdown = "md"
	FormatJSON     = "json"
	FormatText     = "txt"
)

// Export renders a template version: md (body with a metadata header),
// txt (body only) or json (the template with its original and every
// version, for transfer between entry points and agents).
func (l *Library) Export(ref string, v int, format string) ([]byte, error) {
	t, err := l.Get(ref)
	if err != nil {
		return nil, err
	}
	ver := t.Latest
	if v != 0 {
		if ver, err = l.Version(t.ID, v); err != nil {
			return nil, err
		}
	}
	switch format {
	case "", FormatMarkdown, "markdown":
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n<!-- todo-cli prompt template %s v%d (%s) -->\n\n%s", t.Name, t.ID, ver.Version, ver.Source, ver.Body)
		if vars := VariablesText(ver.Summary.Variables); vars != "" {
			fmt.Fprintf(&b, "\n## 变量\n%s\n", vars)
		}
		return []byte(b.String()), nil
	case FormatText, "text":
		return []byte(ver.Body), nil
	case FormatJSON:
		full := *t
		if full.Versions, err = l.Versions(t.ID); err != nil {
			return nil, err
		}
		return json.MarshalIndent(map[string]any{"format": "todo-cli/prompt", "format_version": 1, "template": full, "exported_version": ver.Version}, "", "  ")
	}
	return nil, fmt.Errorf("%w: unknown export format %q (md, txt, json)", ErrInvalid, format)
}

// AgentTask builds a task that hands the rendered template to an agent:
// the description is the final prompt, tagged "agent".
func AgentTask(t *Template, ver *Version, rendered, title string) core.NewTask {
	if strings.TrimSpace(title) == "" {
		title = "智能体任务：" + t.Name
	}
	return core.NewTask{Title: title, Description: rendered, Tags: []string{"agent", "prompt"},
		Notes: fmt.Sprintf("提示词模板 %s v%d (%s)", t.Name, ver.Version, t.ID)}
}

// Copy duplicates a template (its original and the given version, 0 =
// current) under a new name; the copy starts its own version history.
func (l *Library) Copy(ref string, v int, name string) (*Template, error) {
	t, err := l.Get(ref)
	if err != nil {
		return nil, err
	}
	ver := t.Latest
	if v != 0 {
		if ver, err = l.Version(t.ID, v); err != nil {
			return nil, err
		}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = t.Name + " 副本"
	}
	tx, err := l.db().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, now := newID(), l.now()
	if _, err := tx.Exec(`INSERT INTO prompt_templates(id, name, original, current_version, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, t.Original, now, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: a template named %q already exists", ErrInvalid, name)
		}
		return nil, err
	}
	if err := insertVersion(tx, id, 1, ver.Summary, ver.Body, SourceCopy, fmt.Sprintf("copy of %s v%d", t.Name, ver.Version), now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.Get(id)
}
