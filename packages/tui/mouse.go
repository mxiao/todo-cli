package tui

import "time"

const doubleClick = 500 * time.Millisecond

// handleMouse maps clicks, right clicks and the wheel onto the same actions
// the keyboard uses. Hit testing uses the regions of the last Render.
func (a *App) handleMouse(ev Event) {
	if ev.Action != MousePress {
		return // releases and drags carry no meaning here
	}
	if len(a.regions) == 0 {
		a.Render()
	}
	r := a.hit(ev.X, ev.Y)
	kind := regionKind(0)
	if r != nil {
		kind = r.kind
	}
	switch a.mode {
	case modeMenu:
		switch {
		case kind == regMenuItem && (ev.Button == MouseLeft || ev.Button == MouseRight):
			a.runMenuItem(r.index)
		case kind == regOverlay || ev.Button == WheelUp || ev.Button == WheelDown:
		default:
			// Clicking elsewhere closes the menu; a right click reopens it there.
			a.closeMenu()
			if ev.Button == MouseRight {
				a.Render()
				a.handleMouse(ev)
			}
		}
		return
	case modePalette:
		switch {
		case kind == regPaletteItem && ev.Button == MouseLeft:
			a.palette.sel = r.index
			a.runPaletteCmd(&a.palette.matches[r.index], nil)
		case ev.Button == WheelUp:
			a.palette.sel = max(a.palette.sel-1, 0)
		case ev.Button == WheelDown:
			a.palette.sel = min(a.palette.sel+1, max(len(a.palette.matches)-1, 0))
		case kind != regOverlay && kind != regPaletteItem:
			a.closePalette()
		}
		return
	case modeForm:
		if ev.Button != MouseLeft {
			return
		}
		switch kind {
		case regFormField:
			a.form.focus = r.index // content of every field is kept
		case regFormSave:
			a.saveForm()
		case regFormCancel:
			a.closeForm("已取消编辑")
		}
		return
	case modeConfirm:
		if ev.Button != MouseLeft {
			return
		}
		switch kind {
		case regConfirmYes:
			a.answerConfirm(true)
		case regConfirmNo:
			a.answerConfirm(false)
		}
		return
	case modeHelp:
		switch {
		case ev.Button == WheelUp:
			a.helpTop = max(a.helpTop-3, 0)
		case ev.Button == WheelDown:
			a.helpTop += 3
		case kind == regHelpClose || kind != regOverlay:
			a.mode = modeNormal
		}
		return
	case modePrompt:
		return // modal: the typed text must not be disturbed
	case modeSearch:
		if kind != regTask {
			return
		}
		a.mode = modeNormal // clicking a result accepts the search
	}

	switch ev.Button {
	case WheelUp, WheelDown:
		delta := 3
		if ev.Button == WheelUp {
			delta = -3
		}
		inBody := ev.Y >= a.bodyTop() && ev.Y < a.bodyTop()+a.listHeight()
		inDetail := (a.wide() && ev.X > a.listWidth()) || (!a.wide() && a.focus == paneDetail)
		if inBody && inDetail {
			a.detailScroll = max(a.detailScroll+delta, 0)
			return
		}
		a.scrollList(delta)
	case MouseLeft:
		switch kind {
		case regTask:
			id := a.tasks[r.index].ID
			now := a.clock()
			double := id == a.lastClickID && now.Sub(a.lastClick) < doubleClick
			a.selectIndex(r.index)
			a.focus = paneList
			if double {
				a.openDetail()
				a.lastClickID = ""
			} else {
				a.lastClick, a.lastClickID = now, id
			}
		case regTab:
			a.setTab(r.index)
		case regButton:
			a.do(r.action)
		case regDetail:
			a.focus = paneDetail
		case regList:
			a.focus = paneList
		}
	case MouseRight:
		switch kind {
		case regTask:
			a.selectIndex(r.index)
			a.focus = paneList
			a.openContextMenu(ev.X, ev.Y)
		case regDetail, regButton:
			if a.Selected() != nil {
				a.openContextMenu(ev.X, ev.Y)
				return
			}
			a.openGeneralMenu(ev.X, ev.Y)
		default:
			a.openGeneralMenu(ev.X, ev.Y)
		}
	}
}

// scrollList moves the viewport; the selection follows only as far as
// needed to stay visible, so keyboard and mouse agree on the selected task.
func (a *App) scrollList(delta int) {
	h := a.listHeight()
	a.top = max(0, min(a.top+delta, max(len(a.tasks)-h, 0)))
	a.focus = paneList
	switch {
	case a.cursor < a.top:
		a.selectIndex(a.top)
	case a.cursor >= a.top+h:
		a.selectIndex(a.top + h - 1)
	}
}
