// Package tui implements the interactive full-screen terminal interface of
// todo-cli: a task list and a detail pane driven by the keyboard and, where
// the terminal reports it, the mouse. All data goes through packages/core.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Options configures an App.
type Options struct {
	Store  *core.Store
	Keymap *Keymap
	// Warning is shown in the status line on start (e.g. a keymap error).
	Warning string
	// Mouse enables mouse reporting; keyboard paths work either way.
	Mouse bool
	// Color enables ANSI colours (disabled for NO_COLOR).
	Color bool
	Now   func() time.Time
	// ParseDue parses due times the same way as the non-interactive CLI.
	ParseDue func(string, time.Time) (time.Time, error)
}

type mode int

const (
	modeNormal mode = iota
	modeSearch
	modePrompt
	modeForm
	modePalette
	modeMenu
	modeHelp
	modeConfirm
)

type pane int

const (
	paneList pane = iota
	paneDetail
)

type statusTab struct {
	label    string
	statuses []core.Status
}

var tabs = []statusTab{
	{"全部", nil},
	{"待办", []core.Status{core.StatusTodo}},
	{"进行中", []core.Status{core.StatusInProgress}},
	{"已完成", []core.Status{core.StatusDone}},
	{"已归档", []core.Status{core.StatusArchived}},
}

var sortCycle = []core.SortField{core.SortManual, core.SortDue, core.SortPriority, core.SortCreated, core.SortUpdated}

var sortLabels = map[core.SortField]string{
	core.SortManual: "手动", core.SortDue: "截止时间", core.SortPriority: "优先级",
	core.SortCreated: "创建时间", core.SortUpdated: "更新时间",
}

var statusLabels = map[core.Status]string{
	core.StatusTodo: "待办", core.StatusInProgress: "进行中", core.StatusDone: "已完成", core.StatusArchived: "已归档",
}

var priorityLabels = []string{"无", "低", "中", "高", "紧急"}

// App is the TUI state machine. It is driven by HandleEvent and drawn by
// Render; Run connects it to a real terminal.
type App struct {
	store    *core.Store
	keys     *Keymap
	now      func() time.Time
	parseDue func(string, time.Time) (time.Time, error)
	mouse    bool

	w, h int

	tasks  []core.Task
	cursor int
	selID  string
	top    int
	focus  pane

	detail       detailData
	detailScroll int

	// filters
	tab         int
	query       string
	minPriority *core.Priority
	tag         string
	category    string
	overdue     bool
	sort        core.SortField
	reverse     bool

	mode     mode
	prompt   *prompt
	form     *form
	palette  *palette
	menu     *menu
	confirm  *confirm
	helpTop  int
	searchEd *lineEditor
	preQuery string

	msg    string
	msgErr bool

	clock       func() time.Time // wall clock for double-click detection
	lastClick   time.Time
	lastClickID string
	dataVersion int64
	vocab       *vocabulary
	regions     []region
	quit        bool
}

type detailData struct {
	id       string
	version  int64
	children []core.Task
	history  []core.HistoryEntry
	parent   string
}

type prompt struct {
	label    string
	hint     string
	ed       *lineEditor
	submit   func(string) error
	complete func(tok string) []string
}

type confirm struct {
	title string
	msg   string
	yes   string
	onYes func()
}

type vocabulary struct {
	tags       []string
	categories []string
}

