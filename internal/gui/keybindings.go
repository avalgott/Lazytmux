package gui

import (
	"context"
	"fmt"
	"strings"

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
// must not leak into a focus change in scroll mode). Focus changes end
// preview scrolling: the options bar advertises j/k as scrolling only while
// the mode is active.
func (a *App) cycleFocusHandler(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone || a.fullscreen.IsActive() {
		return nil
	}
	if a.previewScroll.IsActive() {
		a.exitPreviewScroll()
	}
	a.focusMain = !a.focusMain
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

// wheelHandlerAt handles a view-scoped wheel event: the coordinates are the
// actual mouse position (content-relative), which SGR consumers use to pick
// the hovered widget — pane cursor coordinates would target the wrong one.
func (a *App) wheelHandlerAt(delta, x, y int) {
	a.wheel(delta, x, y, true)
}

// wheelDecision is what decideFullscreenWheel concluded.
type wheelDecision int

const (
	wheelForwarded wheelDecision = iota // the pane's program received the wheel
	wheelIgnored                        // nothing to do (wheel-down at the live bottom)
	wheelFallback                       // enter lazytmux scroll mode instead
)

func (a *App) wheel(delta, x, y int, hasPos bool) {
	if a.fullscreen.IsActive() {
		if a.scroll.IsActive() {
			a.scroll.Move(delta)
			a.g.Update(func(*gocui.Gui) error { return nil })
			return
		}
		// Positionless events come from the global binding (the mouse is
		// over the status bar, not the pane). The view-scoped "main"
		// binding covers every wheel event actually over the pane — ignore
		// these rather than forwarding at the pane cursor.
		if !hasPos {
			return
		}
		// The tmux round-trips run off the event loop through an ordered
		// worker: each can block for the client timeout, and queued wheel
		// input must neither freeze the UI nor be reordered.
		a.enqueueWheelTask(wheelTask{
			target:  a.fullscreen.Target(),
			fsGen:   a.fullscreenGen.Load(),
			wGen:    a.wheelGen.Load(),
			exitGen: a.wheelExitGen.Load(),
			delta:   delta, x: x, y: y, hasPos: hasPos,
		})
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

// decideFullscreenWheel runs the tmux queries for a live fullscreen wheel
// event and returns the decision without touching UI state — safe to run
// from a goroutine.
func (a *App) decideFullscreenWheel(target string, fsGen, wGen uint64, delta, x, y int, hasPos bool) (wheelDecision, error) {
	alt, mouse, cx, cy, err := a.svc.PaneInputFlags(context.Background(), target)
	if err == nil && alt && mouse {
		// The generation checks and the send are serialized with fullscreen
		// and scroll-mode transitions by fsMu: exit cannot race with
		// injection, and entering scroll mode (which must suppress pane
		// input) invalidates the pending forward.
		a.fsMu.Lock()
		defer a.fsMu.Unlock()
		if a.fullscreenGen.Load() != fsGen || a.wheelGen.Load() != wGen {
			return wheelIgnored, nil
		}
		if hasPos {
			cx, cy = x, y
		}
		ferr := a.svc.ForwardMouseWheel(context.Background(), target, delta < 0, cx, cy)
		if ferr == nil {
			return wheelForwarded, nil
		}
		// A failed forward (dead pane, tmux error) must not be a silent
		// no-op: fall back to lazytmux scroll mode.
		return wheelFallback, fmt.Errorf("forward wheel: %v", ferr)
	}
	// Wheel-down at the live bottom has nothing to browse; entering would
	// start a whole-history load that pins at the bottom.
	if delta > 0 {
		return wheelIgnored, nil
	}
	return wheelFallback, nil
}

// wheelTask is one queued wheel event, with the generations captured at
// enqueue time.
type wheelTask struct {
	target  string
	fsGen   uint64
	wGen    uint64
	exitGen uint64
	delta   int
	x, y    int
	hasPos  bool
}

// enqueueWheelTask appends a wheel event to the ordered queue; when the
// queue is full (a very slow tmux), the newest event is dropped rather than
// blocking the event loop.
func (a *App) enqueueWheelTask(t wheelTask) {
	select {
	case a.wheelQueue <- t:
	default:
	}
}

// wheelWorker drains the wheel queue in arrival order — independent
// goroutines would let delayed flag queries forward events out of order.
func (a *App) wheelWorker() {
	for t := range a.wheelQueue {
		a.processWheelTask(t)
	}
}

// processWheelTask runs one wheel event: the tmux queries off the event
// loop, the state change marshaled back onto it.
func (a *App) processWheelTask(t wheelTask) {
	d, actErr := a.decideFullscreenWheel(t.target, t.fsGen, t.wGen, t.delta, t.x, t.y, t.hasPos)
	if d == wheelForwarded || d == wheelIgnored {
		return
	}
	a.g.Update(func(*gocui.Gui) error {
		a.applyWheelFallbackIfCurrent(t, d, actErr)
		return nil
	})
}

// applyWheelFallbackIfCurrent applies a fallback decision on the event loop
// when the initiating state is still current. Split out so tests can drive
// it directly (headless mode never runs gui.Update).
func (a *App) applyWheelFallbackIfCurrent(t wheelTask, d wheelDecision, actErr error) {
	if a.wheelFallbackCurrent(t.fsGen, t.exitGen, t.target) {
		a.performWheelAction(d, t.delta, actErr)
	}
}

// wheelFallbackCurrent reports whether the state that initiated a wheel
// decision is still the live one — a slow query must not apply its fallback
// to a session the user has since left, or after the user has exited scroll
// mode while the query was in flight. Entering scroll mode does NOT
// invalidate queued fallbacks: their deltas still accumulate in order.
func (a *App) wheelFallbackCurrent(fsGen, exitGen uint64, target string) bool {
	return a.fullscreenGen.Load() == fsGen && a.wheelExitGen.Load() == exitGen &&
		a.fullscreen.IsActive() && a.fullscreen.Target() == target
}

// performWheelAction applies a wheel decision on the event loop.
func (a *App) performWheelAction(d wheelDecision, delta int, actErr error) {
	if actErr != nil {
		a.setError(actErr.Error())
	}
	if d == wheelFallback {
		a.enterScrollMode()
		a.scroll.Move(delta)
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
