package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Action is a named TUI command. Keys, the command palette, menus, mouse
// buttons and the help screen all refer to actions by these names.
type Action string

const (
	ActUp           Action = "up"
	ActDown         Action = "down"
	ActTop          Action = "top"
	ActBottom       Action = "bottom"
	ActPageUp       Action = "page_up"
	ActPageDown     Action = "page_down"
	ActToggleFocus  Action = "toggle_focus"
	ActOpenDetail   Action = "open_detail"
	ActBack         Action = "back"
	ActNew          Action = "new"
	ActNewSubtask   Action = "new_subtask"
	ActEdit         Action = "edit"
	ActToggleDone   Action = "toggle_done"
	ActStart        Action = "start"
	ActDelete       Action = "delete"
	ActArchive      Action = "archive"
	ActPriorityUp   Action = "priority_up"
	ActPriorityDown Action = "priority_down"
	ActMoveUp       Action = "move_up"
	ActMoveDown     Action = "move_down"
	ActSearch       Action = "search"
	ActFilterMenu   Action = "filter_menu"
	ActNextTab      Action = "next_filter"
	ActPrevTab      Action = "prev_filter"
	ActClearFilters Action = "clear_filters"
	ActSort         Action = "sort"
	ActReverse      Action = "reverse_sort"
	ActUndo         Action = "undo"
	ActReload       Action = "reload"
	ActPalette      Action = "palette"
	ActMenu         Action = "menu"
	ActHelp         Action = "help"
	ActQuit         Action = "quit"
)

// ActionInfo describes an action for help, the palette and `todo keys`.
type ActionInfo struct {
	Action Action   `json:"action"`
	Group  string   `json:"group"`
	Label  string   `json:"label"`
	Keys   []string `json:"keys"`
}

// actionTable lists every action in help order with its default keys.
var actionTable = []ActionInfo{
	{ActUp, "导航", "上移选中项", []string{"up", "k"}},
	{ActDown, "导航", "下移选中项", []string{"down", "j"}},
	{ActTop, "导航", "跳到第一项", []string{"home", "g"}},
	{ActBottom, "导航", "跳到最后一项", []string{"end", "G"}},
	{ActPageUp, "导航", "向上翻页", []string{"pgup", "ctrl+u"}},
	{ActPageDown, "导航", "向下翻页", []string{"pgdown", "ctrl+d"}},
	{ActToggleFocus, "导航", "切换列表区 / 详情区", []string{"tab"}},
	{ActOpenDetail, "导航", "打开任务详情", []string{"enter", "l", "right"}},
	{ActBack, "导航", "返回列表 / 清除搜索", []string{"esc", "h", "left"}},
	{ActNew, "任务", "新建任务", []string{"a", "n"}},
	{ActNewSubtask, "任务", "为选中任务新建子任务", []string{"N"}},
	{ActEdit, "任务", "编辑任务", []string{"e"}},
	{ActToggleDone, "任务", "完成 / 重新打开", []string{"x", "space"}},
	{ActStart, "任务", "标记进行中", []string{"s"}},
	{ActDelete, "任务", "删除任务", []string{"d", "delete"}},
	{ActArchive, "任务", "归档任务", []string{"A"}},
	{ActPriorityUp, "任务", "提高优先级", []string{"+", "="}},
	{ActPriorityDown, "任务", "降低优先级", []string{"-"}},
	{ActMoveUp, "任务", "手动排序：上移", []string{"K", "shift+up"}},
	{ActMoveDown, "任务", "手动排序：下移", []string{"J", "shift+down"}},
	{ActUndo, "任务", "撤销最近一次修改", []string{"u"}},
	{ActSearch, "查找", "搜索", []string{"/"}},
	{ActFilterMenu, "查找", "筛选菜单", []string{"f"}},
	{ActNextTab, "查找", "下一个状态筛选", []string{"]"}},
	{ActPrevTab, "查找", "上一个状态筛选", []string{"["}},
	{ActClearFilters, "查找", "清除搜索与筛选", []string{"c"}},
	{ActSort, "查找", "切换排序方式", []string{"o"}},
	{ActReverse, "查找", "反转排序", []string{"O"}},
	{ActPalette, "通用", "命令面板", []string{":", "ctrl+p"}},
	{ActMenu, "通用", "操作菜单（同右键）", []string{"m"}},
	{ActReload, "通用", "重新加载", []string{"R", "ctrl+r"}},
	{ActHelp, "通用", "快捷键帮助", []string{"?", "f1"}},
	{ActQuit, "通用", "退出", []string{"q", "ctrl+c"}},
}