// New builds an App for a w×h terminal and loads the task list.
func New(o Options, w, h int) (*App, error) {
	if o.Store == nil {
		return nil, errors.New("tui: no store")
	}
	if o.Keymap == nil {
		o.Keymap = DefaultKeymap()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ParseDue == nil {
		o.ParseDue = func(s string, now time.Time) (time.Time, error) {
			return time.ParseInLocation("2006-01-02 15:04", s, now.Location())
		}
	}
	a := &App{store: o.Store, keys: o.Keymap, now: o.Now, parseDue: o.ParseDue, mouse: o.Mouse,
		w: w, h: h, sort: core.SortManual, clock: time.Now}
	if v, err := a.store.DataVersion(); err == nil {
		a.dataVersion = v
	}
	if err := a.reload(); err != nil {
		return nil, err
	}
	if o.Warning != "" {
		a.warn(o.Warning)
	} else {
		a.flash(fmt.Sprintf("按 %s 查看快捷键 · %s 打开命令面板", a.keyHint(ActHelp), a.keyHint(ActPalette)))
	}
	return a, nil
}

// Quit reports whether the user asked to leave.
func (a *App) Quit() bool { return a.quit }

// Tasks returns the currently listed tasks.
func (a *App) Tasks() []core.Task { return a.tasks }

// Selected returns the selected task, if any.
func (a *App) Selected() *core.Task {
	if a.cursor >= 0 && a.cursor < len(a.tasks) {
		return &a.tasks[a.cursor]
	}
	return nil
}

// Message returns the current status-line message.
func (a *App) Message() string { return a.msg }

func (a *App) flash(msg string) { a.msg, a.msgErr = msg, false }

func (a *App) warn(msg string) { a.msg, a.msgErr = msg, true }

func (a *App) fail(err error) {
	a.msg, a.msgErr = describeError(err), true
}

// describeError turns store errors into actionable messages.
func describeError(err error) string {
	msg := err.Error()
	for _, code := range []error{core.ErrInvalid, core.ErrNotFound, core.ErrConflict, core.ErrDeleted,
		core.ErrAmbiguousID, core.ErrNothingToUndo} {
		msg = strings.TrimPrefix(msg, code.Error()+": ")
	}
	switch {
	case errors.Is(err, core.ErrConflict):
		return "冲突：任务已在别处被修改 — " + msg
	case errors.Is(err, core.ErrNotFound):
		return "未找到：" + msg
	case errors.Is(err, core.ErrDeleted):
		return "任务已删除：" + msg
	case errors.Is(err, core.ErrNothingToUndo):
		return "没有可撤销的操作"
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrAmbiguousID):
		return "输入有误：" + msg
	}
	return "错误：" + msg
}

func (a *App) filter() core.Filter {
	f := core.Filter{Query: a.query, Statuses: tabs[a.tab].statuses, MinPriority: a.minPriority,
		Category: a.category, Overdue: a.overdue, Sort: a.sort, Reverse: a.reverse}
	if a.tag != "" {
		f.Tags = []string{a.tag}
	}
	return f
}

func (a *App) filtersActive() bool {
	return a.query != "" || a.tab != 0 || a.minPriority != nil || a.tag != "" || a.category != "" || a.overdue
}

// reload re-reads the list, keeping the selection on the same task.
func (a *App) reload() error {
	tasks, err := a.store.List(a.filter())
	if err != nil {
		return err
	}
	a.tasks = tasks
	a.vocab = nil
	if i := slices.IndexFunc(tasks, func(t core.Task) bool { return t.ID == a.selID }); i >= 0 {
		a.cursor = i
	}
	a.selectIndex(a.cursor)
	a.detail = detailData{}
	return a.loadDetail()
}

func (a *App) mustReload() {
	if err := a.reload(); err != nil {
		a.fail(err)
	}
}

// selectIndex is the single place where the selection changes, so the
// keyboard cursor and mouse selection can never disagree.
func (a *App) selectIndex(i int) {
	if len(a.tasks) == 0 {
		a.cursor, a.selID, a.top = 0, "", 0
		return
	}
	i = max(0, min(i, len(a.tasks)-1))
	if a.tasks[i].ID != a.selID {
		a.detailScroll = 0
	}
	a.cursor, a.selID = i, a.tasks[i].ID
	a.scrollToCursor()
	if err := a.loadDetail(); err != nil {
		a.fail(err)
	}
}

func (a *App) scrollToCursor() {
	h := a.listHeight()
	if a.cursor < a.top {
		a.top = a.cursor
	}
	if a.cursor >= a.top+h {
		a.top = a.cursor - h + 1
	}
	a.top = max(0, min(a.top, max(len(a.tasks)-h, 0)))
}

