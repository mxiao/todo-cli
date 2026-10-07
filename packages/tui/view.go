package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mxiao/todo-cli/packages/core"
)

type regionKind int

const (
	regList regionKind = iota + 1
	regTask
	regTab
	regButton
	regDetail
	regOverlay
	regMenuItem
	regPaletteItem
	regFormField
	regFormSave
	regFormCancel
	regConfirmYes
	regConfirmNo
	regHelpClose
)

// region is a clickable area recorded while drawing; later regions are on top.
type region struct {
	x, y, w, h int
	kind       regionKind
	index      int
	action     Action
}

func (a *App) addRegion(r region) { a.regions = append(a.regions, r) }

// hit returns the topmost region at (x, y).
func (a *App) hit(x, y int) *region {
	for i := len(a.regions) - 1; i >= 0; i-- {
		r := &a.regions[i]
		if x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h {
			return r
		}
	}
	return nil
}

const (
	minWidth  = 40
	minHeight = 10
)

func (a *App) bodyTop() int    { return 2 }
func (a *App) listHeight() int { return max(a.h-4, 1) }
func (a *App) wide() bool      { return a.w >= 90 }

func (a *App) listWidth() int {
	if a.wide() {
		return a.w * 11 / 20
	}
	return a.w
}

// detailVisible reports whether the detail pane is on screen.
func (a *App) detailVisible() bool { return a.wide() || a.focus == paneDetail }

// Render draws the current state into a new frame and records the
// clickable regions used for mouse hit testing.
func (a *App) Render() *Screen {
	s := newScreen(a.w, a.h)
	a.regions = a.regions[:0]
	if a.w < minWidth || a.h < minHeight {
		s.put(0, 0, fmt.Sprintf("终端窗口太小：至少需要 %d×%d", minWidth, minHeight), Style{Bold: true}, a.w)
		s.put(0, 1, "q 退出", Style{}, a.w)
		return s
	}
	a.drawHeader(s)
	if !a.wide() && a.focus == paneDetail {
		a.drawDetail(s, 0, a.w)
	} else {
		a.drawList(s)
		if a.wide() {
			x := a.listWidth()
			for y := a.bodyTop(); y < a.bodyTop()+a.listHeight(); y++ {
				s.put(x, y, "│", Style{FG: colGray}, x+1)
			}
			a.drawDetail(s, x+1, a.w-x-1)
		}
	}
	a.drawStatus(s)
	a.drawFooter(s)
	switch a.mode {
	case modeHelp:
		a.drawHelp(s)
	case modePalette:
		a.drawPalette(s)
	case modeMenu:
		a.drawMenu(s)
	case modeForm:
		a.drawForm(s)
	case modeConfirm:
		a.drawConfirm(s)
	}
	return s
}

func (a *App) drawHeader(s *Screen) {
	x := s.put(0, 0, " todo ", Style{Bold: true, FG: colCyan}, a.w)
	for i, t := range tabs {
		label := fmt.Sprintf(" %d %s ", i+1, t.label)
		st := Style{}
		if i == a.tab {
			st = Style{Reverse: true, Bold: true}
		}
		x0 := x + 1
		x = s.put(x0, 0, label, st, a.w)
		a.addRegion(region{x: x0, y: 0, w: x - x0, h: 1, kind: regTab, index: i})
	}
	dir := ""
	if a.reverse {
		dir = "↓"
	}
	right := fmt.Sprintf("排序 %s%s · %d 项 ", sortLabels[a.sort], dir, len(a.tasks))
	if rx := a.w - textWidth(right); rx > x+1 {
		s.put(rx, 0, right, Style{Dim: true}, a.w)
		a.addRegion(region{x: rx, y: 0, w: textWidth(right), h: 1, kind: regButton, action: ActSort})
	}
	if a.filtersActive() {
		x := s.put(1, 1, "筛选："+a.filterSummary(), Style{FG: colYellow}, a.w)
		if k := a.keys.Key(ActClearFilters); k != "" {
			label := " [" + k + " 清除]"
			x2 := s.put(x, 1, label, Style{Dim: true}, a.w)
			a.addRegion(region{x: x, y: 1, w: x2 - x, h: 1, kind: regButton, action: ActClearFilters})
		}
	} else {
		hint := fmt.Sprintf("%s 新建 · %s 搜索 · %s 筛选 · %s 命令面板 · %s 帮助 · 右键菜单",
			a.keyHint(ActNew), a.keyHint(ActSearch), a.keyHint(ActFilterMenu), a.keyHint(ActPalette), a.keyHint(ActHelp))
		if !a.mouse {
			hint = strings.TrimSuffix(hint, " · 右键菜单")
		}
		s.put(1, 1, hint, Style{Dim: true}, a.w)
	}
}

