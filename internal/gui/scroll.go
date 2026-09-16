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

// Top jumps to the oldest line; Bottom to the live view.
func (ss *ScrollState) Top() { ss.offsetFromBottom = ss.maxOffset() }

// Bottom jumps to the live view.
func (ss *ScrollState) Bottom() { ss.offsetFromBottom = 0 }

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
// status bar.
func (ss *ScrollState) position() int {
	return ss.total - ss.viewH - ss.offsetFromBottom + 1
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

	// Load the snapshot in a goroutine: the tmux queries and the capture all
	// run outside the event loop, and only the latest load applies.
	a.scroll.seq++
	seq := a.scroll.seq
	go func() {
		// Derive the capture range from the pane itself: the fullscreen
		// resize runs in another goroutine and may not have completed yet,
		// so assuming viewH visible rows can capture at stale dimensions.
		history, err := a.svc.HistorySize(context.Background(), target)
		if err != nil {
			a.finishScrollLoad(seq, nil, err)
			return
		}
		paneHeight, err := a.svc.PaneHeight(context.Background(), target)
		if err != nil {
			a.finishScrollLoad(seq, nil, err)
			return
		}
		lines, err := fetchScrollbackLines(a.svc, target, width, -history, paneHeight-1)
		if err != nil {
			a.finishScrollLoad(seq, nil, err)
			return
		}
		a.finishScrollLoad(seq, lines, nil)
	}()
}

// finishScrollLoad applies the snapshot load (or its failure) on the event
// loop. A failed load leaves scroll mode instead of stranding the user in a
// perpetual "loading" state, and records the reason in the dashboard log.
func (a *App) finishScrollLoad(seq int64, lines []string, loadErr error) {
	a.g.Update(func(*gocui.Gui) error {
		a.applyScrollLoad(seq, lines, loadErr)
		return nil
	})
}

// applyScrollLoad is the event-loop half of finishScrollLoad, split out so
// tests can drive it directly (headless mode never runs gui.Update).
func (a *App) applyScrollLoad(seq int64, lines []string, loadErr error) {
	if seq != a.scroll.seq {
		return // superseded
	}
	if loadErr != nil {
		if a.scroll.IsActive() {
			a.setError(fmt.Sprintf("scrollback: %v", loadErr))
			a.exitScrollMode()
		}
		return
	}
	if a.scroll.IsActive() {
		a.scroll.lines = lines
		a.scroll.total = len(lines)
		a.scroll.loaded = true
		a.scroll.clampOffset()
	}
}

// exitScrollMode returns to the live fullscreen view.
func (a *App) exitScrollMode() {
	a.scroll.Exit()
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// fetchScrollbackLines captures a range of the pane history and truncates
// lines to the given width. Pure helper: no App state is touched, so it is
// safe to run from a goroutine.
func fetchScrollbackLines(svc session.Provider, target string, width, start, end int) ([]string, error) {
	preview, err := svc.CaptureScrollback(context.Background(), target, start, end)
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