func (a *App) loadDetail() error {
	t := a.Selected()
	if t == nil {
		a.detail = detailData{}
		return nil
	}
	if a.detail.id == t.ID && a.detail.version == t.Version {
		return nil
	}
	parentID := t.ID
	children, err := a.store.List(core.Filter{ParentID: &parentID, IncludeArchived: true})
	if err != nil {
		return err
	}
	hist, err := a.store.History(t.ID)
	if err != nil {
		return err
	}
	d := detailData{id: t.ID, version: t.Version, children: children, history: hist}
	if t.ParentID != "" {
		if p, err := a.store.Get(t.ParentID); err == nil {
			d.parent = p.Title
		}
	}
	a.detail = d
	return nil
}

// Resize adapts the layout to a new terminal size.
func (a *App) Resize(w, h int) {
	a.w, a.h = w, h
	a.scrollToCursor()
}

// HandleEvent applies one input event.
func (a *App) HandleEvent(ev Event) {
	switch ev.Kind {
	case EvResize:
		a.Resize(ev.W, ev.H)
		return
	case EvTick:
		a.checkExternalChanges()
		return
	case EvMouse:
		a.handleMouse(ev)
		return
	}
	if a.mode != modeNormal && ev.Key == "ctrl+c" {
		ev.Key = "esc" // ctrl+c cancels any open input or dialog
	}
	switch a.mode {
	case modeSearch:
		a.handleSearchKey(ev)
	case modePrompt:
		a.handlePromptKey(ev)
	case modeForm:
		a.handleFormKey(ev)
	case modePalette:
		a.handlePaletteKey(ev)
	case modeMenu:
		a.handleMenuKey(ev)
	case modeHelp:
		a.handleHelpKey(ev)
	case modeConfirm:
		a.handleConfirmKey(ev)
	default:
		a.handleNormalKey(ev)
	}
}

// checkExternalChanges reloads when another process (CLI, web) changed data.
func (a *App) checkExternalChanges() {
	v, err := a.store.DataVersion()
	if err != nil || v == a.dataVersion {
		return
	}
	a.dataVersion = v
	a.mustReload()
	if a.form != nil && a.form.taskID != "" {
		if t, err := a.store.Get(a.form.taskID); err == nil && t.Version != a.form.version {
			a.form.err = fmt.Sprintf("提示：该任务刚在别处被修改（版本 %d → %d）。保存时会提示冲突。", a.form.version, t.Version)
		}
	}
}

func (a *App) handleNormalKey(ev Event) {
	if ev.Kind == EvPaste {
		a.openNewTask(ev.Text, "")
		return
	}
	act, ok := a.keys.Lookup(ev.Key)
	if !ok {
		if len(ev.Key) == 1 && ev.Key >= "1" && ev.Key <= "5" {
			a.setTab(int(ev.Key[0] - '1'))
			return
		}
		a.warn(fmt.Sprintf("按键 %s 未绑定任何操作 — 按 %s 查看快捷键", ev.Key, a.keyHint(ActHelp)))
		return
	}
	a.do(act)
}

func (a *App) keyHint(act Action) string {
	if k := a.keys.Key(act); k != "" {
		return k
	}
	return ":" + string(act)
}

