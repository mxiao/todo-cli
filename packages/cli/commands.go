package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// strFlag registers a string flag under several names (e.g. --desc and -d).
func strFlag(fs *flag.FlagSet, usage string, names ...string) *string {
	p := new(string)
	for _, n := range names {
		fs.StringVar(p, n, "", usage)
	}
	return p
}

func listFlag(fs *flag.FlagSet, usage string, names ...string) *stringList {
	p := new(stringList)
	for _, n := range names {
		fs.Var(p, n, usage)
	}
	return p
}

func anySet(fs *flag.FlagSet, names ...string) bool {
	for _, n := range names {
		if flagSet(fs, n) {
			return true
		}
	}
	return false
}

func cmdAdd(a *app, args []string) error {
	fs := newFlags("add")
	desc := strFlag(fs, "description", "desc", "d")
	due := strFlag(fs, "due time: today, tomorrow, +3d, fri, 下周一, 2026-10-07, \"2026-10-07 18:00\"", "due")
	prio := strFlag(fs, "priority: none|low|medium|high|urgent", "priority", "p")
	tags := listFlag(fs, "tag (repeatable or comma separated)", "tag", "t")
	cat := strFlag(fs, "category", "category", "c")
	parent := strFlag(fs, "parent task id", "parent")
	notes := strFlag(fs, "notes", "notes", "n")
	status := strFlag(fs, "initial status: todo|in_progress|done|archived", "status")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	in := core.NewTask{Title: strings.Join(pos, " "), Description: *desc, Tags: *tags, Category: *cat, Notes: *notes}
	if strings.TrimSpace(in.Title) == "" {
		return usagef("add: a title is required")
	}
	if *due != "" {
		d, err := parseDue(*due, a.now())
		if err != nil {
			return usagef("add: %v", err)
		}
		in.DueAt = &d
	}
	if in.Priority, err = core.ParsePriority(*prio); err != nil {
		return err
	}
	if *status != "" {
		if in.Status, err = core.ParseStatus(*status); err != nil {
			return err
		}
	}
	if *parent != "" {
		if in.ParentID, err = a.resolveOne(*parent); err != nil {
			return err
		}
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	t, err := s.Create(in)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task": t})
	}
	a.printf("Created %s\n", a.line(t))
	return nil
}

func cmdEdit(a *app, args []string) error {
	fs := newFlags("edit")
	title := strFlag(fs, "new title", "title")
	desc := strFlag(fs, "description", "desc", "d")
	due := strFlag(fs, "due time (see `todo add -h`)", "due")
	noDue := fs.Bool("no-due", false, "clear the due time")
	prio := strFlag(fs, "priority: none|low|medium|high|urgent", "priority", "p")
	setTags := listFlag(fs, "replace all tags", "tags")
	addTags := listFlag(fs, "add tag (repeatable)", "tag", "t")
	rmTags := listFlag(fs, "remove tag (repeatable)", "untag")
	cat := strFlag(fs, "category (empty clears)", "category", "c")
	parent := strFlag(fs, "parent task id (none clears)", "parent")
	notes := strFlag(fs, "replace notes", "notes", "n")
	appendNotes := strFlag(fs, "append a line to notes", "append-notes")
	status := strFlag(fs, "status: todo|in_progress|done|archived", "status")
	ifVersion := fs.Int64("if-version", 0, "fail with a conflict unless the task is at this version")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("edit: expected exactly one task id")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	p := core.TaskPatch{AddTags: *addTags, RemoveTags: *rmTags, ExpectedVersion: *ifVersion}
	if flagSet(fs, "title") {
		p.Title = title
	}
	if anySet(fs, "desc", "d") {
		p.Description = desc
	}
	if anySet(fs, "category", "c") {
		p.Category = cat
	}
	if anySet(fs, "notes", "n") {
		p.Notes = notes
	}
	if flagSet(fs, "tags") {
		v := []string(*setTags)
		p.Tags = &v
	}
	p.ClearDue = *noDue
	if *due != "" {
		d, err := parseDue(*due, a.now())
		if err != nil {
			return usagef("edit: %v", err)
		}
		p.DueAt = &d
	}
	if anySet(fs, "priority", "p") {
		v, err := core.ParsePriority(*prio)
		if err != nil {
			return err
		}
		p.Priority = &v
	}
	if *status != "" {
		v, err := core.ParseStatus(*status)
		if err != nil {
			return err
		}
		p.Status = &v
	}
	if flagSet(fs, "parent") {
		v := ""
		if *parent != "" && *parent != "none" {
			if v, err = a.resolveOne(*parent); err != nil {
				return err
			}
		}
		p.ParentID = &v
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	if *appendNotes != "" {
		base := *notes
		if p.Notes == nil {
			cur, err := s.Get(id)
			if err != nil {
				return err
			}
			base = cur.Notes
		}
		joined := strings.TrimLeft(base+"\n"+*appendNotes, "\n")
		p.Notes = &joined
	}
	t, err := s.Update(id, p)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task": t})
	}
	a.printf("Updated %s\n", a.line(t))
	return nil
}

