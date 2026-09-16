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
// The viewport position is stored as an offset from the live view (0 = the
// current screen), so output produced while browsing — which appends to, and
// eventually evicts from, the history — does not move the visible lines. The
// history size is re-queried before every fetch for the same reason; the
// coordinates are then translated into capture-pane -S/-E offsets (0 is the
// top of the visible screen, negative values count back into the history).
type ScrollState struct {
	active           bool
	history          int      // scrollback lines (refreshed before each fetch)
	visible          int      // pane rows the viewport covers
	viewH            int      // viewport height
	width            int      // viewport width (for truncation)
	offsetFromBottom int      // 0 = live view; grows when scrolling up
	lines            []string // last applied viewport content
	seq              int64
}

// Enter activates scroll mode at the live view.
func (ss *ScrollState) Enter(history, visible, viewH, width int) {
	ss.active = true
	ss.history = history
	ss.visible = visible
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
}

// Exit deactivates scroll mode.
func (ss *ScrollState) Exit() {
	ss.active = false
	ss.lines = nil
}

// IsActive reports whether scroll mode is on.
func (ss *ScrollState) IsActive() bool {
	return ss.active
}

// total returns the number of browsable lines.
func (ss *ScrollState) total() int {
	return ss.history + ss.visible
}

// maxOffset is the offset of the oldest viewport (top of the history).
func (ss *ScrollState) maxOffset() int {
	m := ss.total() - ss.viewH
	if m < 0 {
		return 0
	}
	return m
}

func (ss *ScrollState) clampOffset() {
	if ss.offsetFromBottom < 0 {
		ss.offsetFromBottom = 0
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

// rangeFor translates the current position into capture-pane -S/-E offsets.
func (ss *ScrollState) rangeFor() (start, end int) {
	pos := ss.total() - ss.viewH - ss.offsetFromBottom
	start = pos - ss.history
	end = start + ss.viewH - 1
	return start, end
}

// position is the 1-based line number of the first visible line, for the
// status bar.
func (ss *ScrollState) position() int {
	return ss.total() - ss.viewH - ss.offsetFromBottom + 1
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
	history, err := a.svc.HistorySize(context.Background(), target)
	if err != nil {
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
	// The target window is resized to exactly the view size on fullscreen
	// entry, so the pane has (at most) viewH visible rows; capture-pane
	// clamps requests past the pane's last row.
	a.scroll.Enter(history, viewH, viewH, width)
	a.updateScrollViewport()
}

// exitScrollMode returns to the live fullscreen view.
func (a *App) exitScrollMode() {
	a.scroll.Exit()
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// updateScrollViewport refreshes the history size and asynchronously fetches
// the viewport for the current position. The fetch goroutine reads no shared
// state — every input is captured here on the event loop — and the result is
// applied inside a gui.Update closure (also on the event loop), so a slow
// tmux server cannot freeze the UI and only the latest fetch wins.
func (a *App) updateScrollViewport() {
	target := a.fullscreen.Target()
	history, err := a.svc.HistorySize(context.Background(), target)
	if err != nil {
		return
	}
	a.scroll.history = history
	start, end := a.scroll.rangeFor()
	width := a.scroll.width

	a.scroll.seq++
	seq := a.scroll.seq
	go func() {
		lines, err := fetchScrollbackLines(a.svc, target, width, start, end)
		if err != nil {
			return
		}
		a.g.Update(func(*gocui.Gui) error {
			if seq == a.scroll.seq && a.scroll.IsActive() {
				a.scroll.lines = lines
			}
			return nil
		})
	}()
}

// fetchScrollbackLines captures one viewport of the pane history and
// truncates lines to the given width. Pure helper: no App state is touched,
// so it is safe to run from a goroutine.
func fetchScrollbackLines(svc session.Provider, target string, width, start, end int) ([]string, error) {
	preview, err := svc.CaptureScrollback(context.Background(), target, start, end)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(preview.Content, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	return lines, nil
}

// renderScrollContent draws the scroll viewport into the fullscreen view.
func (a *App) renderScrollContent(v *gocui.View) {
	v.Clear()
	if len(a.scroll.lines) == 0 {
		return
	}
	fmt.Fprint(v, strings.Join(a.scroll.lines, "\n"))
}

// scrollStatusText is the fullscreen status bar content in scroll mode.
func (a *App) scrollStatusText() string {
	name := a.fullscreen.Target()
	return " " + presentation.Bold + name + presentation.Reset + "  " +
		presentation.FgDimGray + fmt.Sprintf("scroll %d/%d", a.scroll.position(), a.scroll.total()) + presentation.Reset + "  " +
		presentation.StyledKey("j/k", "move") + "  " +
		presentation.StyledKey("pgup/pgdn", "page") + "  " +
		presentation.StyledKey("g/G", "top/bottom") + "  " +
		presentation.StyledKey("esc", "live")
}