// do runs an action against the current selection.
func (a *App) do(act Action) {
	switch act {
	case ActUp:
		a.move(-1)
	case ActDown:
		a.move(1)
	case ActTop:
		if a.focus == paneDetail {
			a.detailScroll = 0
		} else {
			a.selectIndex(0)
		}
	case ActBottom:
		if a.focus == paneDetail {
			a.detailScroll = 1 << 20
		} else {
			a.selectIndex(len(a.tasks) - 1)
		}
	case ActPageUp:
		a.move(-max(a.listHeight()-1, 1))
	case ActPageDown:
		a.move(max(a.listHeight()-1, 1))
	case ActToggleFocus:
		if a.focus == paneList {
			a.openDetail()
		} else {
			a.focus = paneList
		}
	case ActOpenDetail:
		a.openDetail()
	case ActBack:
		switch {
		case a.focus == paneDetail:
			a.focus = paneList
		case a.filtersActive():
			a.clearFilters()
		default:
			a.flash(fmt.Sprintf("按 %s 退出", a.keyHint(ActQuit)))
		}
	case ActNew:
		a.openNewTask("", "")
	case ActNewSubtask:
		if t := a.need(); t != nil {
			a.openNewTask("", t.ID)
		}
	case ActEdit:
		if t := a.need(); t != nil {
			a.openForm(t)
		}
	case ActToggleDone:
		if t := a.need(); t != nil {
			if t.Status == core.StatusDone {
				a.apply("已重新打开", a.store.Reopen, t.ID)
			} else {
				a.apply("已完成", a.store.Complete, t.ID)
			}
		}
	case ActStart:
		if t := a.need(); t != nil {
			if t.Status == core.StatusInProgress {
				a.apply("已移回待办", a.store.Reopen, t.ID)
			} else {
				a.apply("已开始", a.store.Start, t.ID)
			}
		}
	case ActArchive:
		if t := a.need(); t != nil {
			if t.Status == core.StatusArchived {
				a.apply("已取消归档", a.store.Reopen, t.ID)
			} else {
				a.apply("已归档", a.store.Archive, t.ID)
			}
		}
	case ActDelete:
		if t := a.need(); t != nil {
			id, title := t.ID, t.Title
			a.askConfirm("删除任务", fmt.Sprintf("确定删除「%s」吗？删除后可按 %s 撤销。", title, a.keyHint(ActUndo)), "删除", func() {
				a.apply("已删除", a.store.Delete, id)
			})
		}
	case ActPriorityUp, ActPriorityDown:
		if t := a.need(); t != nil {
			p := t.Priority + 1
			if act == ActPriorityDown {
				p = t.Priority - 1
			}
			if p < core.PriorityNone || p > core.PriorityUrgent {
				a.flash("优先级已到边界：" + priorityLabels[t.Priority])
				return
			}
			a.apply("优先级 → "+priorityLabels[p], func(ids ...string) (*core.Result, error) {
				return a.store.SetPriority(p, ids...)
			}, t.ID)
		}
	case ActMoveUp, ActMoveDown:
		a.reorder(act == ActMoveUp)
	case ActSearch:
		a.openSearch()
	case ActFilterMenu:
		a.openFilterMenu(-1, -1)
	case ActNextTab:
		a.setTab((a.tab + 1) % len(tabs))
	case ActPrevTab:
		a.setTab((a.tab + len(tabs) - 1) % len(tabs))
	case ActClearFilters:
		a.clearFilters()
	case ActSort:
		i := slices.Index(sortCycle, a.sort)
		a.setSort(sortCycle[(i+1)%len(sortCycle)], a.reverse)
	case ActReverse:
		a.setSort(a.sort, !a.reverse)
	case ActUndo:
		res, err := a.store.Undo()
		if err != nil {
			a.fail(err)
			return
		}
		if len(res.Restored) > 0 {
			a.selID = res.Restored[0].ID // bring the restored task back into focus
		}
		a.mustReload()
		a.flash("已撤销：" + res.Operation.Summary)
	case ActReload:
		a.mustReload()
		a.flash(fmt.Sprintf("已重新加载 %d 个任务", len(a.tasks)))
	case ActPalette:
		a.openPalette()
	case ActMenu:
		a.openContextMenu(-1, -1)
	case ActHelp:
		a.mode, a.helpTop = modeHelp, 0
	case ActQuit:
		a.quit = true
	}
}

// need returns the selected task or explains that there is none.
func (a *App) need() *core.Task {
	t := a.Selected()
	if t == nil {
		a.warn(fmt.Sprintf("没有选中的任务 — 按 %s 新建", a.keyHint(ActNew)))
	}
	return t
}

func (a *App) move(delta int) {
	if a.focus == paneDetail {
		a.detailScroll = max(a.detailScroll+delta, 0)
		return
	}
	a.selectIndex(a.cursor + delta)
}