func actionInfo(a Action) (ActionInfo, bool) {
	for _, ai := range actionTable {
		if ai.Action == a {
			return ai, true
		}
	}
	return ActionInfo{}, false
}

// KeymapFileName is the custom key binding file inside the data directory.
const KeymapFileName = "keybindings.json"

// Keymap maps keys to actions.
type Keymap struct {
	byKey    map[string]Action
	byAction map[Action][]string
	// Path is the configuration file the custom bindings came from.
	Path string
	// Custom lists actions whose keys were changed by the configuration.
	Custom []Action
}

// DefaultKeymap returns the built-in bindings.
func DefaultKeymap() *Keymap {
	km := &Keymap{byKey: map[string]Action{}, byAction: map[Action][]string{}}
	for _, ai := range actionTable {
		for _, k := range ai.Keys {
			km.bind(ai.Action, k)
		}
	}
	return km
}

func (km *Keymap) bind(a Action, key string) {
	if old, ok := km.byKey[key]; ok {
		km.byAction[old] = slices.DeleteFunc(km.byAction[old], func(k string) bool { return k == key })
	}
	km.byKey[key] = a
	km.byAction[a] = append(km.byAction[a], key)
}

// Lookup returns the action bound to a key.
func (km *Keymap) Lookup(key string) (Action, bool) {
	a, ok := km.byKey[key]
	return a, ok
}

// Keys returns the keys bound to an action, primary key first.
func (km *Keymap) Keys(a Action) []string { return km.byAction[a] }

// Key returns the primary key of an action for hints ("" if unbound).
func (km *Keymap) Key(a Action) string {
	if ks := km.byAction[a]; len(ks) > 0 {
		return ks[0]
	}
	return ""
}

// Bindings lists every action with its effective keys in help order.
func (km *Keymap) Bindings() []ActionInfo {
	out := make([]ActionInfo, 0, len(actionTable))
	for _, ai := range actionTable {
		ai.Keys = slices.Clone(km.byAction[ai.Action])
		if ai.Keys == nil {
			ai.Keys = []string{}
		}
		out = append(out, ai)
	}
	return out
}

// LoadKeymap reads custom bindings from path on top of the defaults. A
// missing file yields the defaults. The file is a JSON object mapping action
// names to a key or a list of keys, for example:
//
//	{"new": ["ctrl+n", "a"], "delete": "D", "archive": []}
//
// Listed actions replace their default keys; a key taken over by another
// action is removed from its old action. On error the defaults are returned
// together with the error so the caller can warn and continue.
func LoadKeymap(path string) (*Keymap, error) {
	km := DefaultKeymap()
	km.Path = path
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return km, nil
	}
	if err != nil {
		return km, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return km, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	custom := map[Action][]string{}
	var problems []string
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := Action(name)
		if _, ok := actionInfo(a); !ok {
			problems = append(problems, fmt.Sprintf("unknown action %q%s", name, suggestAction(name)))
			continue
		}
		var keys []string
		var one string
		if err := json.Unmarshal(cfg[name], &one); err == nil {
			keys = []string{one}
		} else if err := json.Unmarshal(cfg[name], &keys); err != nil {
			problems = append(problems, fmt.Sprintf("%s: want a key or a list of keys", name))
			continue
		}
		var norm []string
		for _, k := range keys {
			nk, err := NormalizeKey(k)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			norm = append(norm, nk)
		}
		custom[a] = norm
	}
	if len(problems) > 0 {
		return DefaultKeymapWithPath(path), fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	for _, name := range names {
		a := Action(name)
		for _, k := range km.byAction[a] {
			delete(km.byKey, k)
		}
		km.byAction[a] = nil
	}
	for _, name := range names {
		a := Action(name)
		for _, k := range custom[a] {
			km.bind(a, k)
		}
		km.Custom = append(km.Custom, a)
	}
	return km, nil
}

