package gui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/gui/presentation"
	"github.com/avalgott/Lazytmux/internal/session"
)

// ScrollState tracks scrollback browsing in fullscreen mode. Adapted from
// lazyclaude's scroll mode: the live preview is replaced by a window over the
// pane's scrollback history, moved with vim-like keys or the mouse wheel.
//
// The whole history (plus the visible screen) is snapshotted once when the
// mode is entered and browsed locally. A frozen snapshot keeps navigation
// stable while the pane keeps producing output — tmux's capture-pane offsets
// are relative to the live screen, which moves, so any live-coordinate
// scheme drifts (and, once the history limit starts evicting lines, cannot
// be corrected). Scrolling is pure in-memory slicing: no tmux calls per
// keystroke, and a slow server cannot freeze the UI.
type ScrollState struct {
	active           bool
	total            int      // snapshot line count (0 while loading)
	viewH            int      // viewport height
	width            int      // viewport width (for truncation)
	offsetFromBottom int      // 0 = live view; grows when scrolling up
	lines            []string // the snapshot
	loaded           bool
	pendingTop       bool  // g pressed while the snapshot was still loading
	seq              int64 // invalidates in-flight snapshot loads
}

// Enter activates scroll mode at the live view. The snapshot loads
// asynchronously via enterScrollMode.
func (ss *ScrollState) Enter(viewH, width int) {
	ss.active = true
	ss.viewH = viewH
	ss.width = width
	if ss.viewH < 1 {
		ss.viewH = 1
	}
	if ss.width < 1 {
		ss.width = 1
	}
	ss.offsetFromBottom = 0
	ss.lines = nil
	ss.total = 0
	ss.loaded = false
	ss.pendingTop = false
}

// Exit deactivates scroll mode and invalidates any in-flight snapshot load.
func (ss *ScrollState) Exit() {
	ss.active = false
	ss.lines = nil
	ss.loaded = false
	ss.total = 0
	ss.seq++
}

// IsActive reports whether scroll mode is on.
func (ss *ScrollState) IsActive() bool {
	return ss.active
}

// maxOffset is the offset of the oldest viewport (top of the snapshot).
func (ss *ScrollState) maxOffset() int {
	m := ss.total - ss.viewH
	if m < 0 {
		return 0
	}
	return m
}

func (ss *ScrollState) clampOffset() {
	if ss.offsetFromBottom < 0 {
		ss.offsetFromBottom = 0
	}
	if !ss.loaded {
		// While the snapshot is loading the real total is unknown; preserve
		// accumulated offsets (the first wheel gesture must still scroll)
		// and let the load callback clamp once the total is known.
		return
	}
	if max := ss.maxOffset(); ss.offsetFromBottom > max {
		ss.offsetFromBottom = max
	}
}

// Move scrolls by delta lines (positive = towards newer content).
func (ss *ScrollState) Move(delta int) {
	ss.offsetFromBottom -= delta
	ss.clampOffset()
}

// Page scrolls by roughly half a viewport.
func (ss *ScrollState) Page(delta int) {
	ss.offsetFromBottom -= delta * (ss.viewH / 2)
	ss.clampOffset()
}

// Top jumps to the oldest line. While the snapshot is still loading the
// real total is unknown, so the request is recorded and honored by the load
// callback.
func (ss *ScrollState) Top() {
	if !ss.loaded {
		ss.pendingTop = true
		ss.offsetFromBottom = 0
		return
	}
	ss.pendingTop = false
	ss.offsetFromBottom = ss.maxOffset()
}

// Bottom jumps to the live view.
func (ss *ScrollState) Bottom() {
	ss.pendingTop = false
	ss.offsetFromBottom = 0
}

// viewport returns the lines currently visible in the snapshot.
func (ss *ScrollState) viewport() []string {
	if !ss.loaded || len(ss.lines) == 0 {
		return nil
	}
	pos := ss.total - ss.viewH - ss.offsetFromBottom
	if pos < 0 {
		pos = 0
	}
	end := pos + ss.viewH
	if end > len(ss.lines) {
		end = len(ss.lines)
	}
	return ss.lines[pos:end]
}

// position is the 1-based line number of the first visible line, for the
// status bar. Clamped to at least 1 for snapshots shorter than the viewport.
func (ss *ScrollState) position() int {
	pos := ss.total - ss.viewH - ss.offsetFromBottom + 1
	if pos < 1 {
		return 1
	}
	return pos
}