func (a *App) openDetail() {
	if a.need() == nil {
		return
	}
	a.focus = paneDetail
}

// apply runs a store mutation on ids, reloads and reports the outcome.
func (a *App) apply(verb string, fn func(...string) (*core.Result, error), ids ...string) {
	res, err := fn(ids...)
	if err != nil {
		a.fail(err)
		a.mustReload()
		return
	}
	a.mustReload()
	if len(res.Tasks) == 1 {
		a.flash(fmt.Sprintf("%s：%s — 按 %s 撤销", verb, res.Tasks[0].Title, a.keyHint(ActUndo)))
	} else {
		a.flash(fmt.Sprintf("%s %d 个任务 — 按 %s 撤销", verb, res.Changed, a.keyHint(ActUndo)))
	}
}

func (a *App) reorder(up bool) {
	t := a.need()
	if t == nil {
		return
	}
	if a.sort != core.SortManual {
		a.warn(fmt.Sprintf("手动调整顺序只在「手动」排序下可用 — 按 %s 切换排序", a.keyHint(ActSort)))
		return
	}
	var pl core.Placement
	switch {
	case up && a.cursor > 0:
		pl.Before = a.tasks[a.cursor-1].ID
	case !up && a.cursor < len(a.tasks)-1:
		pl.After = a.tasks[a.cursor+1].ID
	default:
		a.flash("已经在边界")
		return
	}
	if _, err := a.store.Reorder(t.ID, pl); err != nil {
		a.fail(err)
		return
	}
	a.mustReload()
	a.flash("已调整顺序：" + t.Title)
}

func (a *App) setTab(i int) {
	if i < 0 || i >= len(tabs) {
		return
	}
	a.tab = i
	a.mustReload()
	a.flash(fmt.Sprintf("筛选：%s（%d 项）", tabs[i].label, len(a.tasks)))
}

func (a *App) setSort(f core.SortField, reverse bool) {
	a.sort, a.reverse = f, reverse
	a.mustReload()
	dir := ""
	if reverse {
		dir = "（反向）"
	}
	a.flash("排序：" + sortLabels[f] + dir)
}

func (a *App) clearFilters() {
	a.query, a.tab, a.minPriority, a.tag, a.category, a.overdue = "", 0, nil, "", "", false
	a.mustReload()
	a.flash("已清除搜索与筛选")
}

// --- search -----------------------------------------------------------------

func (a *App) openSearch() {
	a.mode = modeSearch
	a.preQuery = a.query
	a.searchEd = newLineEditor(a.query)
	a.focus = paneList
}

func (a *App) handleSearchKey(ev Event) {
	switch ev.Key {
	case "enter":
		a.mode = modeNormal
		a.flash(fmt.Sprintf("搜索「%s」：%d 项 — %s 清除", a.query, len(a.tasks), a.keyHint(ActBack)))
		if a.query == "" {
			a.flash("已清除搜索")
		}
		return
	case "esc":
		a.mode = modeNormal
		a.query = a.preQuery
		a.mustReload()
		a.flash("已取消搜索")
		return
	case "up", "down":
		a.move(map[string]int{"up": -1, "down": 1}[ev.Key])
		return
	case "tab":
		a.completeInto(a.searchEd, a.searchCandidates)
		a.setQuery(a.searchEd.String())
		return
	}
	if a.searchEd.handle(ev) {
		a.setQuery(a.searchEd.String())
	}
}

func (a *App) setQuery(q string) {
	a.query = strings.TrimSpace(q)
	a.mustReload()
}

func (a *App) searchCandidates(tok string) []string {
	v := a.vocabulary()
	return append(slices.Clone(v.tags), v.categories...)
}

// --- prompts (new task, tag filter, ...) -------------------------------------

func (a *App) openPrompt(p *prompt) {
	a.prompt = p
	a.mode = modePrompt
}