var statusMarks = map[core.Status]string{
	core.StatusTodo: "[ ]", core.StatusInProgress: "[~]", core.StatusDone: "[x]", core.StatusArchived: "[-]",
}

var priorityMarks = []string{"   ", "·  ", "!  ", "!! ", "!!!"}

func (a *App) drawList(s *Screen) {
	lw := a.listWidth()
	top, h := a.bodyTop(), a.listHeight()
	a.addRegion(region{x: 0, y: top, w: lw, h: h, kind: regList})
	if len(a.tasks) == 0 {
		msg := fmt.Sprintf("还没有任务 — 按 %s 新建", a.keyHint(ActNew))
		if a.filtersActive() {
			msg = fmt.Sprintf("没有匹配的任务 — 按 %s 清除筛选", a.keyHint(ActClearFilters))
		}
		s.put(2, top+1, msg, Style{Dim: true}, lw)
		return
	}
	for row := 0; row < h; row++ {
		i := a.top + row
		if i >= len(a.tasks) {
			break
		}
		y := top + row
		a.drawRow(s, &a.tasks[i], i == a.cursor, y, lw)
		a.addRegion(region{x: 0, y: y, w: lw, h: 1, kind: regTask, index: i})
	}
	if len(a.tasks) > h { // scroll indicator
		pos := fmt.Sprintf(" %d/%d ", a.cursor+1, len(a.tasks))
		s.put(lw-textWidth(pos)-1, top+h-1, pos, Style{Dim: true, Reverse: true}, lw)
	}
}

func (a *App) drawRow(s *Screen, t *core.Task, selected bool, y, w int) {
	base := Style{}
	if t.Status == core.StatusDone || t.Status == core.StatusArchived {
		base.Dim = true
	}
	if selected {
		if a.focus == paneList {
			base = Style{Reverse: true}
		} else {
			base.Bold, base.Under = true, true
		}
	}
	s.fill(0, y, w-1, 1, base)
	marker := " "
	if selected {
		marker = "▸"
	}
	x := s.put(0, y, marker, base, w)
	stSt := base
	if t.Status == core.StatusDone && !base.Reverse {
		stSt.FG = colGreen
	}
	if t.Status == core.StatusInProgress && !base.Reverse {
		stSt.FG = colCyan
	}
	x = s.put(x, y, statusMarks[t.Status]+" ", stSt, w)
	pSt := base
	if !base.Reverse {
		switch t.Priority {
		case core.PriorityUrgent:
			pSt.FG = colRed
		case core.PriorityHigh:
			pSt.FG = colYellow
		}
	}
	x = s.put(x, y, priorityMarks[t.Priority]+" ", pSt, w)

	due := ""
	if t.DueAt != nil {
		due = a.dueLabel(*t.DueAt)
	}
	dueW := textWidth(due)
	titleMax := w - 1 - x
	if dueW > 0 {
		titleMax -= dueW + 2
	}
	title := t.Title
	if t.ParentID != "" {
		title = "↳ " + title
	}
	x = s.put(x, y, truncate(title, titleMax), base, x+titleMax)
	if len(t.Tags) > 0 && x < w-1-dueW-3 {
		tagSt := base
		tagSt.Dim = !base.Reverse
		if !base.Reverse {
			tagSt.FG = colBlue
		}
		tags := " #" + strings.Join(t.Tags, " #")
		limit := w - 1
		if dueW > 0 {
			limit -= dueW + 2
		}
		s.put(x, y, truncate(tags, limit-x), tagSt, limit)
	}
	if dueW > 0 {
		dSt := base
		if a.overdueTask(t) && !base.Reverse {
			dSt.FG = colRed
			dSt.Bold = true
		}
		s.put(w-1-dueW, y, due, dSt, w-1)
	}
}

