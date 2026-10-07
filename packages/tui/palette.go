package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mxiao/todo-cli/packages/core"
)

// paletteCmd is one command palette entry. Entries without run invoke the
// action of the same name; entries with usage take arguments.
type paletteCmd struct {
	name   string
	label  string
	usage  string
	action Action
	// bare runs instead of asking for arguments when none are given.
	bare     Action
	run      func(a *App, args []string) error
	complete func(a *App, args []string) []string
}

func paletteCommands() []paletteCmd {
	cmds := []paletteCmd{
		{name: "add", label: "新建任务（支持 #标签 @分类 !优先级 due:时间）", usage: "<标题>", run: cmdPaletteAdd, bare: ActNew,
			complete: func(a *App, args []string) []string { return a.quickCandidates(lastArg(args)) }},
		{name: "search", label: "按关键词搜索", usage: "<关键词>", bare: ActSearch, run: func(a *App, args []string) error {
			a.query = strings.Join(args, " ")
			a.mustReload()
			a.flash(fmt.Sprintf("搜索「%s」：%d 项", a.query, len(a.tasks)))
			return nil
		}},
		{name: "filter", label: "筛选：status / priority / tag / category / overdue / clear", usage: "<条件> [值]", bare: ActFilterMenu,
			run: cmdPaletteFilter, complete: completeFilter},
		{name: "sort", label: "排序：manual / due / priority / created / updated [reverse]", usage: "<字段> [reverse]", bare: ActSort,
			run: cmdPaletteSort, complete: func(a *App, args []string) []string {
				if len(args) <= 1 {
					return []string{"manual", "due", "priority", "created", "updated"}
				}
				return []string{"reverse"}
			}},
		{name: "priority", label: "设置选中任务的优先级", usage: "<none|low|medium|high|urgent>", run: cmdPalettePriority,
			complete: func(*App, []string) []string { return priorityWords }},
		{name: "due", label: "设置选中任务的截止时间（none 清除）", usage: "<时间>", run: cmdPaletteDue,
			complete: func(*App, []string) []string { return dueWords }},
		{name: "tag", label: "为选中任务添加标签", usage: "<标签...>", run: cmdPaletteTag(false),
			complete: func(a *App, _ []string) []string { return a.vocabulary().tags }},
		{name: "untag", label: "移除选中任务的标签", usage: "<标签...>", run: cmdPaletteTag(true),
			complete: func(a *App, _ []string) []string {
				if t := a.Selected(); t != nil {
					return t.Tags
				}
				return nil
			}},
		{name: "category", label: "设置选中任务的分类", usage: "<分类>", run: cmdPaletteCategory,
			complete: func(a *App, _ []string) []string { return a.vocabulary().categories }},
		{name: "goto", label: "按 ID 前缀跳转到任务", usage: "<id>", run: cmdPaletteGoto},
	}
	for _, ai := range actionTable {
		name := strings.ReplaceAll(string(ai.Action), "_", "-")
		if !slices.ContainsFunc(cmds, func(c paletteCmd) bool { return c.name == name }) {
			cmds = append(cmds, paletteCmd{name: name, label: ai.Label, action: ai.Action})
		}
	}
	return cmds
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func findPaletteCmd(name string) *paletteCmd {
	name = strings.ToLower(name)
	for _, c := range paletteCommands() {
		if c.name == name || strings.ReplaceAll(c.name, "-", "_") == name {
			return &c
		}
	}
	return nil
}

type palette struct {
	ed      *lineEditor
	matches []paletteCmd
	sel     int
	top     int
	err     string
}

func (a *App) openPalette() {
	a.palette = &palette{ed: newLineEditor("")}
	a.mode = modePalette
	a.updatePalette()
}

// updatePalette refilters the entries for the command word being typed.
func (a *App) updatePalette() {
	p := a.palette
	word, _, _ := strings.Cut(strings.TrimLeft(p.ed.String(), " "), " ")
	word = strings.ToLower(word)
	var prefix, contains []paletteCmd
	for _, c := range paletteCommands() {
		switch {
		case word == "" || strings.HasPrefix(c.name, word):
			prefix = append(prefix, c)
		case strings.Contains(c.name, word) || strings.Contains(strings.ToLower(c.label), word) || subsequence(c.name, word):
			contains = append(contains, c)
		}
	}
	p.matches = append(prefix, contains...)
	p.sel = min(p.sel, max(len(p.matches)-1, 0))
}

func subsequence(s, sub string) bool {
	i := 0
	for _, r := range s {
		if i < len(sub) && rune(sub[i]) == r {
			i++
		}
	}
	return i == len(sub)
}

func (a *App) handlePaletteKey(ev Event) {
	p := a.palette
	switch ev.Key {
	case "esc":
		a.closePalette()
		a.flash("已关闭命令面板")
		return
	case "up", "ctrl+k":
		p.sel = max(p.sel-1, 0)
		return
	case "down", "ctrl+j":
		p.sel = min(p.sel+1, max(len(p.matches)-1, 0))
		return
	case "tab":
		a.paletteComplete()
		return
	case "enter":
		a.runPalette()
		return
	}
	if p.ed.handle(ev) {
		p.err = ""
		a.updatePalette()
	}
}

func (a *App) closePalette() {
	a.palette = nil
	if a.mode == modePalette {
		a.mode = modeNormal
	}
}

func (a *App) paletteComplete() {
	p := a.palette
	input := strings.TrimLeft(p.ed.String(), " ")
	word, rest, hasArgs := strings.Cut(input, " ")
	if !hasArgs {
		if len(p.matches) == 0 {
			p.err = "没有匹配的命令"
			return
		}
		c := p.matches[p.sel]
		p.ed.set(c.name + " ")
		a.updatePalette()
		return
	}
	c := findPaletteCmd(word)
	if c == nil || c.complete == nil {
		p.err = "该命令没有可补全的参数"
		return
	}
	args := strings.Fields(rest)
	if strings.HasSuffix(rest, " ") || len(args) == 0 {
		args = append(args, "")
	}
	tok := p.ed.lastToken()
	prefix := ""
	if word == "add" {
		for _, pre := range []string{"#", "@", "!", "due:", "^"} {
			if strings.HasPrefix(tok, pre) {
				prefix, tok = pre, strings.TrimPrefix(tok, pre)
				break
			}
		}
	}
	matches := completions(c.complete(a, args), tok)
	switch len(matches) {
	case 0:
		p.err = "没有可补全的内容"
	case 1:
		p.ed.completeToken(prefix + matches[0] + " ")
		p.err = ""
	default:
		p.ed.completeToken(prefix + commonPrefix(matches))
		p.err = "可选：" + strings.Join(matches[:min(len(matches), 8)], "  ")
	}
}

func (a *App) runPalette() {
	p := a.palette
	input := strings.TrimSpace(p.ed.String())
	word, rest, hasArgs := strings.Cut(input, " ")
	if !hasArgs {
		if input != "" {
			if c := findPaletteCmd(word); c != nil {
				a.runPaletteCmd(c, nil)
				return
			}
		}
		if len(p.matches) == 0 {
			p.err = fmt.Sprintf("未知命令 %q%s", word, suggestPalette(word))
			return
		}
		a.runPaletteCmd(&p.matches[p.sel], nil)
		return
	}
	c := findPaletteCmd(word)
	if c == nil {
		p.err = fmt.Sprintf("未知命令 %q%s", word, suggestPalette(word))
		return
	}
	a.runPaletteCmd(c, strings.Fields(rest))
}

func (a *App) runPaletteCmd(c *paletteCmd, args []string) {
	p := a.palette
	if c.run == nil {
		a.closePalette()
		a.do(c.action)
		return
	}
	if len(args) == 0 && c.bare != "" {
		a.closePalette()
		a.do(c.bare)
		return
	}
	if len(args) == 0 {
		// The command needs arguments: put it in the input and show usage.
		p.ed.set(c.name + " ")
		p.err = "用法：" + c.name + " " + c.usage
		a.updatePalette()
		return
	}
	if err := c.run(a, args); err != nil {
		p.err = describeError(err)
		return
	}
	if a.mode == modePalette {
		a.closePalette()
	}
}

func suggestPalette(word string) string {
	best, bestD := "", 3
	for _, c := range paletteCommands() {
		if d := editDistance(word, c.name); d < bestD {
			best, bestD = c.name, d
		}
	}
	if best == "" {
		return " — 输入关键词查找命令"
	}
	return fmt.Sprintf("，是否想输入 %s？", best)
}

func cmdPaletteAdd(a *App, args []string) error {
	in, err := parseQuickAdd(strings.Join(args, " "), a.now(), a.parseDue)
	if err != nil {
		return err
	}
	return a.create(in)
}

func completeFilter(a *App, args []string) []string {
	if len(args) <= 1 {
		return []string{"status", "priority", "tag", "category", "overdue", "clear"}
	}
	switch args[0] {
	case "status":
		return []string{"all", "todo", "in_progress", "done", "archived"}
	case "priority":
		return priorityWords
	case "tag":
		return a.vocabulary().tags
	case "category":
		return a.vocabulary().categories
	}
	return nil
}

func cmdPaletteFilter(a *App, args []string) error {
	val := strings.Join(args[1:], " ")
	need := func() error {
		if val == "" {
			return fmt.Errorf("%w: filter %s 需要一个值", core.ErrInvalid, args[0])
		}
		return nil
	}
	switch args[0] {
	case "status":
		if err := need(); err != nil {
			return err
		}
		if val == "all" || val == "全部" {
			a.setTab(0)
			return nil
		}
		st, err := core.ParseStatus(val)
		if err != nil {
			return err
		}
		a.setTab(slices.IndexFunc(tabs, func(t statusTab) bool { return slices.Contains(t.statuses, st) }))
		return nil
	case "priority":
		if err := need(); err != nil {
			return err
		}
		p, err := core.ParsePriority(val)
		if err != nil {
			return err
		}
		if p == core.PriorityNone {
			a.minPriority = nil
		} else {
			a.minPriority = &p
		}
	case "tag":
		a.tag = strings.TrimPrefix(val, "#")
	case "category":
		a.category = val
	case "overdue":
		a.overdue = !a.overdue
	case "clear":
		a.clearFilters()
		return nil
	default:
		return fmt.Errorf("%w: 未知筛选条件 %q（status / priority / tag / category / overdue / clear）", core.ErrInvalid, args[0])
	}
	a.mustReload()
	a.flash(fmt.Sprintf("筛选：%s（%d 项）", a.filterSummary(), len(a.tasks)))
	return nil
}

func cmdPaletteSort(a *App, args []string) error {
	f, err := core.ParseSortField(args[0])
	if err != nil {
		return err
	}
	a.setSort(f, len(args) > 1 && (args[1] == "reverse" || args[1] == "desc"))
	return nil
}

func cmdPalettePriority(a *App, args []string) error {
	t := a.need()
	if t == nil {
		return fmt.Errorf("%w: 没有选中的任务", core.ErrInvalid)
	}
	p, err := core.ParsePriority(args[0])
	if err != nil {
		return err
	}
	a.apply("优先级 → "+priorityLabels[p], func(ids ...string) (*core.Result, error) { return a.store.SetPriority(p, ids...) }, t.ID)
	return nil
}

func (a *App) patchSelected(verb string, p core.TaskPatch) error {
	t := a.need()
	if t == nil {
		return fmt.Errorf("%w: 没有选中的任务", core.ErrInvalid)
	}
	p.ExpectedVersion = t.Version
	nt, err := a.store.Update(t.ID, p)
	if err != nil {
		a.mustReload()
		return err
	}
	a.mustReload()
	a.flash(fmt.Sprintf("%s：%s — 按 %s 撤销", verb, nt.Title, a.keyHint(ActUndo)))
	return nil
}

func cmdPaletteDue(a *App, args []string) error {
	v := strings.Join(args, " ")
	if v == "none" || v == "无" {
		return a.patchSelected("已清除截止时间", core.TaskPatch{ClearDue: true})
	}
	d, err := a.parseDue(v, a.now())
	if err != nil {
		return fmt.Errorf("%w: %v", core.ErrInvalid, err)
	}
	return a.patchSelected("截止 → "+a.dueLabel(d), core.TaskPatch{DueAt: &d})
}

func cmdPaletteTag(remove bool) func(a *App, args []string) error {
	return func(a *App, args []string) error {
		if remove {
			return a.patchSelected("已移除标签", core.TaskPatch{RemoveTags: args})
		}
		return a.patchSelected("已添加标签", core.TaskPatch{AddTags: args})
	}
}

func cmdPaletteCategory(a *App, args []string) error {
	c := strings.Join(args, " ")
	return a.patchSelected("分类 → "+c, core.TaskPatch{Category: &c})
}

func cmdPaletteGoto(a *App, args []string) error {
	id, err := a.store.ResolveID(args[0])
	if err != nil {
		return err
	}
	i := slices.IndexFunc(a.tasks, func(t core.Task) bool { return t.ID == id })
	if i < 0 {
		a.query, a.tab, a.minPriority, a.tag, a.category, a.overdue = "", 0, nil, "", "", false
		t, err := a.store.Get(id)
		if err != nil {
			return err
		}
		if t.Status == core.StatusArchived {
			a.tab = len(tabs) - 1
		}
		a.selID = id
		a.mustReload()
		i = slices.IndexFunc(a.tasks, func(t core.Task) bool { return t.ID == id })
		if i < 0 {
			return fmt.Errorf("%w: 任务 %s 已删除", core.ErrNotFound, args[0])
		}
	}
	a.selectIndex(i)
	a.focus = paneDetail
	a.flash("已跳转：" + a.tasks[i].Title)
	return nil
}

// --- menus ----------------------------------------------------------------------------

type menuItem struct {
	label string
	hint  string
	run   func()
}

type menu struct {
	title string
	x, y  int
	items []menuItem
	sel   int
}

func (a *App) actionItem(label string, act Action) menuItem {
	return menuItem{label: label, hint: a.keys.Key(act), run: func() { a.do(act) }}
}

// openContextMenu shows the task menu (also used for right clicks). x, y
// < 0 places it next to the selected row.
func (a *App) openContextMenu(x, y int) {
	t := a.Selected()
	if x < 0 {
		x, y = 4, a.bodyTop()+a.cursor-a.top+1
	}
	if t == nil {
		a.openGeneralMenu(x, y)
		return
	}
	done, start, archive := "完成", "开始", "归档"
	if t.Status == core.StatusDone {
		done = "重新打开"
	}
	if t.Status == core.StatusInProgress {
		start = "移回待办"
	}
	if t.Status == core.StatusArchived {
		archive = "取消归档"
	}
	a.menu = &menu{title: truncate(t.Title, 20), x: x, y: y, items: []menuItem{
		a.actionItem("打开详情", ActOpenDetail),
		a.actionItem("编辑", ActEdit),
		a.actionItem(done, ActToggleDone),
		a.actionItem(start, ActStart),
		a.actionItem("新建子任务", ActNewSubtask),
		a.actionItem("提高优先级", ActPriorityUp),
		a.actionItem("降低优先级", ActPriorityDown),
		a.actionItem(archive, ActArchive),
		a.actionItem("删除", ActDelete),
	}}
	a.mode = modeMenu
}

// openGeneralMenu is the right-click menu for empty space.
func (a *App) openGeneralMenu(x, y int) {
	a.menu = &menu{title: "操作", x: x, y: y, items: []menuItem{
		a.actionItem("新建任务", ActNew),
		a.actionItem("搜索", ActSearch),
		a.actionItem("筛选…", ActFilterMenu),
		a.actionItem("切换排序", ActSort),
		a.actionItem("命令面板", ActPalette),
		a.actionItem("快捷键帮助", ActHelp),
	}}
	a.mode = modeMenu
}

func (a *App) openFilterMenu(x, y int) {
	if x < 0 {
		x, y = 2, 1
	}
	mark := func(on bool, s string) string {
		if on {
			return "● " + s
		}
		return "  " + s
	}
	var items []menuItem
	for i, t := range tabs {
		i := i
		items = append(items, menuItem{label: mark(a.tab == i, "状态："+t.label), hint: fmt.Sprint(i + 1), run: func() { a.setTab(i) }})
	}
	high := core.PriorityHigh
	items = append(items,
		menuItem{label: mark(a.overdue, "仅显示逾期"), run: func() { _ = cmdPaletteFilter(a, []string{"overdue"}) }},
		menuItem{label: mark(a.minPriority != nil, "优先级 ≥ 高"), run: func() {
			if a.minPriority != nil {
				a.minPriority = nil
			} else {
				a.minPriority = &high
			}
			a.mustReload()
			a.flash(fmt.Sprintf("筛选：%s（%d 项）", a.filterSummary(), len(a.tasks)))
		}},
		menuItem{label: mark(a.tag != "", "按标签…"), run: func() { a.openFilterPrompt("tag", "按标签筛选", a.tag) }},
		menuItem{label: mark(a.category != "", "按分类…"), run: func() { a.openFilterPrompt("category", "按分类筛选", a.category) }},
		menuItem{label: "  清除全部筛选", hint: a.keys.Key(ActClearFilters), run: a.clearFilters},
	)
	a.menu = &menu{title: "筛选", x: x, y: y, items: items}
	a.mode = modeMenu
}

func (a *App) openFilterPrompt(kind, label, cur string) {
	a.openPrompt(&prompt{
		label: label,
		hint:  "tab 补全 · 留空清除 · enter 应用 · esc 取消",
		ed:    newLineEditor(cur),
		complete: func(string) []string {
			if kind == "tag" {
				return a.vocabulary().tags
			}
			return a.vocabulary().categories
		},
		submit: func(s string) error { return cmdPaletteFilter(a, []string{kind, strings.TrimSpace(s)}) },
	})
}

func (a *App) handleMenuKey(ev Event) {
	m := a.menu
	switch ev.Key {
	case "esc", "q", "m":
		a.closeMenu()
	case "up", "k", "shift+tab":
		m.sel = (m.sel + len(m.items) - 1) % len(m.items)
	case "down", "j", "tab":
		m.sel = (m.sel + 1) % len(m.items)
	case "enter", "space", "l", "right":
		a.runMenuItem(m.sel)
	default:
		// The hint key of an item runs it directly.
		for i, it := range m.items {
			if it.hint != "" && it.hint == ev.Key {
				a.runMenuItem(i)
				return
			}
		}
	}
}

func (a *App) closeMenu() {
	a.menu = nil
	if a.mode == modeMenu {
		a.mode = modeNormal
	}
}

func (a *App) runMenuItem(i int) {
	it := a.menu.items[i]
	a.closeMenu()
	it.run()
}

func (a *App) filterSummary() string {
	var parts []string
	if a.tab != 0 {
		parts = append(parts, "状态 "+tabs[a.tab].label)
	}
	if a.query != "" {
		parts = append(parts, "搜索「"+a.query+"」")
	}
	if a.minPriority != nil {
		parts = append(parts, "优先级 ≥ "+priorityLabels[*a.minPriority])
	}
	if a.tag != "" {
		parts = append(parts, "#"+a.tag)
	}
	if a.category != "" {
		parts = append(parts, "@"+a.category)
	}
	if a.overdue {
		parts = append(parts, "仅逾期")
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " · ")
}