func (a *App) handlePromptKey(ev Event) {
	p := a.prompt
	switch ev.Key {
	case "esc":
		a.mode, a.prompt = modeNormal, nil
		a.flash("已取消")
		return
	case "enter":
		if err := p.submit(p.ed.String()); err != nil {
			a.fail(err)
			return // keep the prompt open so the input can be fixed
		}
		if a.prompt == p {
			a.mode, a.prompt = modeNormal, nil
		}
		return
	case "tab":
		if p.complete != nil {
			a.completeInto(p.ed, p.complete)
		}
		return
	}
	p.ed.handle(ev)
}

// completeInto completes the token at the cursor from candidates; with
// several matches it extends to their common prefix and lists them.
func (a *App) completeInto(ed *lineEditor, candidates func(string) []string) {
	tok := ed.lastToken()
	prefix := ""
	for _, p := range []string{"#", "@", "!", "due:", "^"} {
		if strings.HasPrefix(tok, p) {
			prefix, tok = p, strings.TrimPrefix(tok, p)
			break
		}
	}
	matches := completions(candidates(prefix+tok), tok)
	switch len(matches) {
	case 0:
		a.flash("没有可补全的内容")
	case 1:
		ed.completeToken(prefix + matches[0] + " ")
		a.flash("")
	default:
		ed.completeToken(prefix + commonPrefix(matches))
		a.flash("补全：" + strings.Join(matches[:min(len(matches), 8)], "  "))
	}
}

