package gui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/gui/presentation"
)

// roundedFrame is the set of runes for rounded border corners.
// Order: horizontal, vertical, top-left, top-right, bottom-left, bottom-right.
var roundedFrame = []rune{'─', '│', '╭', '╮', '╰', '╯'}

// setRoundedFrame applies rounded border corners to a gocui view.
func setRoundedFrame(v *gocui.View) {
	v.FrameRunes = roundedFrame
}

// Rect is a simple rectangle from (X0, Y0) to (X1, Y1) inclusive,
// matching gocui's SetView coordinate convention.
type Rect struct {
	X0, Y0, X1, Y1 int
}

// Width returns the number of columns the rectangle spans.
// gocui coordinates are inclusive on both ends, so width is X1-X0+1.
func (r Rect) Width() int {
	return r.X1 - r.X0 + 1
}

// Height returns the number of rows the rectangle spans.
func (r Rect) Height() int {
	return r.Y1 - r.Y0 + 1
}

// Layout holds pre-computed view positions for the main screen.
type Layout struct {
	Sessions Rect // upper-left panel
	Logs     Rect // middle-left panel (status/error messages)
	Version  Rect // lower-left strip (installed app version)
	Main     Rect // right panel (preview)
	Options  Rect // bottom bar
}

// ComputeLayout calculates view positions for the given terminal size.
// The split column logic mirrors lazyclaude's ComputeLayout: the left column
// is a third of the terminal, clamped so both columns remain usable. The
// left column stacks the sessions panel (upper two thirds), the logs panel,
// and a three-row version strip pinned above the options bar, like
// lazyclaude's sessions/plugins/logs stack.
//
// gocui quirk: content is always drawn at (x0+1, y0+1) and the writable area
// is Width-2 / Height-2, so every view needs 2 extra rows/cols. The options
// bar therefore spans maxY-2..maxY: one unused row, then its single content
// row (the hints) lands on the last screen row. maxY itself is off screen
// (valid rows are 0..maxY-1) but harmless, out-of-range cells are swallowed
// by the draw loop.
func ComputeLayout(width, height int) Layout {
	maxX := width
	maxY := height

	splitX := maxX / 3
	if splitX < 20 {
		splitX = 20
	}
	if splitX >= maxX-10 {
		splitX = maxX / 2
	}

	leftH := maxY - 2 // rows available to the left-column panels
	sessY1 := (leftH * 2) / 3
	logsY0 := sessY1 + 1
	// The version strip is exactly three rows, top border (with the
	// title), the version content, bottom border, pinned to the bottom of
	// the left column, right above the options bar. On short terminals the
	// strip is omitted entirely (a zero rect), so the session list and the
	// logs keep usable inner rows instead.
	showVersion := maxY >= 12
	logsY1 := maxY - 2
	if showVersion {
		logsY1 = maxY - 5
	}
	// Keep the logs panel usable on very short terminals.
	if logsY0 > logsY1-2 {
		logsY0 = logsY1 - 2
		sessY1 = logsY0 - 1
	}

	var version Rect
	if showVersion {
		version = Rect{X0: 0, Y0: maxY - 4, X1: splitX - 1, Y1: maxY - 2}
	}

	return Layout{
		Sessions: Rect{X0: 0, Y0: 0, X1: splitX - 1, Y1: sessY1},
		Logs:     Rect{X0: 0, Y0: logsY0, X1: splitX - 1, Y1: logsY1},
		Version:  version,
		Main:     Rect{X0: splitX, Y0: 0, X1: maxX - 1, Y1: maxY - 2},
		Options:  Rect{X0: 0, Y0: maxY - 2, X1: maxX - 1, Y1: maxY},
	}
}

// blankScreen fills the whole terminal with spaces. gocui's draw() only
// repaints view content areas (always inset by one row/col from the view
// rect), so cells outside the current layout's rects keep stale pixels
// otherwise.
func blankScreen(g *gocui.Gui, maxX, maxY int) {
	for y := 0; y < maxY; y++ {
		for x := 0; x < maxX; x++ {
			_ = g.SetRune(x, y, ' ', gocui.ColorDefault, gocui.ColorDefault)
		}
	}
}

