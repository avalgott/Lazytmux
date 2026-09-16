package gui

import (
	"context"

	"github.com/jesseduffield/gocui"
)

// FullScreenState tracks fullscreen passthrough mode: the selected session's
// pane is rendered over the whole terminal and all keyboard input is
// forwarded to it. Adapted from lazyclaude's fullscreen mode.
type FullScreenState struct {
	active bool
	target string // session name receiving forwarded keys
}

// Enter activates fullscreen mode for the given session.
func (fs *FullScreenState) Enter(name string) {
	fs.active = true
	fs.target = name
}

// Exit deactivates fullscreen mode.
func (fs *FullScreenState) Exit() {
	fs.active = false
	fs.target = ""
}

// IsActive reports whether fullscreen mode is on.
func (fs *FullScreenState) IsActive() bool {
	return fs.active
}

// Target returns the session receiving forwarded keys, or "".
func (fs *FullScreenState) Target() string {
	return fs.target
}

// specialKeyMap maps gocui Key constants to tmux send-keys names.
// Ctrl+C and Ctrl+D are deliberately absent: they are intercepted by view
// bindings on the fullscreen view (forward C-c / exit) before the Editor
// sees them.
var specialKeyMap = map[gocui.Key]string{
	gocui.KeySpace:      "Space",
	gocui.KeyTab:        "Tab",
	gocui.KeyBacktab:    "BTab",
	gocui.KeyBackspace:  "BSpace",
	gocui.KeyBackspace2: "BSpace",
	gocui.KeyArrowUp:    "Up",
	gocui.KeyArrowDown:  "Down",
	gocui.KeyArrowLeft:  "Left",
	gocui.KeyArrowRight: "Right",
	gocui.KeyHome:       "Home",
	gocui.KeyEnd:        "End",
	gocui.KeyPgup:       "PageUp",
	gocui.KeyPgdn:       "PageDown",
	gocui.KeyDelete:     "DC",
	gocui.KeyInsert:     "IC",
	gocui.KeyF1:         "F1",
	gocui.KeyF2:         "F2",
	gocui.KeyF3:         "F3",
	gocui.KeyF4:         "F4",
	gocui.KeyF5:         "F5",
	gocui.KeyF6:         "F6",
	gocui.KeyF7:         "F7",
	gocui.KeyF8:         "F8",
	gocui.KeyF9:         "F9",
	gocui.KeyF10:        "F10",
	gocui.KeyF11:        "F11",
	gocui.KeyF12:        "F12",
	gocui.KeyCtrlA:      "C-a",
	gocui.KeyCtrlB:      "C-b",
	gocui.KeyCtrlE:      "C-e",
	gocui.KeyCtrlF:      "C-f",
	gocui.KeyCtrlG:      "C-g",
	gocui.KeyCtrlH:      "C-h",
	gocui.KeyCtrlJ:      "C-j",
	gocui.KeyCtrlK:      "C-k",
	gocui.KeyCtrlL:      "C-l",
	gocui.KeyCtrlN:      "C-n",
	gocui.KeyCtrlP:      "C-p",
	gocui.KeyCtrlQ:      "C-q",
	gocui.KeyCtrlR:      "C-r",
	gocui.KeyCtrlS:      "C-s",
	gocui.KeyCtrlT:      "C-t",
	gocui.KeyCtrlU:      "C-u",
	gocui.KeyCtrlV:      "C-v",
	gocui.KeyCtrlW:      "C-w",
	gocui.KeyCtrlX:      "C-x",
	gocui.KeyCtrlY:      "C-y",
	gocui.KeyCtrlZ:      "C-z",
}

// inputEditor implements gocui.Editor to forward every keypress to the
// fullscreen target's pane. It never edits the view itself.
type inputEditor struct {
	app *App
}

// Edit is called by gocui for every keypress when the view is Editable.
// In scroll mode the keys move the scrollback viewport instead of being
// forwarded; otherwise runes are sent literally and special keys are
// translated via specialKeyMap.
func (e *inputEditor) Edit(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) bool {
	if !e.app.fullscreen.IsActive() {
		return false
	}
	if e.app.scroll.IsActive() {
		return e.scrollEdit(key, ch)
	}
	if key == gocui.KeyEnter {
		e.app.forwardTmuxKey("Enter")
		return true
	}
	if key == gocui.KeyEsc {
		e.app.forwardTmuxKey("Escape")
		return true
	}
	if ch != 0 {
		e.app.forwardLiteral(ch)
		return true
	}
	if name, ok := specialKeyMap[key]; ok {
		e.app.forwardTmuxKey(name)
		return true
	}
	return false
}

// scrollEdit handles keys while scroll mode is active. Rune keys reach the
// Editor because the fork skips rune bindings on editable views.
func (e *inputEditor) scrollEdit(key gocui.Key, ch rune) bool {
	app := e.app
	switch {
	case key == gocui.KeyEsc, ch == 'q', ch == 'Q':
		app.exitScrollMode()
		return true
	case ch == 'j', key == gocui.KeyArrowDown:
		app.scroll.Move(1)
	case ch == 'k', key == gocui.KeyArrowUp:
		app.scroll.Move(-1)
	case ch == 'g':
		app.scroll.Top()
	case ch == 'G':
		app.scroll.Bottom()
	default:
		return false
	}
	// Scrolling slices the in-memory snapshot; only a redraw is needed.
	// Redraw immediately so the status bar position updates without waiting
	// for the async fetch.
	app.g.Update(func(*gocui.Gui) error { return nil })
	return true
}

// forwardLiteral sends a rune as literal text to the fullscreen target and
// marks the preview stale so the next layout captures the pane's response.
func (a *App) forwardLiteral(ch rune) {
	target := a.fullscreen.Target()
	if target == "" {
		return
	}
	_ = a.svc.SendLiteral(context.Background(), target, string(ch))
	a.refreshPreviewSoon()
}

// forwardTmuxKey sends a tmux key name to the fullscreen target.
func (a *App) forwardTmuxKey(tmuxKey string) {
	target := a.fullscreen.Target()
	if target == "" {
		return
	}
	_ = a.svc.SendKeys(context.Background(), target, tmuxKey)
	a.refreshPreviewSoon()
}

// refreshPreviewSoon invalidates the preview timestamp and schedules a
// redraw so the forwarded key's effect shows up without waiting for the
// ticker.
func (a *App) refreshPreviewSoon() {
	a.preview.Lock()
	if !a.preview.Busy() {
		a.preview.InvalidateTimestamp()
	}
	a.preview.Unlock()
	a.g.Update(func(*gocui.Gui) error { return nil })
}