// DefaultKeymapWithPath returns the defaults remembering the config path.
func DefaultKeymapWithPath(path string) *Keymap {
	km := DefaultKeymap()
	km.Path = path
	return km
}

// DefaultKeymapJSON renders the default bindings in the configuration file
// format, as a starting point for customisation.
func DefaultKeymapJSON() []byte {
	var b strings.Builder
	b.WriteString("{\n")
	for i, ai := range actionTable {
		keys, _ := json.Marshal(ai.Keys)
		fmt.Fprintf(&b, "  %q: %s", ai.Action, keys)
		if i < len(actionTable)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

var namedKeys = []string{
	"enter", "tab", "esc", "space", "backspace", "delete", "insert", "up", "down", "left", "right",
	"home", "end", "pgup", "pgdown", "f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12",
}

var keyAliases = map[string]string{
	"return": "enter", "escape": "esc", "del": "delete", "pageup": "pgup", "pagedown": "pgdown",
	"pgdn": "pgdown", "bs": "backspace", "spc": "space", " ": "space", "control": "ctrl", "ctl": "ctrl",
	"option": "alt", "opt": "alt", "meta": "alt",
}

// NormalizeKey validates a key description such as "Ctrl+N", "alt+x",
// "shift+tab", "G", "?" or "f1" and returns its canonical form.
func NormalizeKey(k string) (string, error) {
	orig := k
	if k == "" {
		return "", fmt.Errorf("empty key")
	}
	if k == "+" || k == "-" || utf8.RuneCountInString(k) == 1 {
		if k == " " {
			return "space", nil
		}
		return k, nil
	}
	parts := strings.Split(k, "+")
	if strings.HasSuffix(k, "++") { // "ctrl++"
		parts = append(parts[:len(parts)-2], "+")
	}
	base := parts[len(parts)-1]
	mods := map[string]bool{}
	for _, m := range parts[:len(parts)-1] {
		m = strings.ToLower(strings.TrimSpace(m))
		if a, ok := keyAliases[m]; ok {
			m = a
		}
		if m != "ctrl" && m != "alt" && m != "shift" {
			return "", fmt.Errorf("invalid key %q: unknown modifier %q", orig, m)
		}
		mods[m] = true
	}
	if utf8.RuneCountInString(base) != 1 {
		base = strings.ToLower(base)
		if a, ok := keyAliases[base]; ok {
			base = a
		}
		if !slices.Contains(namedKeys, base) {
			return "", fmt.Errorf("invalid key %q", orig)
		}
	} else if mods["ctrl"] {
		base = strings.ToLower(base)
	} else if mods["shift"] && !mods["alt"] {
		// Terminals report shift+letter as the shifted character itself.
		return strings.ToUpper(base), nil
	}
	if base == " " {
		base = "space"
	}
	var out string
	for _, m := range []string{"ctrl", "alt", "shift"} {
		if mods[m] {
			out += m + "+"
		}
	}
	switch out + base {
	case "ctrl+i":
		return "", fmt.Errorf("invalid key %q: terminals send ctrl+i as tab", orig)
	case "ctrl+m", "ctrl+j":
		return "", fmt.Errorf("invalid key %q: terminals send it as enter", orig)
	case "ctrl+h":
		return "", fmt.Errorf("invalid key %q: terminals send it as backspace", orig)
	}
	return out + base, nil
}

func suggestAction(name string) string {
	best, bestD := "", 4
	for _, ai := range actionTable {
		if d := editDistance(name, string(ai.Action)); d < bestD {
			best, bestD = string(ai.Action), d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// EditDistance exposes the Levenshtein distance for "did you mean" hints.
func EditDistance(a, b string) int { return editDistance(a, b) }