// layout is the gocui manager function: it (re)creates the three views and
// any active dialog overlay, then routes focus.
func (a *App) layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()

	// Detect terminal resize -> clear the preview cache so the next render
	// captures at the new dimensions, and blank the whole screen. Blanking
	// matters because intermediate layouts drawn during the resize sequence
	// can leave border cells outside the final view rects, and the fork's
	// frameless views never clear the border column (content is always drawn
	// at x0+1).
	if maxX != a.lastWidth || maxY != a.lastHeight {
		a.preview.Invalidate()
		a.lastWidth = maxX
		a.lastHeight = maxY
		// The pane geometry changed: the no-scrollback verdict no longer
		// describes the current screen.
		a.fullscreenNoScrollback = false
		blankScreen(g, maxX, maxY)
		// The scroll viewport dimensions are tied to the pane geometry;
		// leave scroll mode so the next entry recomputes them at the new
		// size (the live preview resizes correctly on its own).
		if a.scroll.IsActive() {
			a.scroll.Exit()
		}
		if a.previewScroll.IsActive() {
			a.previewScroll.Exit()
			a.previewScrollTarget = ""
		}
	}

	// Same blanking when entering or leaving fullscreen: the dashboard and
	// fullscreen layouts share no view rects, so pixels from the previous
	// mode (e.g. the sessions panel's top frame at row 0, which the frameless
	// fullscreen view never touches) would otherwise linger.
	if a.fullscreen.IsActive() != a.lastFullscreen {
		a.lastFullscreen = a.fullscreen.IsActive()
		blankScreen(g, maxX, maxY)
	}

	if a.fullscreen.IsActive() {
		return a.layoutFullScreen(g, maxX, maxY)
	}

	if err := a.layoutMain(g, maxX, maxY); err != nil {
		return err
	}
	return a.layoutDialog(g, maxX, maxY)
}

// setDashboardFocus gives focus to the sessions panel or the main preview
// panel, depending on the Tab-focus state.
func (a *App) setDashboardFocus(g *gocui.Gui) error {
	name := "sessions"
	if a.focusMain {
		name = "main"
	}
	if _, err := g.SetCurrentView(name); err != nil && !isUnknownView(err) {
		return err
	}
	g.Cursor = false
	return nil
}