func cmdShow(a *app, args []string) error {
	fs := newFlags("show")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("show: expected exactly one task id")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	s := a.store
	t, err := s.Get(id)
	if err != nil {
		return err
	}
	children, err := s.List(core.Filter{ParentID: &id, IncludeArchived: true})
	if err != nil {
		return err
	}
	hist, err := s.History(id)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task": t, "children": children, "history": hist})
	}
	a.printDetail(t)
	if len(children) > 0 {
		a.printf("\nSubtasks:\n")
		for i := range children {
			a.printf("  %s\n", a.line(&children[i]))
		}
	}
	a.printHistory(hist)
	return nil
}

func cmdHistory(a *app, args []string) error {
	fs := newFlags("history")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("history: expected exactly one task id")
	}
	id, err := a.resolveOne(pos[0])
	if err != nil {
		return err
	}
	hist, err := a.store.History(id)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task_id": id, "history": hist})
	}
	a.printHistory(hist)
	return nil
}

// filterFlags registers list/search filters and returns a builder.
func filterFlags(fs *flag.FlagSet) func(a *app) (core.Filter, error) {
	statuses := listFlag(fs, "status filter: todo,in_progress,done,archived or all", "status", "s")
	prios := listFlag(fs, "priority filter, e.g. high,urgent", "priority", "p")
	minPrio := strFlag(fs, "minimum priority", "min-priority")
	tags := listFlag(fs, "require tag (repeatable; all must match)", "tag", "t")
	cat := strFlag(fs, "category", "category", "c")
	parent := strFlag(fs, "only subtasks of this task id (none = top level)", "parent")
	dueBefore := strFlag(fs, "due on or before (date or time)", "due-before")
	dueAfter := strFlag(fs, "due on or after (date or time)", "due-after")
	overdue := fs.Bool("overdue", false, "only open tasks past their due time")
	hasDue := fs.Bool("has-due", false, "only tasks with a due time")
	noDue := fs.Bool("no-due", false, "only tasks without a due time")
	query := strFlag(fs, "keyword search", "search", "q")
	sortBy := strFlag(fs, "sort: manual|due|priority|created|updated", "sort")
	reverse := fs.Bool("reverse", false, "reverse the sort order")
	all := fs.Bool("all", false, "include archived tasks")
	deleted := fs.Bool("deleted", false, "only deleted tasks")
	limit := fs.Int("limit", 0, "maximum number of tasks")
	return func(a *app) (core.Filter, error) {
		f := core.Filter{Category: *cat, Query: *query, Overdue: *overdue, IncludeArchived: *all,
			OnlyDeleted: *deleted, Reverse: *reverse, Limit: *limit, Tags: *tags}
		var err error
		for _, v := range *statuses {
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
		for _, v := range *prios {
			p, err := core.ParsePriority(v)
			if err != nil {
				return f, err
			}
			f.Priorities = append(f.Priorities, p)
		}
		if *minPrio != "" {
			p, err := core.ParsePriority(*minPrio)
			if err != nil {
				return f, err
			}
			f.MinPriority = &p
		}
		if f.Sort, err = core.ParseSortField(*sortBy); err != nil {
			return f, err
		}
		if *dueBefore != "" {
			t, dateOnly, err := parseWhen(*dueBefore, a.now())
			if err != nil {
				return f, usagef("--due-before: %v", err)
			}
			if dateOnly {
				t = endOfDay(t)
			}
			f.DueBefore = &t
		}
		if *dueAfter != "" {
			t, _, err := parseWhen(*dueAfter, a.now())
			if err != nil {
				return f, usagef("--due-after: %v", err)
			}
			f.DueAfter = &t
		}
		if *hasDue && *noDue {
			return f, usagef("--has-due and --no-due are mutually exclusive")
		}
		if *hasDue || *noDue {
			v := *hasDue
			f.HasDue = &v
		}
		if *parent != "" {
			v := ""
			if *parent != "none" {
				if v, err = a.resolveOne(*parent); err != nil {
					return f, err
				}
			}
			f.ParentID = &v
		}
		return f, nil
	}
}

func cmdList(a *app, args []string) error { return runList(a, "list", args) }

func cmdSearch(a *app, args []string) error { return runList(a, "search", args) }

func runList(a *app, name string, args []string) error {
	fs := newFlags(name)
	build := filterFlags(fs)
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if name == "list" && len(pos) > 0 {
		return usagef("list: unexpected argument %q (use `todo search` or --search for keywords)", pos[0])
	}
	f, err := build(a)
	if err != nil {
		return err
	}
	if name == "search" {
		f.Query = strings.TrimSpace(f.Query + " " + strings.Join(pos, " "))
		if f.Query == "" {
			return usagef("search: keywords are required")
		}
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	tasks, err := s.List(f)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"count": len(tasks), "tasks": tasks})
	}
	if len(tasks) == 0 {
		a.printf("No tasks.\n")
		return nil
	}
	for i := range tasks {
		a.printf("%s\n", a.line(&tasks[i]))
	}
	a.printf("%d task(s)\n", len(tasks))
	return nil
}

