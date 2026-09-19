package gui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/session"
)

// setupKeybindings registers all keybindings.
//
// gocui dispatch while an editable dialog input is focused: view-specific
// rune bindings are skipped and unmatched runes go to the Editor, so typing
// in a dialog never triggers global actions. Special keys (Enter/Esc/Tab)
// still match view bindings before the Editor. The confirm-delete dialog is
// NOT editable, so its rune keys (y/n/q) dispatch normally — and the global
// rune handlers below still guard on a.dialog being open, which covers that
// case (j/k/n/d/r are not bound on the confirm view).
func (a *App) setupKeybindings() error {
	// 1. Navigation.
	for _, ch := range []rune{'j', 'k'} {
		delta := 1
		if ch == 'k' {
			delta = -1
		}
		if err := a.g.SetKeybinding("", ch, gocui.ModNone, a.cursorMoveHandler(delta)); err != nil {
			return err
		}
	}
	for _, key := range []gocui.Key{gocui.KeyArrowDown, gocui.KeyArrowUp} {
		delta := 1
		if key == gocui.KeyArrowUp {
			delta = -1
		}
		if err := a.g.SetKeybinding("", key, gocui.ModNone, a.cursorMoveHandler(delta)); err != nil {
			return err
		}
	}

	// 2. Actions.
	if err := a.g.SetKeybinding("", 'n', gocui.ModNone, a.openCreateHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", 'd', gocui.ModNone, a.openConfirmDeleteHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", 'r', gocui.ModNone, a.openRenameHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", gocui.KeyEnter, gocui.ModNone, a.openFullScreenHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", 'a', gocui.ModNone, a.attach); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", 'q', gocui.ModNone, a.quit); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, a.quit); err != nil {
		return err
	}

	// Tab / Shift+Tab cycle focus between the sessions panel and the main
	// preview panel.
	if err := a.g.SetKeybinding("", gocui.KeyTab, gocui.ModNone, a.cycleFocusHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", gocui.KeyBacktab, gocui.ModNone, a.cycleFocusHandler); err != nil {
		return err
	}

	// 2b. Fullscreen view bindings. The fullscreen main view is Editable,
	// and gocui dispatches view-specific bindings for special keys before the
	// Editor, so these intercept Ctrl+D / Ctrl+O / Ctrl+\ / Ctrl+C while every
	// other key is forwarded by the inputEditor.
	if err := a.g.SetKeybinding("main", gocui.KeyCtrlD, gocui.ModNone, a.exitFullScreenHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("main", gocui.KeyCtrlBackslash, gocui.ModNone, a.exitFullScreenHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("main", gocui.KeyCtrlO, gocui.ModNone, a.forwardEOFHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("main", gocui.KeyCtrlC, gocui.ModNone, a.ctrlCHandler); err != nil {
		return err
	}

	// Ctrl+V toggles scrollback browsing in fullscreen.
	if err := a.g.SetKeybinding("main", gocui.KeyCtrlV, gocui.ModNone, a.toggleScrollHandler); err != nil {
		return err
	}

	// PageUp/PageDown: scroll the history in scroll mode, forwarded to the
	// pane otherwise (the view binding takes precedence over the Editor).
	for _, b := range []struct {
		key  gocui.Key
		name string
	}{
		{gocui.KeyPgup, "PageUp"},
		{gocui.KeyPgdn, "PageDown"},
	} {
		if err := a.g.SetKeybinding("main", b.key, gocui.ModNone, a.pageHandler(b.name)); err != nil {
			return err
		}
	}

	// Mouse wheel: scrolls the preview panel on the dashboard and enters
	// scroll mode in fullscreen (or forwards to a mouse-tracking pane).
	// View-scoped bindings fire first when the mouse is over the main view
	// and carry the actual mouse position for the pane forward; the global
	// ones remain the fallback where the location is irrelevant.
	if err := a.g.SetKeybinding("", gocui.MouseWheelUp, gocui.ModNone, a.wheelHandler(-3)); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("", gocui.MouseWheelDown, gocui.ModNone, a.wheelHandler(3)); err != nil {
		return err
	}
	for _, b := range []struct {
		key   gocui.Key
		delta int
	}{
		{gocui.MouseWheelUp, -3},
		{gocui.MouseWheelDown, 3},
	} {
		delta := b.delta
		if err := a.g.SetViewClickBinding(&gocui.ViewMouseBinding{
			ViewName: "main",
			Key:      b.key,
			Modifier: gocui.ModNone,
			Handler: func(opts gocui.ViewMouseBindingOpts) error {
				// The editable view clamps X to the rendered line width;
				// the raw viewport position is the pane coordinate.
				x, y := a.clampWheelCoords(opts.ViewportX, opts.ViewportY)
				a.wheelHandlerAt(delta, x, y)
				return nil
			},
		}); err != nil {
			return err
		}
	}

	// g/G browse the snapshot in the dashboard preview panel. In fullscreen
	// the editable main view skips rune view bindings, so the Editor keeps
	// handling g/G there (scrollEdit).
	if err := a.g.SetKeybinding("main", 'g', gocui.ModNone, a.previewScrollTopHandler); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("main", 'G', gocui.ModNone, a.previewScrollBottomHandler); err != nil {
		return err
	}

	// 3. Create dialog: Enter confirms, Esc cancels, Tab cycles fields.
	for _, name := range createFieldViews {
		if err := a.g.SetKeybinding(name, gocui.KeyEnter, gocui.ModNone, a.confirmCreate); err != nil {
			return err
		}
		if err := a.g.SetKeybinding(name, gocui.KeyEsc, gocui.ModNone, a.cancelDialog); err != nil {
			return err
		}
		if err := a.g.SetKeybinding(name, gocui.KeyTab, gocui.ModNone, a.nextCreateField); err != nil {
			return err
		}
	}

	// 4. Rename dialog.
	if err := a.g.SetKeybinding("rename-input", gocui.KeyEnter, gocui.ModNone, a.confirmRename); err != nil {
		return err
	}
	if err := a.g.SetKeybinding("rename-input", gocui.KeyEsc, gocui.ModNone, a.cancelDialog); err != nil {
		return err
	}

	// 5. Confirm-delete dialog: y kills, anything else cancels.
	if err := a.g.SetKeybinding("confirm-delete", 'y', gocui.ModNone, a.confirmDelete); err != nil {
		return err
	}
	for _, ch := range []rune{'n', 'q'} {
		if err := a.g.SetKeybinding("confirm-delete", ch, gocui.ModNone, a.cancelDialog); err != nil {
			return err
		}
	}
	if err := a.g.SetKeybinding("confirm-delete", gocui.KeyEsc, gocui.ModNone, a.cancelDialog); err != nil {
		return err
	}

	return nil
}

// cursorMoveHandler returns a handler that moves the session cursor. Ignores
// the key while a dialog is open (dialog input views have their own bindings).
func (a *App) cursorMoveHandler(delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.dialog != DialogNone {
			return nil
		}
		if a.focusMain {
			// j/k/arrows scroll the preview panel when it has focus.
			a.previewScrollMove(delta)
			return nil
		}
		a.moveCursor(delta)
		return nil
	}
}

// cycleFocusHandler toggles dashboard focus between the sessions list and the
// main preview panel. Guarded against dialogs (their inputs have their own
// bindings) and fullscreen (Tab is forwarded to the pane in live mode and
// must not leak into a focus change in scroll mode). An active preview scroll
// survives the focus change: the frozen snapshot keeps its position while the
// user peeks at the session list — it only resets on the documented events
// (selecting a different session, resizing, entering fullscreen, or the
// target disappearing).
func (a *App) cycleFocusHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone || a.fullscreen.IsActive() {
		return nil
	}
	a.focusMain = !a.focusMain
	// Apply the view focus immediately: the redraw is queued, but keys
	// arriving before it must already route by the new focus.
	_ = a.setDashboardFocus(g)
	a.g.Update(func(*gocui.Gui) error { return nil })
	return nil
}