type seg struct {
	text string
	st   Style
	act  Action
}

type dline []seg

func plain(text string, st Style) dline { return dline{{text: text, st: st}} }

// detailLines lays out the selected task's details for a pane w wide.
func (a *App) detailLines(t *core.Task, w int) []dline {
	var out []dline
	for _, l := range wrap(t.Title, w) {
		out = append(out, plain(l, Style{Bold: true}))
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
	btn := func(label string, act Action) seg {
		if k := a.keys.Key(act); k != "" {
			label = k + " " + label
		}
		return seg{text: "[" + label + "]", st: Style{FG: colCyan}, act: act}
	}
	// Action buttons, wrapped to the pane width.
	var row dline
	used := 0
	for _, b := range []seg{btn("编辑", ActEdit), btn(done, ActToggleDone), btn(start, ActStart),
		btn(archive, ActArchive), btn("删除", ActDelete)} {
		bw := textWidth(b.text)
		if used > 0 && used+1+bw > w {
			out = append(out, row)
			row, used = nil, 0
		}
		if used > 0 {
			row = append(row, seg{text: " "})
			used++
		}
		row = append(row, b)
		used += bw
	}
	out = append(out, row, nil)
	field := func(label, val string, st Style) {
		if val == "" {
			return
		}
		lines := wrap(val, max(w-9, 1))
		for i, l := range lines {
			lab := ""
			if i == 0 {
				lab = label
			}
			out = append(out, dline{{text: pad(lab, 9), st: Style{Dim: true}}, {text: l, st: st}})
		}
	}
	loc := a.now().Location()
	stamp := func(tp interface{ Format(string) string }) string { return tp.Format("2006-01-02 15:04") }
	id := t.ID
	if textWidth(id) > w-9 {
		id = id[:8]
	}
	field("ID", id, Style{})
	field("状态", statusLabels[t.Status], Style{})
	field("优先级", priorityLabels[t.Priority], Style{})
	if t.DueAt != nil {
		st := Style{}
		label := stamp(t.DueAt.In(loc)) + "（" + a.dueLabel(*t.DueAt) + "）"
		if a.overdueTask(t) {
			st.FG = colRed
			label += " 已逾期"
		}
		field("截止", label, st)
	}
	if len(t.Tags) > 0 {
		field("标签", "#"+strings.Join(t.Tags, " #"), Style{FG: colBlue})
	}
	field("分类", t.Category, Style{})
	if t.ParentID != "" {
		field("父任务", a.detail.parent+" ("+t.ParentID[:8]+")", Style{})
	}
	field("创建", stamp(t.CreatedAt.In(loc)), Style{})
	field("更新", fmt.Sprintf("%s（版本 %d）", stamp(t.UpdatedAt.In(loc)), t.Version), Style{})
	if t.CompletedAt != nil {
		field("完成", stamp(t.CompletedAt.In(loc)), Style{})
	}
	section := func(title string, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		out = append(out, nil, plain(title, Style{Bold: true}))
		for _, l := range wrap(body, max(w-2, 1)) {
			out = append(out, plain("  "+l, Style{}))
		}
	}
	section("描述", t.Description)
	section("备注", t.Notes)
	if len(a.detail.children) > 0 {
		out = append(out, nil, plain(fmt.Sprintf("子任务（%d）", len(a.detail.children)), Style{Bold: true}))
		for _, c := range a.detail.children {
			out = append(out, plain("  "+statusMarks[c.Status]+" "+truncate(c.Title, w-6), Style{}))
		}
	}
	if n := len(a.detail.history); n > 0 {
		out = append(out, nil, plain(fmt.Sprintf("历史（%d）", n), Style{Bold: true}))
		for i := n - 1; i >= 0 && i >= n-20; i-- {
			h := a.detail.history[i]
			keys := make([]string, 0, len(h.Changes))
			for k := range h.Changes {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			line := fmt.Sprintf("  %s %s %s", h.CreatedAt.In(loc).Format("01-02 15:04"), h.Actor, h.Action)
			if len(keys) > 0 && h.Action != "create" {
				line += " " + strings.Join(keys, ",")
			}
			out = append(out, plain(truncate(line, w), Style{Dim: true}))
		}
	}
	return out
}

func (a *App) drawDetail(s *Screen, x0, w int) {
	top, h := a.bodyTop(), a.listHeight()
	a.addRegion(region{x: x0, y: top, w: w, h: h, kind: regDetail})
	titleSt := Style{Dim: true}
	label := fmt.Sprintf(" 详情 · %s 切换列表/详情 ", a.keyHint(ActToggleFocus))
	if a.focus == paneDetail {
		titleSt = Style{Reverse: true, Bold: true}
	}
	s.fill(x0, top, w, 1, titleSt)
	s.put(x0, top, label, titleSt, x0+w)
	t := a.Selected()
	if t == nil {
		s.put(x0+1, top+2, "未选中任务", Style{Dim: true}, x0+w)
		return
	}
	lines := a.detailLines(t, max(w-2, 10))
	avail := h - 1
	a.detailScroll = max(0, min(a.detailScroll, len(lines)-avail))
	for i := 0; i < avail && a.detailScroll+i < len(lines); i++ {
		y := top + 1 + i
		x := x0 + 1
		for _, sg := range lines[a.detailScroll+i] {
			nx := s.put(x, y, sg.text, sg.st, x0+w)
			if sg.act != "" {
				a.addRegion(region{x: x, y: y, w: nx - x, h: 1, kind: regButton, action: sg.act})
			}
			x = nx
		}
	}
	if len(lines) > avail {
		more := fmt.Sprintf(" %d/%d ", a.detailScroll+1, len(lines)-avail+1)
		s.put(x0+w-textWidth(more)-1, top, more, titleSt, x0+w)
	}
}

func (a *App) drawStatus(s *Screen) {
	y := a.h - 2
	switch a.mode {
	case modeSearch:
		a.drawInput(s, y, "搜索", a.searchEd, "enter 确定 · esc 取消 · tab 补全", Style{FG: colCyan, Bold: true})
		return
	case modePrompt:
		a.drawInput(s, y, a.prompt.label, a.prompt.ed, a.prompt.hint, Style{FG: colCyan, Bold: true})
		if a.msgErr && a.msg != "" { // keep input errors visible on the header line
			s.fill(0, 1, a.w, 1, Style{})
			s.put(1, 1, a.msg, Style{FG: colRed, Bold: true}, a.w)
		}
		return
	}
	st := Style{FG: colGreen}
	if a.msgErr {
		st = Style{FG: colRed, Bold: true}
	}
	s.put(1, y, a.msg, st, a.w)
}

// drawInput renders a one-line input: "label: text" with the hint after it.
func (a *App) drawInput(s *Screen, y int, label string, ed *lineEditor, hint string, st Style) {
	x := s.put(0, y, " "+label+"：", st, a.w)
	hintW := 0
	if textWidth(hint)+x+20 < a.w {
		hintW = textWidth(hint) + 2
	}
	fw := a.w - x - hintW - 1
	text, cx := ed.render(fw)
	s.fill(x, y, fw, 1, Style{Under: true})
	s.put(x, y, text, Style{Under: true}, x+fw)
	if hintW > 0 {
		s.put(a.w-hintW+1, y, hint, Style{Dim: true}, a.w)
	}
	s.ShowCursor, s.CursorX, s.CursorY = true, x+cx, y
}

type footerButton struct {
	act   Action
	label string
}

var footerButtons = []footerButton{
	{ActNew, "新建"}, {ActEdit, "编辑"}, {ActToggleDone, "完成"}, {ActDelete, "删除"}, {ActSearch, "搜索"},
	{ActFilterMenu, "筛选"}, {ActSort, "排序"}, {ActPalette, "命令"}, {ActHelp, "帮助"}, {ActQuit, "退出"},
}

func (a *App) drawFooter(s *Screen) {
	y := a.h - 1
	s.fill(0, y, a.w, 1, Style{Reverse: true})
	x := 0
	for _, b := range footerButtons {
		k := a.keys.Key(b.act)
		w := textWidth(k) + textWidth(b.label) + 3
		if x+w > a.w {
			break
		}
		x0 := x
		x = s.put(x, y, " ", Style{Reverse: true}, a.w)
		if k != "" {
			x = s.put(x, y, k, Style{Reverse: true, Bold: true}, a.w)
			x = s.put(x, y, " ", Style{Reverse: true}, a.w)
		}
		x = s.put(x, y, b.label+" ", Style{Reverse: true}, a.w)
		a.addRegion(region{x: x0, y: y, w: x - x0, h: 1, kind: regButton, action: b.act})
	}
}

// --- overlays -------------------------------------------------------------------

func (a *App) helpLines() []dline {
	var out []dline
	group := ""
	for _, b := range a.keys.Bindings() {
		if b.Group != group {
			if group != "" {
				out = append(out, nil)
			}
			group = b.Group
			out = append(out, plain(group, Style{Bold: true, FG: colCyan}))
		}
		keys := strings.Join(b.Keys, " / ")
		if keys == "" {
			keys = "（未绑定）"
		}
		out = append(out, dline{{text: "  " + pad(keys, 22), st: Style{Bold: true}}, {text: b.Label}, {text: "  :" + strings.ReplaceAll(string(b.Action), "_", "-"), st: Style{Dim: true}}})
	}
	out = append(out, nil, plain("鼠标", Style{Bold: true, FG: colCyan}))
	mouse := []string{
		"  单击任务          选中（与键盘光标同步）",
		"  双击任务          打开详情",
		"  右键任务          操作菜单（完成、编辑、删除…）",
		"  滚轮              滚动列表 / 详情",
		"  单击顶部标签      切换状态筛选；单击底栏按钮执行操作",
	}
	if !a.mouse {
		mouse = append([]string{"  鼠标上报已关闭（--no-mouse 或 TODO_CLI_MOUSE=0），所有操作均可用键盘完成"}, mouse...)
	}
	for _, l := range mouse {
		out = append(out, plain(l, Style{}))
	}
	out = append(out, nil, plain("快速新建语法", Style{Bold: true, FG: colCyan}),
		plain("  写周报 #work @office !high due:tomorrow", Style{}),
		plain("  #标签  @分类  !low|!medium|!high|!urgent（或 ! !! !!!）  due:today|+3d|fri|2026-10-09", Style{Dim: true}),
		nil, plain("命令面板（"+a.keyHint(ActPalette)+"）", Style{Bold: true, FG: colCyan}),
		plain("  输入命令名筛选，tab 补全，enter 执行。带参数的命令：", Style{}),
		plain("  add <标题>   search <关键词>   filter status|priority|tag|category|overdue|clear [值]", Style{Dim: true}),
		plain("  sort <字段> [reverse]   priority <级别>   due <时间|none>   tag|untag <标签>   category <分类>   goto <id>", Style{Dim: true}),
		nil, plain("自定义快捷键", Style{Bold: true, FG: colCyan}),
	)
	path := a.keys.Path
	if path == "" {
		path = "~/.todo-cli/" + KeymapFileName
	}
	out = append(out,
		plain("  配置文件："+path, Style{}),
		plain("  格式：{\"new\": [\"ctrl+n\", \"a\"], \"delete\": \"D\"}；运行 todo keys --init 生成模板，todo keys 查看当前绑定", Style{Dim: true}),
	)
	if len(a.keys.Custom) > 0 {
		var names []string
		for _, c := range a.keys.Custom {
			names = append(names, string(c))
		}
		out = append(out, plain("  已自定义："+strings.Join(names, ", "), Style{FG: colYellow}))
	}
	return out
}

func (a *App) drawHelp(s *Screen) {
	bw, bh := min(a.w-2, 110), a.h-2
	x, y := (a.w-bw)/2, 1
	s.box(x, y, bw, bh, "快捷键帮助", Style{FG: colCyan})
	a.addRegion(region{x: x, y: y, w: bw, h: bh, kind: regOverlay})
	closeLabel := "[esc 关闭]"
	cx := x + bw - textWidth(closeLabel) - 2
	s.put(cx, y, closeLabel, Style{Bold: true}, x+bw-1)
	a.addRegion(region{x: cx, y: y, w: textWidth(closeLabel), h: 1, kind: regHelpClose})
	var lines []dline
	for _, l := range a.helpLines() {
		if len(l) != 1 || textWidth(l[0].text) <= bw-4 {
			lines = append(lines, l)
			continue
		}
		for i, part := range wrap(l[0].text, bw-8) {
			if i > 0 {
				part = "    " + part
			}
			lines = append(lines, plain(part, l[0].st))
		}
	}
	avail := bh - 2
	a.helpTop = max(0, min(a.helpTop, len(lines)-avail))
	for i := 0; i < avail && a.helpTop+i < len(lines); i++ {
		xx := x + 2
		for _, sg := range lines[a.helpTop+i] {
			xx = s.put(xx, y+1+i, sg.text, sg.st, x+bw-1)
		}
	}
	if len(lines) > avail {
		more := fmt.Sprintf(" ↑↓ 滚动 %d/%d ", a.helpTop+1, len(lines)-avail+1)
		s.put(x+bw-textWidth(more)-2, y+bh-1, more, Style{Dim: true}, x+bw-1)
	}
}

func (a *App) paletteBox() (x, y, w, h int) {
	w = min(a.w-4, 80)
	h = min(a.h-2, 16)
	return (a.w - w) / 2, 1, w, h
}

func (a *App) drawPalette(s *Screen) {
	p := a.palette
	x, y, w, h := a.paletteBox()
	s.box(x, y, w, h, "命令面板", Style{FG: colCyan})
	a.addRegion(region{x: x, y: y, w: w, h: h, kind: regOverlay})
	px := s.put(x+2, y+1, "> ", Style{Bold: true}, x+w-1)
	text, cx := p.ed.render(x + w - 2 - px)
	s.put(px, y+1, text, Style{}, x+w-1)
	s.ShowCursor, s.CursorX, s.CursorY = true, px+cx, y+1
	info, infoSt := "↑↓ 选择 · tab 补全 · enter 执行 · esc 关闭", Style{Dim: true}
	if p.err != "" {
		info, infoSt = p.err, Style{FG: colRed}
		if strings.HasPrefix(p.err, "用法") || strings.HasPrefix(p.err, "可选") {
			infoSt = Style{FG: colYellow}
		}
	}
	s.put(x+2, y+2, info, infoSt, x+w-1)
	rows := h - 4
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+rows {
		p.top = p.sel - rows + 1
	}
	for i := 0; i < rows && p.top+i < len(p.matches); i++ {
		c := p.matches[p.top+i]
		yy := y + 3 + i
		st := Style{}
		if p.top+i == p.sel {
			st = Style{Reverse: true}
		}
		s.fill(x+1, yy, w-2, 1, st)
		name := c.name
		if c.usage != "" {
			name += " " + c.usage
		}
		key := ""
		if c.action != "" {
			key = a.keys.Key(c.action)
		} else if c.bare != "" {
			key = a.keys.Key(c.bare)
		}
		nx := s.put(x+2, yy, pad(name, 28), Style{Bold: true, Reverse: st.Reverse}, x+w-2)
		s.put(nx+1, yy, c.label, st, x+w-3-textWidth(key))
		if key != "" {
			s.put(x+w-2-textWidth(key), yy, key, Style{Dim: !st.Reverse, Reverse: st.Reverse}, x+w-1)
		}
		a.addRegion(region{x: x + 1, y: yy, w: w - 2, h: 1, kind: regPaletteItem, index: p.top + i})
	}
	if len(p.matches) == 0 {
		s.put(x+2, y+3, "没有匹配的命令", Style{Dim: true}, x+w-1)
	}
}

func (a *App) menuBox() (x, y, w, h int) {
	m := a.menu
	w = textWidth(m.title) + 6
	for _, it := range m.items {
		w = max(w, textWidth(it.label)+textWidth(it.hint)+6)
	}
	h = len(m.items) + 2
	x = max(0, min(m.x, a.w-w))
	y = max(0, min(m.y, a.h-h))
	return x, y, w, h
}

func (a *App) drawMenu(s *Screen) {
	m := a.menu
	x, y, w, h := a.menuBox()
	s.box(x, y, w, h, m.title, Style{FG: colCyan})
	a.addRegion(region{x: x, y: y, w: w, h: h, kind: regOverlay})
	for i, it := range m.items {
		st := Style{}
		if i == m.sel {
			st = Style{Reverse: true}
		}
		yy := y + 1 + i
		s.fill(x+1, yy, w-2, 1, st)
		s.put(x+2, yy, it.label, st, x+w-1)
		if it.hint != "" {
			s.put(x+w-2-textWidth(it.hint), yy, it.hint, Style{Dim: !st.Reverse, Reverse: st.Reverse}, x+w-1)
		}
		a.addRegion(region{x: x + 1, y: yy, w: w - 2, h: 1, kind: regMenuItem, index: i})
	}
}

func (a *App) formBox() (x, y, w, h int) {
	w = min(a.w-4, 80)
	h = min(len(a.form.fields)+7, a.h)
	return (a.w - w) / 2, max((a.h-h)/2, 0), w, h
}

func (a *App) drawForm(s *Screen) {
	f := a.form
	x, y, w, h := a.formBox()
	s.box(x, y, w, h, fmt.Sprintf("编辑任务 · 版本 %d", f.version), Style{FG: colCyan})
	a.addRegion(region{x: x, y: y, w: w, h: h, kind: regOverlay})
	fw := w - 14
	for i, fl := range f.fields {
		yy := y + 1 + i
		lst := Style{Dim: true}
		if i == f.focus {
			lst = Style{Reverse: true, Bold: true}
		}
		s.put(x+2, yy, pad(fl.label, 8), lst, x+10)
		text, cx := fl.ed.render(fw)
		s.fill(x+11, yy, fw, 1, Style{Under: true})
		s.put(x+11, yy, text, Style{Under: true}, x+11+fw)
		if i == f.focus {
			s.ShowCursor, s.CursorX, s.CursorY = true, x+11+cx, yy
		}
		a.addRegion(region{x: x + 1, y: yy, w: w - 2, h: 1, kind: regFormField, index: i})
	}
	yy := y + len(f.fields) + 2
	if f.err != "" {
		s.put(x+2, yy, truncate(f.err, w-4), Style{FG: colRed, Bold: true}, x+w-1)
	} else {
		s.put(x+2, yy, "截止：today / tomorrow / +3d / fri / 2026-10-09 18:00，留空清除", Style{Dim: true}, x+w-1)
	}
	s.put(x+2, yy+1, "↑↓ 切换字段 · tab 补全/下一项 · enter 下一项 · ctrl+s 保存 · esc 取消", Style{Dim: true}, x+w-1)
	bx := x + 2
	save, cancel := "[ ctrl+s 保存 ]", "[ esc 取消 ]"
	nx := s.put(bx, yy+2, save, Style{Bold: true, FG: colGreen}, x+w-1)
	a.addRegion(region{x: bx, y: yy + 2, w: nx - bx, h: 1, kind: regFormSave})
	cx := nx + 2
	nx = s.put(cx, yy+2, cancel, Style{}, x+w-1)
	a.addRegion(region{x: cx, y: yy + 2, w: nx - cx, h: 1, kind: regFormCancel})
}

func (a *App) drawConfirm(s *Screen) {
	c := a.confirm
	w := min(a.w-4, 60)
	lines := wrap(c.msg, w-4)
	h := len(lines) + 4
	x, y := (a.w-w)/2, max((a.h-h)/2, 0)
	s.box(x, y, w, h, c.title, Style{FG: colRed})
	a.addRegion(region{x: x, y: y, w: w, h: h, kind: regOverlay})
	for i, l := range lines {
		s.put(x+2, y+1+i, l, Style{}, x+w-1)
	}
	by := y + h - 2
	yes, no := "[ y "+c.yes+" ]", "[ n 取消 ]"
	nx := s.put(x+2, by, yes, Style{Bold: true, FG: colRed}, x+w-1)
	a.addRegion(region{x: x + 2, y: by, w: nx - x - 2, h: 1, kind: regConfirmYes})
	cx := nx + 2
	nx = s.put(cx, by, no, Style{}, x+w-1)
	a.addRegion(region{x: cx, y: by, w: nx - cx, h: 1, kind: regConfirmNo})
}