// enterScrollMode switches fullscreen into scrollback browsing.
func (a *App) enterScrollMode() {
	if !a.fullscreen.IsActive() || a.scroll.IsActive() {
		return
	}
	target := a.fullscreen.Target()
	if target == "" {
		return
	}
	v, err := a.g.View("main")
	if err != nil {
		return
	}
	viewH := v.InnerHeight()
	width := v.InnerWidth()
	if viewH < 1 {
		viewH = 1
	}
	if width < 1 {
		width = 1
	}
	a.scroll.Enter(viewH, width)

	a.restartScrollLoad()
}

// restartScrollLoad (re)loads the fullscreen scroll snapshot. Called on entry
// and again when a pending pane resize completes, so the frozen viewport
// always matches the pane's final geometry.
func (a *App) restartScrollLoad() {
	a.restartScrollLoadState(a.scroll, a.fullscreen.Target(), a.applyScrollLoad)
}

// restartScrollLoadState (re)loads a scrollback snapshot for a ScrollState
// and target pair in a goroutine; the single atomic tmux capture runs
// outside the event loop, and only the latest load applies. apply is the
// mode-specific event-loop applier.
func (a *App) restartScrollLoadState(ss *ScrollState, target string, apply func(seq int64, lines []string, loadErr error)) {
	if target == "" || !ss.IsActive() {
		return
	}
	width := ss.width

	ss.seq++
	seq := ss.seq
	go func() {
		lines, err := fetchScrollbackLines(a.svc, target, width)
		if err != nil {
			a.finishScrollLoadFor(apply, seq, nil, err)
			return
		}
		a.finishScrollLoadFor(apply, seq, lines, nil)
	}()
}

// finishScrollLoadFor applies a snapshot load (or its failure) on the event
// loop via the given mode-specific applier.
func (a *App) finishScrollLoadFor(apply func(seq int64, lines []string, loadErr error), seq int64, lines []string, loadErr error) {
	a.g.Update(func(*gocui.Gui) error {
		apply(seq, lines, loadErr)
		return nil
	})
}

// applyScrollLoad is the fullscreen event-loop applier, split out so tests
// can drive it directly (headless mode never runs gui.Update). A failed load
// leaves scroll mode instead of stranding the user in a perpetual "loading"
// state, and records the reason in the dashboard log.
func (a *App) applyScrollLoad(seq int64, lines []string, loadErr error) {
	if err := a.applyScrollLoadState(a.scroll, seq, lines, loadErr); err != nil {
		a.setError(fmt.Sprintf("scrollback: %v", err))
		a.exitScrollMode()
	}
}

// applyScrollLoadState applies a snapshot load to the given ScrollState.
// Returns non-nil when a load failed for an active scroll state (the caller
// exits the mode).
func (a *App) applyScrollLoadState(ss *ScrollState, seq int64, lines []string, loadErr error) error {
	if seq != ss.seq {
		return nil // superseded
	}
	if loadErr != nil {
		if ss.IsActive() {
			return loadErr
		}
		return nil
	}
	if ss.IsActive() {
		ss.lines = lines
		ss.total = len(lines)
		ss.loaded = true
		if ss.pendingTop {
			ss.pendingTop = false
			ss.offsetFromBottom = ss.maxOffset()
		} else {
			ss.clampOffset()
		}
	}
	return nil
}

// exitScrollMode returns to the live fullscreen view.
func (a *App) exitScrollMode() {
	a.scroll.Exit()
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// fetchScrollbackLines captures the whole pane history (one atomic tmux
// operation, oldest sentinel to current bottom) and truncates lines to the
// given width. Pure helper: no App state is touched, so it is safe to run
// from a goroutine.
func fetchScrollbackLines(svc session.Provider, target string, width int) ([]string, error) {
	preview, err := svc.CaptureScrollback(context.Background(), target)
	if err != nil {
		return nil, err
	}
	return splitScrollback(preview.Content, width), nil
}

// splitScrollback splits raw capture output into lines and truncates them to
// the given width. capture-pane -p terminates its output with a newline;
// exactly one is stripped so it does not become a phantom final line, while
// preceding newlines that represent blank rows are preserved.
func splitScrollback(content string, width int) []string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	return lines
}

// renderScrollContent draws the scroll viewport into the fullscreen view.
func (a *App) renderScrollContent(v *gocui.View) {
	v.Clear()
	if !a.scroll.loaded {
		fmt.Fprintln(v, " Loading scrollback...")
		return
	}
	lines := a.scroll.viewport()
	if len(lines) > 0 {
		fmt.Fprint(v, strings.Join(lines, "\n"))
	}
}