func (a *App) previewScrollTopHandler(g *gocui.Gui, v *gocui.View) error {
	a.previewScrollTop()
	return nil
}

func (a *App) previewScrollBottomHandler(g *gocui.Gui, v *gocui.View) error {
	a.previewScrollBottom()
	return nil
}

func (a *App) openCreateHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone {
		return nil
	}
	a.createField = 0
	a.dialog = DialogCreate
	return nil
}

func (a *App) openRenameHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone {
		return nil
	}
	sess := a.currentSession()
	if sess == nil {
		return nil
	}
	if sess.Plan != nil {
		a.setStatus(fmt.Sprintf("session %q is part of the active plan and cannot be renamed", sess.Name))
		return nil
	}
	a.renameTarget = sess.Name
	a.dialog = DialogRename
	return nil
}

func (a *App) openConfirmDeleteHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone {
		return nil
	}
	sess := a.currentSession()
	if sess == nil {
		return nil
	}
	if sess.Plan != nil {
		a.setStatus(fmt.Sprintf("session %q is part of the active plan and cannot be deleted", sess.Name))
		return nil
	}
	a.confirmTarget = sess.Name
	a.dialog = DialogConfirmDelete
	return nil
}

// openFullScreenHandler enters fullscreen passthrough mode for the selected
// session (Enter).
func (a *App) openFullScreenHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone {
		return nil
	}
	a.enterFullScreen()
	return nil
}