func statusCmd(fn func(*core.Store, ...string) (*core.Result, error), verb string) func(*app, []string) error {
	return func(a *app, args []string) error {
		fs := newFlags(strings.ToLower(verb))
		pos, err := a.parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return usagef("%s: at least one task id is required", strings.ToLower(verb))
		}
		ids, err := a.resolve(pos)
		if err != nil {
			return err
		}
		res, err := fn(a.store, ids...)
		if err != nil {
			return err
		}
		return a.printResult(verb, res)
	}
}

func cmdPriority(a *app, args []string) error {
	fs := newFlags("priority")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("priority: usage: todo priority <level> <id>...")
	}
	p, err := core.ParsePriority(pos[0])
	if err != nil {
		return err
	}
	ids, err := a.resolve(pos[1:])
	if err != nil {
		return err
	}
	res, err := a.store.SetPriority(p, ids...)
	if err != nil {
		return err
	}
	return a.printResult("Set priority "+p.String()+" on", res)
}

func cmdMove(a *app, args []string) error {
	fs := newFlags("move")
	cat := strFlag(fs, "move into category (empty clears)", "category", "c")
	parent := strFlag(fs, "move under parent task id (none clears)", "parent")
	before := strFlag(fs, "place before task id (manual order)", "before")
	after := strFlag(fs, "place after task id (manual order)", "after")
	top := fs.Bool("top", false, "move to the top of the manual order")
	bottom := fs.Bool("bottom", false, "move to the bottom of the manual order")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("move: at least one task id is required")
	}
	ids, err := a.resolve(pos)
	if err != nil {
		return err
	}
	refile := anySet(fs, "category", "c", "parent")
	reorder := *before != "" || *after != "" || *top || *bottom
	if refile == reorder {
		return usagef("move: use either --category/--parent or one of --before/--after/--top/--bottom")
	}
	if refile {
		var t core.MoveTarget
		if anySet(fs, "category", "c") {
			t.Category = cat
		}
		if flagSet(fs, "parent") {
			v := ""
			if *parent != "" && *parent != "none" {
				if v, err = a.resolveOne(*parent); err != nil {
					return err
				}
			}
			t.ParentID = &v
		}
		res, err := a.store.Move(t, ids...)
		if err != nil {
			return err
		}
		return a.printResult("Moved", res)
	}
	if len(ids) != 1 {
		return usagef("move: reordering takes exactly one task id")
	}
	pl := core.Placement{Top: *top, Bottom: *bottom}
	if *before != "" {
		if pl.Before, err = a.resolveOne(*before); err != nil {
			return err
		}
	}
	if *after != "" {
		if pl.After, err = a.resolveOne(*after); err != nil {
			return err
		}
	}
	t, err := a.store.Reorder(ids[0], pl)
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"task": t})
	}
	a.printf("Reordered %s\n", a.line(t))
	return nil
}