func (a *App) layoutMain(g *gocui.Gui, maxX, maxY int) error {
	g.DeleteView("fullscreen-bar")     // clean up after fullscreen mode
	g.DeleteView("fullscreen-command") // clean up after fullscreen mode

	l := ComputeLayout(maxX, maxY)
	a.clampCursor()

	// Sessions view (left panel)
	v, err := g.SetView("sessions", l.Sessions.X0, l.Sessions.Y0, l.Sessions.X1, l.Sessions.Y1, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	setRoundedFrame(v)
	v.Title = fmt.Sprintf(" Sessions (%d) ", len(a.sessions))
	v.Highlight = true
	v.SelBgColor = gocui.Get256Color(24)
	v.SelFgColor = gocui.ColorWhite
	v.Clear()
	renderSessions(v, a.sessions, a.cursor)

	// Logs view (lower left), recent status/error messages
	vlog, err := g.SetView("logs", l.Logs.X0, l.Logs.Y0, l.Logs.X1, l.Logs.Y1, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	setRoundedFrame(vlog)
	vlog.Title = " Logs "
	vlog.Wrap = true
	vlog.Clear()
	renderLogs(vlog, a.logs)

	// Version strip (bottom of the left column): the installed app version.
	// Skipped on short terminals, where the strip is a zero rect.
	if l.Version.Y1 > l.Version.Y0 {
		vver, err := g.SetView("version", l.Version.X0, l.Version.Y0, l.Version.X1, l.Version.Y1, 0)
		if err != nil && !isUnknownView(err) {
			return err
		}
		setRoundedFrame(vver)
		vver.Title = " Version "
		vver.Clear()
		fmt.Fprintf(vver, " %s", a.version)
	} else {
		// The terminal shrank below the strip threshold: a version view
		// from the taller layout would otherwise stay registered at its
		// old coordinates, overlapping the compact panels.
		g.DeleteView("version")
	}

	// Main panel (right side), live preview of the selected session
	v3, err := g.SetView("main", l.Main.X0, l.Main.Y0, l.Main.X1, l.Main.Y1, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	setRoundedFrame(v3)
	v3.Wrap = false
	v3.Editable = false
	v3.Clear()
	if a.previewScroll.IsActive() {
		// Browsing must not stop observing the pane: the capture tick keeps
		// live captures (and the synthetic-buffer feeds) running while the
		// frozen snapshot is shown.
		if sess := a.currentSession(); sess != nil {
			a.previewCaptureTick(v3, sess)
		}
		a.renderPreviewScroll(v3)
	} else {
		a.renderPreview(v3)
	}

	// Options bar (bottom, frameless): keybinding hints
	v4, err := g.SetView("options", l.Options.X0, l.Options.Y0, l.Options.X1, l.Options.Y1, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	v4.Frame = false
	v4.Clear()
	a.renderOptionsBar(v4)

	// Focus priority: dialog > (Tab-focus state: main panel or sessions).
	if a.dialog == DialogNone {
		if err := a.setDashboardFocus(g); err != nil && !isUnknownView(err) {
			return err
		}
	}
	return nil
}

// wrapCommandLines wraps the configured command into at most 3 display rows
// for the command panel, breaking at word boundaries at the given display
// width (long unbroken tokens break at the width). When the command needs
// more than 3 rows, the last visible row is truncated with an ellipsis.
func wrapCommandLines(cmd string, width int) []string {
	if width < 1 {
		width = 1
	}
	cmd = strings.TrimRight(cmd, "\n")
	lines := strings.Split(ansi.Hardwrap(cmd, width, false), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
		lines[2] = ansi.Truncate(lines[2], width-1, "…")
	}
	return lines
}

// fullscreenPlanCommand returns the YAML-configured command of the fullscreen
// target, or "" when the target is ad-hoc or its plan entry has no command.
// The session list keeps refreshing every 300ms even in fullscreen, so read
// it fresh on every layout rather than caching, the panel always shows the
// plan command, never what the pane is currently running.
func (a *App) fullscreenPlanCommand() string {
	target := a.fullscreen.Target()
	if target == "" {
		return ""
	}
	for _, s := range a.sessions {
		if s.Name == target && s.Plan != nil {
			return s.Plan.Command
		}
	}
	return ""
}

// layoutFullScreen lays out the passthrough mode: the main view fills the
// terminal (frameless) and forwards every key to the target session's pane;
// a status bar at the bottom shows the mode hints. A planned session with a
// configured command additionally gets a framed Command panel above the bar.
func (a *App) layoutFullScreen(g *gocui.Gui, maxX, maxY int) error {
	// Remove split-panel views so only the fullscreen view remains.
	g.DeleteView("sessions")
	g.DeleteView("logs")
	g.DeleteView("version")
	g.DeleteView("options")

	// Command panel geometry: a framed view with N content rows spans N+2
	// rows (the fork draws content at y0+1), and its bottom frame lands on
	// maxY-3, the row directly above the status bar (maxY-2..maxY), so
	// top = maxY-4-N. On very short terminals the panel shrinks and finally
	// disappears rather than starve the main view.
	cmd := a.fullscreenPlanCommand()
	cmdTop := 0
	if cmd != "" {
		lines := wrapCommandLines(cmd, maxX-2)
		rows := len(lines)
		if maxRows := maxY - 11; rows > maxRows {
			rows = maxRows
		}
		if rows >= 1 {
			cmdTop = maxY - 4 - rows
			vc, err := g.SetView("fullscreen-command", 0, cmdTop, maxX-1, maxY-3, 0)
			if err != nil && !isUnknownView(err) {
				return err
			}
			setRoundedFrame(vc)
			vc.Title = " Command "
			vc.Wrap = false
			vc.Editable = false
			vc.Clear()
			for _, l := range lines[:rows] {
				fmt.Fprintln(vc, " "+l)
			}
		}
	}
	if cmdTop == 0 {
		// No panel this layout, a previous target's panel must not linger.
		g.DeleteView("fullscreen-command")
	}

	mainY1 := maxY - 2
	if cmdTop > 0 {
		mainY1 = cmdTop - 1
	}
	v, err := g.SetView("main", 0, 0, maxX-1, mainY1, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	// Framed, like lazyclaude's fullscreen: the rounded border with the
	// session title occupies the view's edge rows (the fork draws content at
	// y0+1 either way, so the frame costs no content rows and removes the
	// awkward blank top row of a frameless view).
	setRoundedFrame(v)
	v.Title = " " + a.fullscreen.Target() + " "
	v.Wrap = false
	v.Editable = true
	if a.editor == nil {
		a.editor = &inputEditor{app: a}
	}
	v.Editor = a.editor
	v.Clear()
	a.resizeFullScreenTarget(v)
	if a.scroll.IsActive() {
		// Same as the dashboard preview: keep live captures (and the
		// synthetic buffer) fed while the scroll snapshot is shown.
		if sess := a.currentSession(); sess != nil {
			a.previewCaptureTick(v, sess)
		}
		a.renderScrollContent(v)
	} else {
		a.renderPreview(v)
	}

	// Status bar (bottom, frameless): session name + mode hints.
	v2, err := g.SetView("fullscreen-bar", 0, maxY-2, maxX-1, maxY, 0)
	if err != nil && !isUnknownView(err) {
		return err
	}
	v2.Frame = false
	v2.Clear()
	if a.scroll.IsActive() {
		fmt.Fprint(v2, a.scrollStatusText())
	} else {
		name := a.fullscreen.Target()
		fmt.Fprint(v2, " "+presentation.Bold+name+presentation.Reset+"  "+
			presentation.StyledKey("ctrl+d", "back")+"  "+
			presentation.StyledKey("ctrl+o", "eof")+"  "+
			presentation.StyledKey("ctrl+v", "scroll")+"  "+
			presentation.StyledKey("ctrl+\\", "back"))
		// The pane's program keeps its own scrollback (alternate screen):
		// the wheel goes to the program, lazytmux has nothing to browse.
		if a.fullscreenNoScrollback {
			a.buffersMu.Lock()
			currentPane := a.paneIDs[a.fullscreen.Target()]
			a.buffersMu.Unlock()
			// The verdict goes stale the moment the synthetic buffer gains
			// history, the badge must not keep claiming otherwise.
			if currentPane == a.fullscreenNoScrollbackPane && !a.bufferHasHistory(a.fullscreen.Target()) {
				fmt.Fprint(v2, "  "+presentation.Dim+"no scrollback"+presentation.Reset)
			}
		}
	}

	g.Cursor = true
	if _, err := g.SetCurrentView("main"); err != nil && !isUnknownView(err) {
		return err
	}
	return nil
}

// resizeFullScreenTarget sizes the target session's window to fill the
// fullscreen view. capture-pane returns the pane's real size, and sessions
// created detached (or via `n`) keep tmux's default 80x24 window forever
// unless something attaches to them, so without this the fullscreen content
// stays a small box no matter how large the terminal is.
//
// The window is sized to exactly the view dimensions (like lazyclaude), so
// the pane and the view agree 1:1, full-screen programs (e.g. Claude Code)
// lay out to the exact space the view shows, and no row is clipped. With the
// session's status bar on, the pane is one row shorter and still fits
// entirely.
//
// Runs once per (target, size) pair; the resize itself happens in a
// goroutine so the event loop never blocks on tmux.
func (a *App) resizeFullScreenTarget(v *gocui.View) {
	target := a.fullscreen.Target()
	if target == "" {
		return
	}
	previewW := v.InnerWidth()
	previewH := v.InnerHeight()
	if previewW < 1 {
		previewW = 1
	}
	if previewH < 1 {
		previewH = 1
	}
	if target == a.lastResizeName && previewW == a.lastResizeW && previewH == a.lastResizeH {
		return
	}
	a.lastResizeName = target
	a.lastResizeW = previewW
	a.lastResizeH = previewH

	// Force a fresh capture so the pane's new size shows up quickly.
	a.preview.Invalidate()

	go func() {
		_ = a.svc.ResizeWindow(context.Background(), target, previewW, previewH)
		a.g.Update(func(*gocui.Gui) error {
			// The resize may have completed after a scroll snapshot was
			// captured at the old pane geometry (the terminal itself did not
			// change, so the layout resize detection cannot help). Reload the
			// snapshot so the frozen viewport matches the pane's final size.
			if a.scroll.IsActive() {
				a.restartScrollLoad()
			}
			return nil
		})
	}()
}

// layoutDialog lays out the active dialog overlay on top of the main screen.
// Dialog views manage their own focus; main views stay visible underneath.
func (a *App) layoutDialog(g *gocui.Gui, maxX, maxY int) error {
	switch a.dialog {
	case DialogCreate:
		a.layoutCreateDialog(g, maxX, maxY)
	case DialogRename:
		a.layoutRenameDialog(g, maxX, maxY)
	case DialogConfirmDelete:
		a.layoutConfirmDeleteDialog(g, maxX, maxY)
	}
	return nil
}

// dialogRect computes a centered rectangle of the given size.
func dialogRect(maxX, maxY, w, h int) Rect {
	if w > maxX-4 {
		w = maxX - 4
	}
	if h > maxY-4 {
		h = maxY - 4
	}
	x0 := (maxX - w) / 2
	y0 := (maxY - h) / 2
	if y0 < 1 {
		y0 = 1
	}
	return Rect{X0: x0, Y0: y0, X1: x0 + w - 1, Y1: y0 + h - 1}
}

// setInputView configures a view for single-line text input.
func setInputView(v *gocui.View) {
	setRoundedFrame(v)
	v.Editable = true
	v.Editor = gocui.DefaultEditor
	v.TextArea.Clear()
	v.RenderTextArea()
}

// setInputContent fills an input view with the given text.
func setInputContent(v *gocui.View, text string) {
	v.TextArea.Clear()
	for _, ch := range text {
		v.TextArea.TypeCharacter(string(ch))
	}
	v.RenderTextArea()
}

// Create dialog: three stacked input fields (name, directory, command) and a
// hint bar. Tab cycles fields; Enter confirms; Esc cancels.
const createDialogWidth = 50

var createFieldNames = [3]string{" Name ", " Directory ", " Command "}

func (a *App) layoutCreateDialog(g *gocui.Gui, maxX, maxY int) {
	// Height: 3 inputs x 2 rows + 2 gaps + 1 hint row + frame slack.
	totalH := 3*3 + 2
	r := dialogRect(maxX, maxY, createDialogWidth, totalH)

	// Initial content of the three fields. The directory field is prefilled
	// with the working directory; the other two start empty. Content is only
	// written when a view is newly created, re-typing it on every layout
	// cycle would move the input cursor to the end of the field.
	initial := [3]string{"", cwdOrEmpty(), ""}
	for i := 0; i < 3; i++ {
		name := createFieldViews[i]
		y0 := r.Y0 + i*3
		v, err := g.SetView(name, r.X0, y0, r.X1, y0+2, 0)
		if err != nil && !isUnknownView(err) {
			return
		}
		v.Title = createFieldNames[i]
		if isUnknownView(err) {
			setInputView(v)
			setInputContent(v, initial[i])
		}
	}

	hintY := r.Y0 + 9
	vh, err := g.SetView("create-hint", r.X0, hintY, r.X1, hintY+2, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	vh.Frame = false
	vh.Clear()
	fmt.Fprint(vh, " "+presentation.StyledKey("Enter", "create")+"  "+
		presentation.StyledKey("Tab", "next")+"  "+
		presentation.StyledKey("Esc", "cancel"))

	if _, err := g.SetCurrentView(createFieldViews[a.createField]); err != nil && !isUnknownView(err) {
		return
	}
	g.Cursor = true
}

func (a *App) closeCreateDialog(g *gocui.Gui) {
	a.dialog = DialogNone
	a.createField = 0
	for _, name := range createFieldViews {
		g.DeleteView(name)
	}
	g.DeleteView("create-hint")
	g.Cursor = false
	_ = a.setDashboardFocus(g)
}

// Rename dialog: single input prefilled with the current session name.
// The prefill happens only on creation so the cursor stays where the user
// places it across layout cycles.
func (a *App) layoutRenameDialog(g *gocui.Gui, maxX, maxY int) {
	r := dialogRect(maxX, maxY, 40, 3)

	v, err := g.SetView("rename-input", r.X0, r.Y0, r.X1, r.Y0+2, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	v.Title = " Rename "
	if isUnknownView(err) {
		setInputView(v)
		setInputContent(v, a.renameTarget)
	}

	if _, err := g.SetCurrentView("rename-input"); err != nil && !isUnknownView(err) {
		return
	}
	g.Cursor = true
}

func (a *App) closeRenameDialog(g *gocui.Gui) {
	a.dialog = DialogNone
	a.renameTarget = ""
	g.DeleteView("rename-input")
	g.Cursor = false
	_ = a.setDashboardFocus(g)
}

// Confirm-delete dialog: asks y/n before killing a session.
func (a *App) layoutConfirmDeleteDialog(g *gocui.Gui, maxX, maxY int) {
	r := dialogRect(maxX, maxY, 40, 3)

	v, err := g.SetView("confirm-delete", r.X0, r.Y0, r.X1, r.Y0+2, 0)
	if err != nil && !isUnknownView(err) {
		return
	}
	setRoundedFrame(v)
	v.Title = " Kill session "
	v.Editable = false
	v.Clear()
	fmt.Fprintf(v, " Kill %q? (y/n) ", a.confirmTarget)

	if _, err := g.SetCurrentView("confirm-delete"); err != nil && !isUnknownView(err) {
		return
	}
	g.Cursor = false
}

func (a *App) closeConfirmDeleteDialog(g *gocui.Gui) {
	a.dialog = DialogNone
	a.confirmTarget = ""
	g.DeleteView("confirm-delete")
	_ = a.setDashboardFocus(g)
}

// createFieldViews are the gocui view names of the create dialog fields,
// ordered to match the createField index.
var createFieldViews = [3]string{"create-name", "create-dir", "create-command"}

// cwdOrEmpty returns the current working directory, or "" if it cannot be
// resolved. Used to prefill the create dialog's directory field.
func cwdOrEmpty() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}