// scrollStatusText is the fullscreen status bar content in scroll mode.
func (a *App) scrollStatusText() string {
	name := a.fullscreen.Target()
	if !a.scroll.loaded {
		return " " + presentation.Bold + name + presentation.Reset + "  " +
			presentation.Dim + "loading scrollback..." + presentation.Reset + "  " +
			presentation.StyledKey("esc", "live")
	}
	return " " + presentation.Bold + name + presentation.Reset + "  " +
		presentation.FgDimGray + fmt.Sprintf("scroll %d/%d", a.scroll.position(), a.scroll.total) + presentation.Reset + "  " +
		presentation.StyledKey("j/k", "move") + "  " +
		presentation.StyledKey("pgup/pgdn", "page") + "  " +
		presentation.StyledKey("g/G", "top/bottom") + "  " +
		presentation.StyledKey("esc", "live")
}

// enterPreviewScroll activates scrollback browsing on the dashboard: the
// whole pane history of the selected session is snapshotted once and browsed
// in memory (the same frozen-snapshot model as fullscreen scroll mode).
func (a *App) enterPreviewScroll() {
	if a.fullscreen.IsActive() || a.previewScroll.IsActive() || a.dialog != DialogNone {
		return
	}
	sess := a.currentSession()
	if sess == nil {
		return
	}
	v, err := a.g.View("main")
	if err != nil {
		return
	}
	viewH := v.InnerHeight()
	width := v.InnerWidth()
	if viewH < 1 {
		viewH = 1
	}
	if width < 1 {
		width = 1
	}
	a.previewScrollTarget = sess.Name
	a.previewScroll.Enter(viewH, width)
	a.restartPreviewScrollLoad()
}

// exitPreviewScroll returns the preview panel to its live capture.
func (a *App) exitPreviewScroll() {
	a.previewScroll.Exit()
	a.previewScrollTarget = ""
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

func (a *App) restartPreviewScrollLoad() {
	a.restartScrollLoadState(a.previewScroll, a.previewScrollTarget, a.applyPreviewScrollLoad)
}

// applyPreviewScrollLoad is the dashboard event-loop applier; a failed load
// logs the reason and returns the panel to the live capture. A successful
// load that lands at the live bottom (the first gesture was j/PgDn while the
// snapshot was still loading) also returns to the live capture, so the
// outcome does not depend on load timing.
func (a *App) applyPreviewScrollLoad(seq int64, lines []string, loadErr error) {
	if err := a.applyScrollLoadState(a.previewScroll, seq, lines, loadErr); err != nil {
		a.setError(fmt.Sprintf("scrollback: %v", err))
		a.exitPreviewScroll()
		return
	}
	if a.previewScroll.IsActive() && a.previewScroll.loaded && a.previewScroll.offsetFromBottom == 0 {
		a.exitPreviewScroll()
	}
}

// previewScrollMove scrolls the dashboard preview snapshot by delta lines
// (positive = towards newer content). The first gesture enters the mode;
// reaching the live bottom returns to the live capture.
func (a *App) previewScrollMove(delta int) {
	if a.fullscreen.IsActive() || a.dialog != DialogNone {
		return
	}
	// A positive delta at the live bottom has nothing to browse: entering
	// would start a full history load that immediately exits again.
	if !a.previewScroll.IsActive() && delta > 0 {
		return
	}
	if !a.previewScroll.IsActive() {
		a.enterPreviewScroll()
	}
	if !a.previewScroll.IsActive() {
		return // no session, or the main view is missing
	}
	a.previewScroll.Move(delta)
	if a.previewScroll.loaded && a.previewScroll.offsetFromBottom == 0 {
		a.exitPreviewScroll()
		return
	}
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// previewScrollTop jumps to the oldest line (g). A top request made while
// the snapshot loads is honored by the load callback (pendingTop).
func (a *App) previewScrollTop() {
	if a.fullscreen.IsActive() || a.dialog != DialogNone {
		return
	}
	if !a.previewScroll.IsActive() {
		a.enterPreviewScroll()
	}
	if !a.previewScroll.IsActive() {
		return
	}
	a.previewScroll.Top()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// previewScrollBottom returns to the live capture (G).
func (a *App) previewScrollBottom() {
	if a.fullscreen.IsActive() || a.dialog != DialogNone {
		return
	}
	if a.previewScroll.IsActive() {
		a.exitPreviewScroll()
	}
}

// renderPreviewScroll draws the frozen snapshot viewport into the dashboard
// main view; the Title shows the scroll position.
func (a *App) renderPreviewScroll(v *gocui.View) {
	name := a.previewScrollTarget
	if !a.previewScroll.loaded {
		v.Title = fmt.Sprintf(" %s ", name)
		fmt.Fprintln(v, " Loading scrollback...")
		return
	}
	v.Title = fmt.Sprintf(" %s — scroll %d/%d ", name, a.previewScroll.position(), a.previewScroll.total)
	lines := a.previewScroll.viewport()
	if len(lines) > 0 {
		fmt.Fprint(v, strings.Join(lines, "\n"))
	}
}
