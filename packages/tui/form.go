package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// form edits all fields of one task in place.
type form struct {
	taskID  string
	version int64
	title   string
	fields  []*formField
	focus   int
	err     string
	// force is set after a version conflict was shown: the next save
	// deliberately overwrites the newer version.
	force bool
}

type formField struct {
	key      string
	label    string
	ed       *lineEditor
	complete func(tok string) []string
}

const (
	fieldTitle    = "title"
	fieldDesc     = "description"
	fieldDue      = "due"
	fieldPriority = "priority"
	fieldTags     = "tags"
	fieldCategory = "category"
	fieldNotes    = "notes"
)

func (a *App) openForm(t *core.Task) {
	due := ""
	if t.DueAt != nil {
		due = t.DueAt.In(a.now().Location()).Format("2006-01-02 15:04")
	}
	v := func(tok string) []string { return a.vocabulary().tags }
	f := &form{taskID: t.ID, version: t.Version, title: t.Title, fields: []*formField{
		{key: fieldTitle, label: "标题", ed: newLineEditor(t.Title)},
		{key: fieldDesc, label: "描述", ed: newLineEditor(t.Description)},
		{key: fieldDue, label: "截止", ed: newLineEditor(due), complete: func(string) []string { return dueWords }},
		{key: fieldPriority, label: "优先级", ed: newLineEditor(t.Priority.String()), complete: func(string) []string { return priorityWords }},
		{key: fieldTags, label: "标签", ed: newLineEditor(strings.Join(t.Tags, ", ")), complete: v},
		{key: fieldCategory, label: "分类", ed: newLineEditor(t.Category), complete: func(string) []string { return a.vocabulary().categories }},
		{key: fieldNotes, label: "备注", ed: newLineEditor(t.Notes)},
	}}
	a.form = f
	a.mode = modeForm
	a.flash("")
}

func (f *form) field(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return strings.TrimSpace(fl.ed.String())
		}
	}
	return ""
}

func (a *App) handleFormKey(ev Event) {
	f := a.form
	cur := f.fields[f.focus]
	switch ev.Key {
	case "esc":
		a.closeForm("已取消编辑")
		return
	case "ctrl+s":
		a.saveForm()
		return
	case "enter":
		if f.focus == len(f.fields)-1 {
			a.saveForm()
		} else {
			f.focus++
		}
		return
	case "up", "shift+tab":
		f.focus = (f.focus + len(f.fields) - 1) % len(f.fields)
		return
	case "down":
		f.focus = (f.focus + 1) % len(f.fields)
		return
	case "tab":
		// Tab completes when the field has suggestions for the word being
		// typed; otherwise it moves to the next field.
		if cur.complete != nil && cur.ed.lastToken() != "" {
			if m := completions(cur.complete(cur.ed.lastToken()), cur.ed.lastToken()); len(m) > 0 && !(len(m) == 1 && m[0] == cur.ed.lastToken()) {
				a.completeInto(cur.ed, cur.complete)
				return
			}
		}
		f.focus = (f.focus + 1) % len(f.fields)
		return
	}
	cur.ed.handle(ev)
}

func (a *App) closeForm(msg string) {
	a.form = nil
	a.mode = modeNormal
	a.flash(msg)
}

// formPatch converts the form into a patch, validating every field.
func (a *App) formPatch(f *form) (core.TaskPatch, error) {
	var p core.TaskPatch
	title := f.field(fieldTitle)
	if title == "" {
		return p, errors.New("标题不能为空")
	}
	desc, notes, cat := f.field(fieldDesc), f.field(fieldNotes), f.field(fieldCategory)
	p.Title, p.Description, p.Notes, p.Category = &title, &desc, &notes, &cat
	switch due := f.field(fieldDue); due {
	case "", "none", "无":
		p.ClearDue = true
	default:
		d, err := a.parseDue(due, a.now())
		if err != nil {
			return p, fmt.Errorf("截止时间：%v", err)
		}
		p.DueAt = &d
	}
	pr, err := core.ParsePriority(f.field(fieldPriority))
	if err != nil {
		return p, fmt.Errorf("优先级：可用 none / low / medium / high / urgent")
	}
	p.Priority = &pr
	tags := core.NormalizeTags(strings.FieldsFunc(f.field(fieldTags), func(r rune) bool { return r == ',' || r == ' ' || r == '，' }))
	p.Tags = &tags
	if !f.force {
		p.ExpectedVersion = f.version
	}
	return p, nil
}

func (a *App) saveForm() {
	f := a.form
	p, err := a.formPatch(f)
	if err != nil {
		f.err = err.Error()
		return
	}
	t, err := a.store.Update(f.taskID, p)
	if errors.Is(err, core.ErrConflict) {
		latest, gerr := a.store.Get(f.taskID)
		if gerr == nil {
			f.version = latest.Version
		}
		f.force = true
		f.err = "冲突：该任务已在别处被修改。你的输入仍保留：再次保存（ctrl+s）将覆盖，esc 放弃。"
		a.mustReload()
		return
	}
	if err != nil {
		f.err = describeError(err)
		return
	}
	a.selID = t.ID
	a.mustReload()
	a.closeForm(fmt.Sprintf("已保存：%s — 按 %s 撤销", t.Title, a.keyHint(ActUndo)))
}

// --- relative time labels ----------------------------------------------------------

func (a *App) dueLabel(d time.Time) string {
	now := a.now()
	loc := now.Location()
	d = d.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	clock := ""
	if d.Hour() != 23 || d.Minute() != 59 {
		clock = " " + d.Format("15:04")
	}
	switch int(day.Sub(today).Hours() / 24) {
	case 0:
		return "今天" + clock
	case 1:
		return "明天" + clock
	case -1:
		return "昨天" + clock
	}
	if d.Year() != now.Year() {
		return d.Format("2006-01-02")
	}
	return d.Format("01-02") + clock
}

func (a *App) overdueTask(t *core.Task) bool {
	return t.DueAt != nil && t.DueAt.Before(a.now()) && (t.Status == core.StatusTodo || t.Status == core.StatusInProgress)
}