// exitFullScreenHandler leaves fullscreen passthrough mode (Ctrl+D / Ctrl+\).
func (a *App) exitFullScreenHandler(g *gocui.Gui, v *gocui.View) error {
	if !a.fullscreen.IsActive() {
		return nil
	}
	a.exitFullScreen()
	return nil
}

// forwardEOFHandler sends a literal Ctrl+D to the pane (Ctrl+O) so shells and
// REPLs can still receive EOF while Ctrl+D is reserved for leaving fullscreen.
// A no-op while scroll mode is active: nothing may be forwarded to the pane
// while browsing its history.
func (a *App) forwardEOFHandler(g *gocui.Gui, v *gocui.View) error {
	if !a.fullscreen.IsActive() || a.scroll.IsActive() {
		return nil
	}
	a.forwardTmuxKey("C-d")
	return nil
}

// ctrlCHandler intercepts Ctrl+C: in fullscreen mode it is forwarded to the
// pane (so the pane's program receives SIGINT); on the dashboard it quits.
// While browsing the history it exits scroll mode back to the live view
// (same as Esc) instead of forwarding.
func (a *App) ctrlCHandler(g *gocui.Gui, v *gocui.View) error {
	if !a.fullscreen.IsActive() {
		return a.quit(g, v)
	}
	if a.scroll.IsActive() {
		a.exitScrollMode()
		return nil
	}
	a.forwardTmuxKey("C-c")
	return nil
}

// toggleScrollHandler toggles scrollback browsing in fullscreen (Ctrl+V).
func (a *App) toggleScrollHandler(g *gocui.Gui, v *gocui.View) error {
	if !a.fullscreen.IsActive() {
		return nil
	}
	if a.scroll.IsActive() {
		a.exitScrollMode()
	} else {
		a.enterScrollMode()
	}
	return nil
}

// pageHandler handles PageUp/PageDown in fullscreen: scroll the history in
// scroll mode, forward to the pane otherwise.
func (a *App) pageHandler(tmuxKey string) func(*gocui.Gui, *gocui.View) error {
	delta := -1
	if tmuxKey == "PageDown" {
		delta = 1
	}
	return func(g *gocui.Gui, v *gocui.View) error {
		if a.fullscreen.IsActive() {
			if a.scroll.IsActive() {
				a.scroll.Page(delta)
				a.g.Update(func(*gocui.Gui) error { return nil })
				return nil
			}
			a.forwardTmuxKey(tmuxKey)
			return nil
		}
		// Dashboard: the binding is view-scoped to "main", so this only runs
		// while the preview panel has focus.
		if a.dialog != DialogNone {
			return nil
		}
		// PageDown at the live bottom has nothing to browse; entering would
		// start a full history load that immediately exits again.
		if !a.previewScroll.IsActive() && delta > 0 {
			return nil
		}
		if !a.previewScroll.IsActive() {
			a.enterPreviewScroll()
		}
		if !a.previewScroll.IsActive() {
			return nil
		}
		a.previewScroll.Page(delta)
		// A downward gesture reaching the live bottom returns to the live
		// capture — loaded or not, so the result is independent of load timing.
		if a.previewScroll.offsetFromBottom == 0 && delta > 0 {
			a.exitPreviewScroll()
			return nil
		}
		a.g.Update(func(*gocui.Gui) error { return nil })
		return nil
	}
}

// wheelHandler handles the mouse wheel: in fullscreen it enters scroll mode
// (if needed) and scrolls — unless the pane's program runs in the alternate
// screen with SGR mouse tracking (e.g. Claude Code), in which case the wheel
// is forwarded to the pane as a real SGR mouse event so the program scrolls its own
// history. On the dashboard the wheel scrolls the preview panel.
func (a *App) wheelHandler(delta int) func(*gocui.Gui, *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		a.wheel(delta, 0, 0, false)
		return nil
	}
}