func completions(cands []string, tok string) []string {
	var out []string
	low := strings.ToLower(tok)
	for _, c := range cands {
		if strings.HasPrefix(strings.ToLower(c), low) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

func commonPrefix(ss []string) string {
	p := []rune(ss[0])
	for _, s := range ss[1:] {
		r := []rune(s)
		n := 0
		for n < len(p) && n < len(r) && p[n] == r[n] {
			n++
		}
		p = p[:n]
	}
	return string(p)
}

func (a *App) vocabulary() *vocabulary {
	if a.vocab != nil {
		return a.vocab
	}
	v := &vocabulary{}
	all, err := a.store.List(core.Filter{IncludeArchived: true})
	if err == nil {
		for _, t := range all {
			for _, tag := range t.Tags {
				if !slices.Contains(v.tags, tag) {
					v.tags = append(v.tags, tag)
				}
			}
			if t.Category != "" && !slices.Contains(v.categories, t.Category) {
				v.categories = append(v.categories, t.Category)
			}
		}
	}
	slices.Sort(v.tags)
	slices.Sort(v.categories)
	a.vocab = v
	return v
}

var dueWords = []string{"today", "tomorrow", "+1d", "+3d", "+1w", "mon", "tue", "wed", "thu", "fri", "sat", "sun",
	"next", "今天", "明天", "后天", "下周一", "周五", "none"}

var priorityWords = []string{"none", "low", "medium", "high", "urgent"}

// quickCandidates completes the quick-add syntax: #tag, @category, !priority, due:when.
func (a *App) quickCandidates(tok string) []string {
	v := a.vocabulary()
	switch {
	case strings.HasPrefix(tok, "#"):
		return v.tags
	case strings.HasPrefix(tok, "@"):
		return v.categories
	case strings.HasPrefix(tok, "!"):
		return priorityWords
	case strings.HasPrefix(tok, "due:"), strings.HasPrefix(tok, "^"):
		return dueWords
	}
	return nil
}

func (a *App) openNewTask(initial, parentID string) {
	label := "新任务"
	if parentID != "" {
		if t := a.Selected(); t != nil {
			label = "子任务（父：" + truncate(t.Title, 16) + "）"
		}
	}
	a.openPrompt(&prompt{
		label:    label,
		hint:     "#标签 @分类 !优先级 due:时间 · tab 补全 · enter 创建 · esc 取消",
		ed:       newLineEditor(strings.TrimSpace(initial)),
		complete: a.quickCandidates,
		submit: func(s string) error {
			in, err := parseQuickAdd(s, a.now(), a.parseDue)
			if err != nil {
				return err
			}
			in.ParentID = parentID
			return a.create(in)
		},
	})
}

func (a *App) create(in core.NewTask) error {
	t, err := a.store.Create(in)
	if err != nil {
		return err
	}
	// Make sure the new task is visible: drop filters that would hide it.
	a.selID = t.ID
	a.mustReload()
	if a.Selected() == nil || a.Selected().ID != t.ID {
		a.query, a.tab, a.minPriority, a.tag, a.category, a.overdue = "", 0, nil, "", "", false
		a.mustReload()
	}
	a.flash(fmt.Sprintf("已创建：%s — 按 %s 撤销", t.Title, a.keyHint(ActUndo)))
	return nil
}

// parseQuickAdd splits "title #tag @category !high due:tomorrow" into a
// NewTask. Bare words form the title.
func parseQuickAdd(s string, now time.Time, parseDue func(string, time.Time) (time.Time, error)) (core.NewTask, error) {
	var in core.NewTask
	var title []string
	for _, tok := range strings.Fields(s) {
		switch {
		case len(tok) > 1 && tok[0] == '#':
			in.Tags = append(in.Tags, tok[1:])
		case len(tok) > 1 && tok[0] == '@':
			in.Category = tok[1:]
		case tok[0] == '!':
			p, err := parseBangPriority(tok)
			if err != nil {
				return in, err
			}
			in.Priority = p
		case strings.HasPrefix(tok, "due:") || (len(tok) > 1 && tok[0] == '^'):
			v := strings.TrimPrefix(strings.TrimPrefix(tok, "due:"), "^")
			d, err := parseDue(v, now)
			if err != nil {
				return in, fmt.Errorf("%w: 截止时间 %v", core.ErrInvalid, err)
			}
			in.DueAt = &d
		default:
			title = append(title, tok)
		}
	}
	in.Title = strings.Join(title, " ")
	if in.Title == "" {
		return in, fmt.Errorf("%w: 标题不能为空", core.ErrInvalid)
	}
	in.Tags = core.NormalizeTags(in.Tags)
	return in, nil
}

func parseBangPriority(tok string) (core.Priority, error) {
	switch strings.Trim(tok, "!") {
	case "":
		switch len(tok) {
		case 1:
			return core.PriorityMedium, nil
		case 2:
			return core.PriorityHigh, nil
		default:
			return core.PriorityUrgent, nil
		}
	}
	p, err := core.ParsePriority(tok[1:])
	if err != nil {
		return 0, fmt.Errorf("%w: 优先级 %q 无效（可用 !low !medium !high !urgent 或 ! !! !!!）", core.ErrInvalid, tok)
	}
	return p, nil
}

// --- confirm ------------------------------------------------------------------

func (a *App) askConfirm(title, msg, yes string, onYes func()) {
	a.confirm = &confirm{title: title, msg: msg, yes: yes, onYes: onYes}
	a.mode = modeConfirm
}

func (a *App) handleConfirmKey(ev Event) {
	switch strings.ToLower(ev.Key) {
	case "y", "enter":
		a.answerConfirm(true)
	case "n", "esc", "q":
		a.answerConfirm(false)
	}
}

func (a *App) answerConfirm(yes bool) {
	c := a.confirm
	a.mode, a.confirm = modeNormal, nil
	if yes {
		c.onYes()
	} else {
		a.flash("已取消")
	}
}

// --- help -----------------------------------------------------------------------

func (a *App) handleHelpKey(ev Event) {
	if act, ok := a.keys.Lookup(ev.Key); ok && (act == ActHelp || act == ActQuit) {
		a.mode = modeNormal
		return
	}
	switch ev.Key {
	case "esc", "q", "enter":
		a.mode = modeNormal
	case "up", "k":
		a.helpTop = max(a.helpTop-1, 0)
	case "down", "j":
		a.helpTop++
	case "pgup", "ctrl+u":
		a.helpTop = max(a.helpTop-(a.h-6), 0)
	case "pgdown", "ctrl+d", "space":
		a.helpTop += a.h - 6
	case "home", "g":
		a.helpTop = 0
	case "end", "G":
		a.helpTop = 1 << 20
	}
}