func cmdBatch(a *app, args []string) error {
	if len(args) == 0 {
		return usagef("batch: an action is required (done|start|reopen|archive|delete|restore|priority|move)")
	}
	switch args[0] {
	case "done", "complete", "start", "reopen", "archive", "delete", "rm", "restore", "priority", "prio", "move", "mv":
		return findCommand(args[0]).run(a, args[1:])
	}
	return usagef("batch: unknown action %q", args[0])
}

func cmdUndo(a *app, args []string) error {
	fs := newFlags("undo")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	res, err := s.Undo()
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(res)
	}
	a.printf("Undid %q (%s by %s)\n", res.Operation.Summary, res.Operation.Kind, res.Operation.Actor)
	for i := range res.Restored {
		a.printf("  restored %s\n", a.line(&res.Restored[i]))
	}
	for _, id := range res.Removed {
		a.printf("  removed  %s\n", shortID(id))
	}
	return nil
}

func cmdExport(a *app, args []string) error {
	fs := newFlags("export")
	out := strFlag(fs, "write to file instead of stdout", "output", "o")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	if *out == "" || *out == "-" {
		return s.ExportJSON(a.env.Stdout)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := s.ExportJSON(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"path": *out})
	}
	a.printf("Exported to %s\n", *out)
	return nil
}

func cmdImport(a *app, args []string) error {
	fs := newFlags("import")
	replace := fs.Bool("replace", false, "delete local tasks missing from the file (soft delete, undoable)")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("import: expected one file path (or - for stdin)")
	}
	var r io.Reader = a.env.Stdin
	if pos[0] != "-" {
		f, err := os.Open(pos[0])
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	doc, err := core.ReadExport(r)
	if err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	res, err := s.Import(doc, core.ImportOptions{Replace: *replace})
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(res)
	}
	a.printf("Imported: %d inserted, %d updated, %d skipped, %d deleted\n", res.Inserted, res.Updated, res.Skipped, res.Deleted)
	if res.OperationID != 0 {
		a.printf("Undo with `todo undo`\n")
	}
	return nil
}

func cmdBackup(a *app, args []string) error {
	fs := newFlags("backup")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	p, err := s.Backup("manual")
	if err != nil {
		return err
	}
	if a.json {
		return a.emitJSON(map[string]any{"path": p})
	}
	a.printf("Backup written to %s\n", p)
	return nil
}

func cmdVersion(a *app, _ []string) error {
	if a.json {
		return a.emitJSON(map[string]any{"version": Version})
	}
	a.printf("todo %s\n", Version)
	return nil
}