// wheelTmuxTimeout bounds the wheel path's tmux round-trips: a slow or
// wedged server must not freeze the whole interface per wheel notch.
const wheelTmuxTimeout = 250 * time.Millisecond

// wheelHandlerAt handles a view-scoped wheel event: the coordinates are the
// actual mouse position (content-relative), which SGR consumers use to pick
// the hovered widget — pane cursor coordinates would target the wrong one.
func (a *App) wheelHandlerAt(delta, x, y int) {
	a.wheel(delta, x, y, true)
}

func (a *App) wheel(delta, x, y int, hasPos bool) {
	if a.fullscreen.IsActive() {
		// The scroll-state check, the pane query/adoption, and the forward
		// are one fsMu section — recordPaneLocked rebinds the pane under the
		// same lock, so a rebind can never race the injection. The lock is
		// released before the scroll fallback (enterScrollMode takes it).
		a.fsMu.Lock()
		scrollActive := a.scroll.IsActive()
		if !scrollActive {
			forwarded := false
			if hasPos {
				target := a.fullscreen.Target()
				// Query the pane's input mode synchronously at dispatch
				// time: the pane may have stopped tracking mouse input
				// since the last observation (e.g. vim exited), and raw SGR
				// bytes must never reach a program that cannot parse them.
				// One tmux call per wheel — the same cost as any forwarded
				// key, and inherently ordered with keyboard input. The calls
				// carry a short timeout so a wedged tmux cannot freeze the
				// interface for the full client timeout on every wheel.
				ctx, cancel := context.WithTimeout(context.Background(), wheelTmuxTimeout)
				alt, sgr, cx, cy, pane, ferr := a.svc.PaneInputFlags(ctx, target)
				if ferr == nil && alt && sgr {
					// The synchronous query is authoritative: it resolved the
					// target to its current active pane at dispatch time, and
					// fsMu serializes it against every internal rebind. A
					// recorded binding from before a pane switch must not
					// divert the wheel into scroll mode — adopt the queried
					// pane (dropping the old pane's buffer, like a capture
					// that observes the switch) and send to it, so the first
					// wheel after a switch still forwards.
					cx, cy = x, y
					if pane != "" {
						a.buffersMu.Lock()
						if a.paneIDs[target] != pane {
							a.dropBufferLocked(target)
							a.paneIDs[target] = pane
							a.paneSeq[target] = a.captureSeq.Add(1)
						}
						a.buffersMu.Unlock()
						if werr := a.svc.ForwardMouseWheel(ctx, pane, delta < 0, cx, cy); werr == nil {
							forwarded = true
						}
					} else {
						// No pane ID (pre-pane_id tmux): the flags describe
						// the current pane; send by name.
						if werr := a.svc.ForwardMouseWheel(ctx, target, delta < 0, cx, cy); werr == nil {
							forwarded = true
						}
					}
				}
				cancel()
			}
			a.fsMu.Unlock()
			if forwarded {
				return
			}
			// Positionless events come from the global binding (the mouse
			// is over the status bar, not the pane). The view-scoped "main"
			// binding covers every wheel event actually over the pane —
			// ignore these rather than forwarding at the pane cursor.
			if !hasPos {
				return
			}
			// Wheel-down at the live bottom has nothing to browse; entering
			// would start a whole-history load that pins at the bottom.
			if delta > 0 {
				return
			}
			a.enterScrollMode()
			a.scroll.Move(delta)
			a.g.Update(func(*gocui.Gui) error { return nil })
			return
		}
		a.fsMu.Unlock()
		a.scroll.Move(delta)
		a.g.Update(func(*gocui.Gui) error { return nil })
		return
	}
	// Dashboard: the wheel scrolls the preview panel.
	if a.dialog != DialogNone {
		return
	}
	// Wheel-down at the live bottom has nothing to browse; entering would
	// start a full history load that immediately exits again.
	if !a.previewScroll.IsActive() && delta > 0 {
		return
	}
	if !a.previewScroll.IsActive() {
		a.enterPreviewScroll()
	}
	if !a.previewScroll.IsActive() {
		return
	}
	a.previewScroll.Move(delta)
	// A downward gesture reaching the live bottom returns to the live
	// capture — loaded or not, so the result is independent of load timing.
	if a.previewScroll.offsetFromBottom == 0 && delta > 0 {
		a.exitPreviewScroll()
		return
	}
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// clampWheelCoords bounds view mouse coordinates to the content area —
// gocui reports -1 on the top/left frame borders and the inner size on the
// right/bottom ones, which SGR consumers would discard as out of range.
func (a *App) clampWheelCoords(x, y int) (int, int) {
	v, err := a.g.View("main")
	if err != nil {
		return x, y
	}
	if w := v.InnerWidth(); w > 0 {
		x = clampInt(x, 0, w-1)
	}
	if h := v.InnerHeight(); h > 0 {
		y = clampInt(y, 0, h-1)
	}
	return x, y
}

// cancelDialog closes whichever dialog is active. Bound to Esc on all dialog
// views, and to n on the confirm-delete dialog.
func (a *App) cancelDialog(g *gocui.Gui, v *gocui.View) error {
	switch a.dialog {
	case DialogCreate:
		a.closeCreateDialog(g)
	case DialogRename:
		a.closeRenameDialog(g)
	case DialogConfirmDelete:
		a.closeConfirmDeleteDialog(g)
	}
	return nil
}

// nextCreateField cycles focus through the create dialog fields.
func (a *App) nextCreateField(g *gocui.Gui, v *gocui.View) error {
	a.createField = (a.createField + 1) % 3
	if _, err := g.SetCurrentView(createFieldViews[a.createField]); err != nil && !isUnknownView(err) {
		return err
	}
	return nil
}

// confirmCreate reads the three input fields and starts a new session.
// tmux rejects bad names; the error is shown in the options bar.
func (a *App) confirmCreate(g *gocui.Gui, v *gocui.View) error {
	nameView, dirView, cmdView, err := a.createDialogViews(g)
	if err != nil {
		return nil
	}
	name := strings.TrimSpace(nameView.TextArea.GetContent())
	dir := strings.TrimSpace(dirView.TextArea.GetContent())
	command := strings.TrimSpace(cmdView.TextArea.GetContent())

	if err := session.ValidateName(name); err != nil {
		a.setError(err.Error())
		return nil
	}

	a.closeCreateDialog(g)

	go func() {
		err := a.svc.Create(context.Background(), session.CreateOpts{
			Name:    name,
			Dir:     dir,
			Command: command,
		})
		a.g.Update(func(*gocui.Gui) error {
			if err != nil {
				a.setError(err.Error())
			} else {
				a.setStatus(fmt.Sprintf("Created %q", name))
			}
			return nil
		})
	}()
	return nil
}

// confirmRename renames the selected session. Validation happens before the
// dialog closes so errors appear inline.
func (a *App) confirmRename(g *gocui.Gui, v *gocui.View) error {
	oldName := a.renameTarget
	input, err := g.View("rename-input")
	if err != nil {
		return nil
	}
	newName := strings.TrimSpace(input.TextArea.GetContent())

	if err := session.ValidateName(newName); err != nil {
		a.setError(err.Error())
		return nil
	}

	a.closeRenameDialog(g)
	if newName == "" || newName == oldName {
		return nil
	}

	go func() {
		err := a.svc.Rename(context.Background(), oldName, newName)
		a.g.Update(func(*gocui.Gui) error {
			if err != nil {
				a.setError(err.Error())
			} else {
				a.setStatus(fmt.Sprintf("Renamed %q to %q", oldName, newName))
			}
			return nil
		})
	}()
	return nil
}

// confirmDelete kills the selected session after y confirmation.
func (a *App) confirmDelete(g *gocui.Gui, v *gocui.View) error {
	name := a.confirmTarget
	a.closeConfirmDeleteDialog(g)
	if name == "" {
		return nil
	}

	go func() {
		err := a.svc.Kill(context.Background(), name)
		a.g.Update(func(*gocui.Gui) error {
			if err != nil {
				a.setError(err.Error())
			} else {
				a.setStatus(fmt.Sprintf("Killed %q", name))
			}
			return nil
		})
	}()
	return nil
}

// createDialogViews resolves the three create dialog views, returning an
// error if any is missing (dialog was closed concurrently).
func (a *App) createDialogViews(g *gocui.Gui) (*gocui.View, *gocui.View, *gocui.View, error) {
	nameView, err := g.View(createFieldViews[0])
	if err != nil {
		return nil, nil, nil, err
	}
	dirView, err := g.View(createFieldViews[1])
	if err != nil {
		return nil, nil, nil, err
	}
	cmdView, err := g.View(createFieldViews[2])
	if err != nil {
		return nil, nil, nil, err
	}
	return nameView, dirView, cmdView, nil
}
