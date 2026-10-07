package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mxiao/todo-cli/packages/core"
)

// Tuesday 2026-10-06 10:00 in UTC+8.
var cst = time.FixedZone("CST", 8*3600)
var testNow = time.Date(2026, 10, 6, 10, 0, 0, 0, cst)

func testParseDue(s string, now time.Time) (time.Time, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, now.Location())
	switch s {
	case "today":
		return day, nil
	case "tomorrow":
		return day.AddDate(0, 0, 1), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t.Add(23*time.Hour + 59*time.Minute), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}

type th struct {
	t     *testing.T
	a     *App
	s     *core.Store
	dir   string
	clock time.Time
}

func newTH(t *testing.T, w, h int, km *Keymap, titles ...string) *th {
	t.Helper()
	dir := t.TempDir()
	s, err := core.Open(core.Options{DataDir: dir, Actor: "tui"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, title := range titles {
		if _, err := s.Create(core.NewTask{Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := New(Options{Store: s, Keymap: km, Mouse: true, Now: func() time.Time { return testNow }, ParseDue: testParseDue}, w, h)
	if err != nil {
		t.Fatal(err)
	}
	h2 := &th{t: t, a: a, s: s, dir: dir, clock: testNow}
	// Each click happens a second after the previous one unless a test
	// freezes the clock to produce a double click.
	a.clock = func() time.Time { h2.clock = h2.clock.Add(time.Second); return h2.clock }
	return h2
}

// keys feeds raw terminal bytes (keys, mouse escape sequences, pastes),
// rendering before each event like the real event loop does.
func (h *th) keys(seq string) {
	h.t.Helper()
	evs, n := ParseInput([]byte(seq), true)
	if n != len(seq) {
		h.t.Fatalf("unparsed input in %q", seq)
	}
	for _, ev := range evs {
		h.a.Render()
		h.a.HandleEvent(ev)
	}
}

func sgrMouse(button, x, y int) string {
	return fmt.Sprintf("\x1b[<%d;%d;%dM\x1b[<%d;%d;%dm", button, x+1, y+1, button&^64, x+1, y+1)
}

func (h *th) click(x, y int)  { h.keys(sgrMouse(0, x, y)) }
func (h *th) rclick(x, y int) { h.keys(sgrMouse(2, x, y)) }
func (h *th) wheel(down bool, x, y int) {
	b := 64
	if down {
		b = 65
	}
	h.keys(fmt.Sprintf("\x1b[<%d;%d;%dM", b, x+1, y+1))
}

func (h *th) screen() string { return h.a.Render().String() }

// find locates text on screen and returns its cell coordinates.
func (h *th) find(text string) (int, int) {
	h.t.Helper()
	sc := h.a.Render()
	for y := 0; y < sc.H; y++ {
		line := sc.Line(y)
		if i := strings.Index(line, text); i >= 0 {
			return textWidth(line[:i]), y
		}
	}
	h.t.Fatalf("%q not on screen:\n%s", text, sc.String())
	return 0, 0
}

func (h *th) clickText(text string) {
	h.t.Helper()
	x, y := h.find(text)
	h.click(x, y)
}

// clickMenu clicks the open menu's item with the given label.
func (h *th) clickMenu(label string) {
	h.t.Helper()
	h.a.Render()
	x, y, _, _ := h.a.menuBox()
	for i, it := range h.a.menu.items {
		if strings.TrimSpace(it.label) == label {
			h.click(x+3, y+1+i)
			return
		}
	}
	h.t.Fatalf("menu has no %q", label)
}

// clickFooter clicks a footer button by its label.
func (h *th) clickFooter(label string) {
	h.t.Helper()
	sc := h.a.Render()
	line := sc.Line(sc.H - 1)
	i := strings.Index(line, label)
	if i < 0 {
		h.t.Fatalf("footer has no %q: %s", label, line)
	}
	h.click(textWidth(line[:i]), sc.H-1)
}

func (h *th) wantScreen(parts ...string) {
	h.t.Helper()
	sc := h.screen()
	for _, p := range parts {
		if !strings.Contains(sc, p) {
			h.t.Errorf("screen missing %q:\n%s", p, sc)
		}
	}
}

func (h *th) task(title string) *core.Task {
	h.t.Helper()
	all, err := h.s.List(core.Filter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		h.t.Fatal(err)
	}
	for i := range all {
		if all[i].Title == title {
			return &all[i]
		}
	}
	h.t.Fatalf("no task %q", title)
	return nil
}

func (h *th) selected() string {
	if t := h.a.Selected(); t != nil {
		return t.Title
	}
	return ""
}

func TestKeyboardCreateEditCompleteDelete(t *testing.T) {
	h := newTH(t, 100, 30, nil, "第一项", "第二项")
	h.wantScreen("第一项", "第二项", "1 全部", "a 新建")

	// Quick add with tags, category, priority and due date.
	h.keys("a")
	h.wantScreen("新任务：")
	h.keys("写周报 #work @office !high due:tomorrow\r")
	tk := h.task("写周报")
	if tk.Priority != core.PriorityHigh || strings.Join(tk.Tags, ",") != "work" || tk.Category != "office" || tk.DueAt == nil {
		t.Fatalf("quick add: %+v", tk)
	}
	if h.selected() != "写周报" {
		t.Errorf("new task should be selected, got %q", h.selected())
	}
	h.wantScreen("已创建：写周报", "明天")

	// Navigation.
	h.keys("gj")
	if h.selected() != "第二项" {
		t.Errorf("g j: %q", h.selected())
	}
	h.keys("\x1b[B") // down arrow
	if h.selected() != "写周报" {
		t.Errorf("down: %q", h.selected())
	}
	h.keys("k\x1b[A")
	if h.selected() != "第一项" {
		t.Errorf("k up: %q", h.selected())
	}

	// Complete and reopen.
	h.keys("x")
	if h.task("第一项").Status != core.StatusDone {
		t.Errorf("x should complete")
	}
	h.keys(" ")
	if h.task("第一项").Status != core.StatusTodo {
		t.Errorf("space should reopen")
	}
	h.keys("s")
	if h.task("第一项").Status != core.StatusInProgress {
		t.Errorf("s should start")
	}

	// Edit form: replace the title, move to the priority field, save.
	h.keys("e")
	h.wantScreen("编辑任务", "标题", "优先级")
	h.keys("\x15改名后\x1b[B\x1b[B\x1b[B\x15urgent\x13") // ctrl+u clears, ↓ moves, ctrl+s saves
	tk = h.task("改名后")
	if tk.Priority != core.PriorityUrgent {
		t.Errorf("edit priority: %v", tk.Priority)
	}
	h.wantScreen("已保存：改名后")

	// Priority keys.
	h.keys("-")
	if h.task("改名后").Priority != core.PriorityHigh {
		t.Errorf("- should lower priority")
	}

	// Delete asks for confirmation; n cancels, y deletes, u undoes.
	h.keys("d")
	h.wantScreen("删除任务", "确定删除「改名后」")
	h.keys("n")
	if h.task("改名后").Deleted() {
		t.Fatalf("n must cancel delete")
	}
	h.keys("dy")
	if !h.task("改名后").Deleted() {
		t.Fatalf("y should delete")
	}
	h.wantScreen("已删除：改名后 — 按 u 撤销")
	h.keys("u")
	if h.task("改名后").Deleted() {
		t.Errorf("u should undo the delete")
	}

	// Archive toggles.
	h.keys("A")
	if h.task("改名后").Status != core.StatusArchived {
		t.Errorf("A should archive")
	}
	h.keys("q")
	if !h.a.Quit() {
		t.Errorf("q should quit")
	}
}

func TestQuickAddErrorsKeepInput(t *testing.T) {
	h := newTH(t, 100, 30, nil)
	h.keys("a买菜 !bogus\r")
	if h.a.mode != modePrompt || h.a.prompt.ed.String() != "买菜 !bogus" {
		t.Fatalf("an invalid quick add must keep the prompt open")
	}
	h.wantScreen("优先级 \"!bogus\" 无效")
	h.keys("\x7f\x7f\x7f\x7f\x7f\x7fdue:someday\r")
	h.wantScreen("截止时间")
	h.keys("\x15 #only-tags\r")
	h.wantScreen("标题不能为空")
	h.keys("\x1b")
	if h.a.mode != modeNormal {
		t.Errorf("esc should cancel the prompt")
	}
	if n := len(h.a.Tasks()); n != 0 {
		t.Errorf("nothing should have been created, got %d", n)
	}
	// Pasting text in the list opens a prefilled new-task prompt.
	h.keys("\x1b[200~粘贴的任务\x1b[201~\r")
	h.task("粘贴的任务")
}

func TestSearchFilterSort(t *testing.T) {
	h := newTH(t, 100, 30, nil)
	for _, in := range []core.NewTask{
		{Title: "写周报", Tags: []string{"work"}, Priority: core.PriorityHigh},
		{Title: "买牛奶", Tags: []string{"home"}},
		{Title: "周会纪要", Tags: []string{"work"}, Priority: core.PriorityUrgent},
	} {
		if _, err := h.s.Create(in); err != nil {
			t.Fatal(err)
		}
	}
	h.keys("R")
	if len(h.a.Tasks()) != 3 {
		t.Fatalf("reload: %d", len(h.a.Tasks()))
	}
	// Live search narrows as you type; enter keeps it, esc in the list clears it.
	h.keys("/周")
	if len(h.a.Tasks()) != 2 {
		t.Errorf("live search: %d", len(h.a.Tasks()))
	}
	h.keys("报\r")
	if len(h.a.Tasks()) != 1 || h.selected() != "写周报" {
		t.Errorf("search: %v", h.a.Tasks())
	}
	h.wantScreen("筛选：搜索「周报」")
	h.keys("\x1b")
	if len(h.a.Tasks()) != 3 {
		t.Errorf("esc should clear the search")
	}
	// Esc while typing restores the previous query.
	h.keys("/牛\x1b")
	if len(h.a.Tasks()) != 3 || h.a.query != "" {
		t.Errorf("cancelled search: %q", h.a.query)
	}
	// Tab completes search words from tags.
	h.keys("/ho\t\r")
	if h.a.query != "home" || len(h.a.Tasks()) != 1 {
		t.Errorf("search completion: %q", h.a.query)
	}
	h.keys("c")

	// Status tabs: complete one task, then switch to the "done" tab.
	h.keys("gx")
	h.keys("]]]")
	if h.a.tab != 3 || len(h.a.Tasks()) != 1 {
		t.Errorf("done tab: tab=%d n=%d", h.a.tab, len(h.a.Tasks()))
	}
	h.keys("1")
	if h.a.tab != 0 {
		t.Errorf("1 jumps to the first tab")
	}

	// Filter menu via keyboard: "优先级 ≥ 高" is the 7th item.
	h.keys("f")
	h.wantScreen("筛选", "状态：全部", "仅显示逾期", "按标签…")
	h.keys("jjjjjj\r")
	if h.a.minPriority == nil || len(h.a.Tasks()) != 2 {
		t.Errorf("priority filter: %d", len(h.a.Tasks()))
	}
	// Filter by tag through the menu prompt with completion.
	h.keys("fjjjjjjj\r")
	h.keys("wo\t\r")
	if h.a.tag != "work" {
		t.Errorf("tag filter: %q", h.a.tag)
	}
	h.keys("c")
	if h.a.filtersActive() {
		t.Errorf("c clears everything")
	}

	// Sorting cycles manual → due → priority.
	h.keys("oo")
	if h.a.sort != core.SortPriority || h.a.Tasks()[0].Title != "周会纪要" {
		t.Errorf("priority sort: %v %s", h.a.sort, h.a.Tasks()[0].Title)
	}
	h.keys("O")
	if !h.a.reverse || h.a.Tasks()[0].Priority != core.PriorityNone {
		t.Errorf("reverse sort")
	}
	h.wantScreen("排序 优先级↓")
	// Manual reordering needs the manual sort.
	h.keys("K")
	h.wantScreen("手动调整顺序只在「手动」排序下可用")
	h.keys("ooo") // back to manual
	if h.a.sort != core.SortManual {
		t.Fatalf("sort: %v", h.a.sort)
	}
	h.keys("O") // un-reverse
	first := h.a.Tasks()[0].Title
	h.keys("gJ")
	if h.a.Tasks()[1].Title != first || h.selected() != first {
		t.Errorf("J should move the task down and keep it selected: %v", h.a.Tasks())
	}
}

func TestPalette(t *testing.T) {
	h := newTH(t, 100, 30, nil, "任务甲", "任务乙")
	h.keys(":")
	h.wantScreen("命令面板", "add <标题>", "filter <条件> [值]")
	h.keys("add 从面板新建 #p\r")
	h.task("从面板新建")
	if h.a.mode != modeNormal {
		t.Errorf("palette should close after running")
	}
	// Tab completes command names and arguments.
	h.keys("\x10fil\t")
	if got := h.a.palette.ed.String(); got != "filter " {
		t.Errorf("tab completion: %q", got)
	}
	h.keys("st\tdo\t\r")
	if h.a.tab != 3 {
		t.Errorf("filter status done: tab=%d", h.a.tab)
	}
	h.keys("1")
	// Unknown commands get a suggestion and keep the palette open.
	h.keys(":sotr due\r")
	h.wantScreen("未知命令 \"sotr\"，是否想输入 sort？")
	h.keys("\x1b")
	// Commands acting on the selection.
	h.keys("g:priority urgent\r")
	if h.task("任务甲").Priority != core.PriorityUrgent {
		t.Errorf("palette priority")
	}
	h.keys(":due tomorrow\r")
	if h.task("任务甲").DueAt == nil {
		t.Errorf("palette due")
	}
	h.keys(":tag a b\r:untag a\r:category 家\r")
	if tk := h.task("任务甲"); strings.Join(tk.Tags, ",") != "b" || tk.Category != "家" {
		t.Errorf("tag/category: %+v", tk)
	}
	// Choosing an entry by arrows runs the action; actions without args run directly.
	h.keys(":help\r")
	if h.a.mode != modeHelp {
		t.Errorf("palette help")
	}
	h.keys("\x1b")
	h.keys(":goto " + h.task("任务乙").ID[:6] + "\r")
	if h.selected() != "任务乙" || h.a.focus != paneDetail {
		t.Errorf("goto: %q", h.selected())
	}
	// A command needing arguments without them shows its usage.
	h.keys(":priority\r")
	h.wantScreen("用法：priority <none|low|medium|high|urgent>")
}

func TestHelpShowsEffectiveKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeymapFileName)
	os.WriteFile(path, []byte(`{"new": "ctrl+n", "help": ["f1", "H"]}`), 0o600)
	km, err := LoadKeymap(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newTH(t, 100, 30, km, "任务")
	h.wantScreen("ctrl+n 新建") // footer shows the custom key
	h.keys("a")
	h.wantScreen("按键 a 未绑定任何操作")
	h.keys("\x0e") // ctrl+n
	if h.a.mode != modePrompt {
		t.Fatalf("ctrl+n should open the new-task prompt")
	}
	h.keys("\x1b")
	h.keys("H")
	if h.a.mode != modeHelp {
		t.Fatalf("H should open help, mode=%d", h.a.mode)
	}
	h.wantScreen("快捷键帮助", "ctrl+n", "新建任务")
	h.keys("jjj")
	if h.a.helpTop != 3 {
		t.Errorf("help scroll: %d", h.a.helpTop)
	}
	h.keys("G")
	h.wantScreen("f1 / H", "鼠标", "右键任务", "配置文件：", KeymapFileName, "todo keys --init", "已自定义：help, new")
	h.keys("\x1b")
	if h.a.mode != modeNormal {
		t.Errorf("esc closes help")
	}
	// ctrl+c cancels open inputs instead of quitting.
	h.keys("e\x15changed\x03")
	if h.a.mode != modeNormal || h.a.Quit() || h.task("任务").Title != "任务" {
		t.Errorf("ctrl+c should cancel the form without saving or quitting")
	}
	h.keys("\x03")
	if !h.a.Quit() {
		t.Errorf("ctrl+c quits from the list")
	}
}

func TestMouseSelectionMatchesKeyboard(t *testing.T) {
	h := newTH(t, 100, 30, nil, "一", "二", "三", "四")
	top := h.a.bodyTop()
	h.click(10, top+2)
	if h.selected() != "三" || h.a.cursor != 2 {
		t.Fatalf("click should select row 3, got %q", h.selected())
	}
	// The keyboard continues from the clicked row.
	h.keys("j")
	if h.selected() != "四" {
		t.Errorf("j after click: %q", h.selected())
	}
	h.keys("k")
	if h.selected() != "三" {
		t.Errorf("k after click: %q", h.selected())
	}
	// Single click in the detail pane focuses it; the selection is kept.
	h.click(80, top+5)
	if h.a.focus != paneDetail || h.selected() != "三" {
		t.Errorf("detail click: focus=%d sel=%q", h.a.focus, h.selected())
	}
	h.keys("\t")
	if h.a.focus != paneList {
		t.Errorf("tab returns to the list")
	}
	// Double click opens the details.
	frozen := h.clock
	h.a.clock = func() time.Time { return frozen }
	h.click(10, top)
	h.click(10, top)
	if h.selected() != "一" || h.a.focus != paneDetail {
		t.Errorf("double click: sel=%q focus=%d", h.selected(), h.a.focus)
	}
	h.a.clock = func() time.Time { frozen = frozen.Add(time.Second); return frozen }

	// Detail buttons are clickable.
	h.clickText("[x 完成]")
	if h.task("一").Status != core.StatusDone {
		t.Errorf("detail button should complete the task")
	}
	// Tabs and footer buttons are clickable.
	h.clickText("4 已完成")
	if h.a.tab != 3 || len(h.a.Tasks()) != 1 {
		t.Errorf("tab click: %d", h.a.tab)
	}
	h.clickText("1 全部")
	h.clickFooter("新建")
	if h.a.mode != modePrompt {
		t.Fatalf("footer button should open the new-task prompt")
	}
	// Clicks while typing are ignored so the input is not disturbed.
	h.keys("鼠标新建")
	h.click(10, top+1)
	if h.a.mode != modePrompt || h.a.prompt.ed.String() != "鼠标新建" {
		t.Errorf("prompt input disturbed by click")
	}
	h.keys("\r")
	h.task("鼠标新建")
}

func TestRightClickMenu(t *testing.T) {
	h := newTH(t, 100, 30, nil, "一", "二", "三")
	top := h.a.bodyTop()
	h.rclick(12, top+1)
	if h.selected() != "二" || h.a.mode != modeMenu {
		t.Fatalf("right click should select and open the menu: sel=%q mode=%d", h.selected(), h.a.mode)
	}
	h.wantScreen("打开详情", "编辑", "完成", "删除")
	// Menu is placed at the click position; click the "完成" item.
	if x, y, _, _ := h.a.menuBox(); x != 12 || y != top+1 {
		t.Errorf("menu should open at the pointer: %d,%d", x, y)
	}
	h.clickMenu("完成")
	if h.task("二").Status != core.StatusDone || h.a.mode != modeNormal {
		t.Errorf("menu click should complete the task")
	}
	// Right click → keyboard navigation inside the menu also works.
	h.rclick(12, top+2)
	h.keys("\x1b[B\r") // second item: 编辑
	if h.a.mode != modeForm || h.a.form.title != "三" {
		t.Fatalf("menu keyboard: mode=%d", h.a.mode)
	}
	// In the form, clicking another field focuses it and keeps all input.
	h.keys("\x15三（改）")
	x, y := h.find("备注")
	h.click(x, y)
	if h.a.form.focus != 6 || h.a.form.field(fieldTitle) != "三（改）" {
		t.Errorf("form click: focus=%d title=%q", h.a.form.focus, h.a.form.field(fieldTitle))
	}
	h.keys("记得带伞")
	h.clickText("[ ctrl+s 保存 ]")
	tk := h.task("三（改）")
	if tk.Notes != "记得带伞" {
		t.Errorf("notes: %q", tk.Notes)
	}
	// Delete through the menu with mouse confirmation.
	h.rclick(12, top+2)
	h.clickMenu("删除")
	h.wantScreen("确定删除「三（改）」")
	h.clickText("[ y 删除 ]")
	if !h.task("三（改）").Deleted() {
		t.Errorf("menu delete + confirm click")
	}
	// Clicking outside the menu closes it without acting.
	h.rclick(12, top)
	h.click(95, 25)
	if h.a.mode != modeNormal {
		t.Errorf("outside click should close the menu")
	}
	// Right click on empty space opens the general menu.
	h.rclick(12, top+10)
	h.wantScreen("新建任务", "快捷键帮助")
	h.keys("\x1b")
	// The m key opens the same menu for keyboard-only terminals.
	h.keys("m")
	if h.a.mode != modeMenu || !strings.Contains(h.screen(), "打开详情") {
		t.Errorf("m should open the task menu")
	}
}

func TestWheelScrollKeepsSelectionVisible(t *testing.T) {
	var titles []string
	for i := 1; i <= 60; i++ {
		titles = append(titles, fmt.Sprintf("任务%02d", i))
	}
	h := newTH(t, 100, 20, nil, titles...)
	lh := h.a.listHeight()
	h.wheel(true, 10, 5)
	h.wheel(true, 10, 5)
	if h.a.top != 6 {
		t.Errorf("wheel down x2 should scroll 6 rows, top=%d", h.a.top)
	}
	if h.a.cursor < h.a.top || h.a.cursor >= h.a.top+lh {
		t.Errorf("selection must stay visible: cursor=%d top=%d", h.a.cursor, h.a.top)
	}
	if h.selected() != "任务07" {
		t.Errorf("selection follows the viewport: %q", h.selected())
	}
	h.wantScreen("任务07", "任务20")
	h.wheel(false, 10, 5)
	if h.a.top != 3 {
		t.Errorf("wheel up: top=%d", h.a.top)
	}
	// Clicking a row after scrolling selects that row.
	h.click(10, h.a.bodyTop()+4)
	if h.selected() != "任务08" {
		t.Errorf("click after scroll: %q", h.selected())
	}
	h.keys("G")
	h.wantScreen("任务60", "60/60")
	// Page keys.
	h.keys("\x1b[5~")
	if h.a.cursor != 59-(lh-1) {
		t.Errorf("pgup: %d", h.a.cursor)
	}
	// Wheel over the detail pane scrolls the details, not the list.
	cur := h.a.cursor
	h.wheel(true, 80, 5)
	if h.a.cursor != cur {
		t.Errorf("detail wheel moved the list selection")
	}
}

func TestNarrowLayoutSwitchesPanes(t *testing.T) {
	h := newTH(t, 60, 20, nil, "窄屏任务")
	sc := h.screen()
	if strings.Contains(sc, "详情 · tab") {
		t.Errorf("narrow layout should show only the list:\n%s", sc)
	}
	h.keys("\r")
	h.wantScreen("详情 · tab", "窄屏任务", "[e 编辑]")
	// Common actions work without leaving the detail view.
	h.keys("x")
	if h.task("窄屏任务").Status != core.StatusDone || h.a.focus != paneDetail {
		t.Errorf("complete from detail view")
	}
	h.keys("\t")
	if h.a.focus != paneList {
		t.Errorf("tab back to the list")
	}
	h.keys("\x1b[C") // right arrow opens details too
	h.keys("\x1b")
	if h.a.focus != paneList {
		t.Errorf("esc returns to the list")
	}
	// Tiny terminals get a clear message instead of a broken layout.
	h.a.HandleEvent(Event{Kind: EvResize, W: 30, H: 8})
	h.wantScreen("终端窗口太小")
}

func TestExternalChangesAndConflicts(t *testing.T) {
	h := newTH(t, 100, 30, nil, "共享任务")
	other, err := core.Open(core.Options{DataDir: h.dir, Actor: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Create(core.NewTask{Title: "别处创建"}); err != nil {
		t.Fatal(err)
	}
	h.a.HandleEvent(Event{Kind: EvTick})
	h.wantScreen("别处创建")
	if h.selected() != "共享任务" {
		t.Errorf("reload must keep the selection: %q", h.selected())
	}

	// Edit while another entry point changes the same task.
	h.keys("e\x15我的标题")
	id := h.task("共享任务").ID
	newTitle := "别处的标题"
	if _, err := other.Update(id, core.TaskPatch{Title: &newTitle}); err != nil {
		t.Fatal(err)
	}
	h.a.HandleEvent(Event{Kind: EvTick})
	if h.a.form == nil || h.a.form.field(fieldTitle) != "我的标题" {
		t.Fatalf("a reload must not touch the open form")
	}
	h.keys("\x13") // ctrl+s
	h.wantScreen("冲突：该任务已在别处被修改")
	if h.task("别处的标题").ID != id {
		t.Fatalf("conflicting save must not overwrite silently")
	}
	h.keys("\x13") // explicit second save overwrites
	if h.task("我的标题").ID != id || h.a.mode != modeNormal {
		t.Errorf("second save should apply")
	}
}

func TestEmptyStateAndErrors(t *testing.T) {
	h := newTH(t, 100, 30, nil)
	h.wantScreen("还没有任务 — 按 a 新建")
	h.keys("e")
	h.wantScreen("没有选中的任务 — 按 a 新建")
	h.keys("u")
	h.wantScreen("没有可撤销的操作")
	h.keys("Z")
	h.wantScreen("按键 Z 未绑定任何操作 — 按 ? 查看快捷键")
	h.keys("/不存在\r")
	h.wantScreen("没有匹配的任务 — 按 c 清除筛选")
}

func TestSubtaskAndDetail(t *testing.T) {
	h := newTH(t, 100, 30, nil, "父任务")
	h.keys("N子任务一\r")
	child := h.task("子任务一")
	if child.ParentID != h.task("父任务").ID {
		t.Fatalf("subtask parent: %q", child.ParentID)
	}
	h.keys("g")
	h.wantScreen("子任务（1）", "历史（1）", "↳ 子任务一")
	h.keys("j")
	h.wantScreen("父任务   父任务")
}

func TestRenderStyles(t *testing.T) {
	h := newTH(t, 100, 30, nil, "高亮")
	sc := h.a.Render()
	if !sc.StyleAt(2, h.a.bodyTop()).Reverse {
		t.Errorf("the selected row is highlighted while the list has focus")
	}
	h.keys("\t")
	sc = h.a.Render()
	if sc.StyleAt(2, h.a.bodyTop()).Reverse || !sc.StyleAt(2, h.a.bodyTop()).Under {
		t.Errorf("the selected row stays marked while the detail pane has focus")
	}
	var b strings.Builder
	if err := sc.flush(&b, nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x1b[0;3") || strings.Contains(b.String(), ";3") && strings.Contains(b.String(), "m高亮") {
		t.Errorf("no colour codes with colour disabled")
	}
	b.Reset()
	sc.flush(&b, nil, true)
	if !strings.Contains(b.String(), ";36m") {
		t.Errorf("colour output expected")
	}
	// Only changed lines are redrawn.
	prev := sc
	h.keys("\t")
	b.Reset()
	h.a.Render().flush(&b, prev, true)
	if strings.Count(b.String(), "\x1b[") > 200 || strings.Contains(b.String(), "\x1b[2J") {
		t.Errorf("incremental redraw should not clear the screen")
	}
}