func cmdHelp(a *app, args []string) error {
	if len(args) > 0 {
		c := findCommand(args[0])
		if c == nil {
			return usagef("unknown command %q", args[0])
		}
		return c.run(a, []string{"-h"})
	}
	w := a.env.Stdout
	fmt.Fprintf(w, "todo %s — local task manager\n\nusage: todo [--json] [--data-dir DIR] <command> [args]\n\ncommands:\n", Version)
	for _, c := range commands {
		name := c.name
		if len(c.aliases) > 0 {
			name += " (" + strings.Join(c.aliases, ", ") + ")"
		}
		fmt.Fprintf(w, "  %-24s %s\n", name, c.summary)
	}
	fmt.Fprintf(w, "\nglobal flags:\n  --json            machine-readable JSON output\n  --data-dir DIR    data directory (default $%s or ~/.todo-cli)\n", core.EnvHome)
	fmt.Fprintf(w, "\nTask ids may be shortened to any unique prefix. Run `todo help <command>` for flags.\n")
	return nil
}

func (a *app) printResult(verb string, res *core.Result) error {
	if a.json {
		return a.emitJSON(res)
	}
	a.printf("%s %d task(s)", verb, res.Changed)
	if n := len(res.Tasks) - res.Changed; n > 0 {
		a.printf(", %d already up to date", n)
	}
	if res.Changed > 0 {
		a.printf(" — undo with `todo undo`")
	}
	a.printf("\n")
	for i := range res.Tasks {
		a.printf("  %s\n", a.line(&res.Tasks[i]))
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

var statusMarks = map[core.Status]string{
	core.StatusTodo: "[ ]", core.StatusInProgress: "[~]", core.StatusDone: "[x]", core.StatusArchived: "[a]",
}

// line renders a one-line task summary.
func (a *app) line(t *core.Task) string {
	var b strings.Builder
	b.WriteString(statusMarks[t.Status])
	b.WriteString(" " + shortID(t.ID) + "  ")
	if t.Priority != core.PriorityNone {
		b.WriteString("!" + t.Priority.String() + " ")
	}
	b.WriteString(t.Title)
	if t.DueAt != nil {
		b.WriteString("  due " + a.localTime(*t.DueAt))
		if (t.Status == core.StatusTodo || t.Status == core.StatusInProgress) && t.DueAt.Before(a.now()) {
			b.WriteString(" (overdue)")
		}
	}
	for _, tag := range t.Tags {
		b.WriteString("  #" + tag)
	}
	if t.Category != "" {
		b.WriteString("  @" + t.Category)
	}
	if t.ParentID != "" {
		b.WriteString("  ↳" + shortID(t.ParentID))
	}
	if t.Deleted() {
		b.WriteString("  (deleted)")
	}
	return b.String()
}

func (a *app) printDetail(t *core.Task) {
	row := func(k, v string) {
		if v != "" {
			a.printf("%-12s %s\n", k+":", v)
		}
	}
	row("ID", t.ID)
	row("Title", t.Title)
	row("Status", string(t.Status))
	row("Priority", t.Priority.String())
	if t.DueAt != nil {
		row("Due", a.localTime(*t.DueAt))
	}
	row("Tags", strings.Join(t.Tags, ", "))
	row("Category", t.Category)
	row("Parent", t.ParentID)
	row("Description", t.Description)
	row("Notes", t.Notes)
	row("Created", a.localTime(t.CreatedAt))
	row("Updated", a.localTime(t.UpdatedAt))
	if t.CompletedAt != nil {
		row("Completed", a.localTime(*t.CompletedAt))
	}
	if t.ArchivedAt != nil {
		row("Archived", a.localTime(*t.ArchivedAt))
	}
	if t.DeletedAt != nil {
		row("Deleted", a.localTime(*t.DeletedAt))
	}
	row("Version", strconv.FormatInt(t.Version, 10))
}

func (a *app) printHistory(hist []core.HistoryEntry) {
	if len(hist) == 0 {
		return
	}
	a.printf("\nHistory:\n")
	for _, h := range hist {
		var fields []string
		for k := range h.Changes {
			fields = append(fields, k)
		}
		slices.Sort(fields)
		a.printf("  %s  %-10s by %-6s %s\n", a.localTime(h.CreatedAt), h.Action, h.Actor, strings.Join(fields, ", "))
	}
}

func (a *app) localTime(t time.Time) string {
	return t.In(a.now().Location()).Format("2006-01-02 15:04")
}
