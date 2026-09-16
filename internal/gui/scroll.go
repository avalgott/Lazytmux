package gui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/gui/presentation"
)

// ScrollState tracks scrollback browsing in fullscreen mode. Adapted from
// lazyclaude's scroll mode: the live preview is replaced by a window over the
// pane's scrollback history, moved with vim-like keys or the mouse wheel.
//
// Coordinates: the pane's lines are numbered from -history (oldest scrollback
// line) to visibleRows-1 (bottom of the visible screen); capture-pane -S/-E
// use exactly these offsets. The state keeps pos as a 0-based index from the
// oldest line, so capture start = pos - history.
type ScrollState struct {
	active  bool
	history int      // scrollback lines (capture-pane negative side)
	visible int      // visible screen rows of the pane
	pos     int      // first line of the viewport, 0-based from the oldest line
	viewH   int      // viewport height
	lines   []string // last captured viewport content
	width   int      // viewport width (for truncation)
}

// Enter activates scroll mode. total is history + visible rows.
func (ss *ScrollState) Enter(history, visible, viewH, width int) {
	ss.active = true
	ss.history = history
	ss.visible = visible
	ss.viewH = viewH
	ss.width = width
	if viewH < 1 {
		viewH = 1
	}
	// Start at the bottom: the live screen.
	ss.pos = ss.total() - ss.viewH
	if ss.pos < 0 {
		ss.pos = 0
	}
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

// maxPos is the lowest legal pos (bottom viewport).
func (ss *ScrollState) maxPos() int {
	maxPos := ss.total() - ss.viewH
	if maxPos < 0 {
		return 0
	}
	return maxPos
}

// setPos clamps pos and updates the viewport lines from the pane history.
// Returns false when the fetch fails (state left unchanged).
func (ss *ScrollState) setPos(pos int, fetch func(start, end int) ([]string, error)) bool {
	if pos < 0 {
		pos = 0
	}
	if maxPos := ss.maxPos(); pos > maxPos {
		pos = maxPos
	}
	start := pos - ss.history
	end := start + ss.viewH - 1
	lines, err := fetch(start, end)
	if err != nil {
		return false
	}
	ss.pos = pos
	ss.lines = lines
	return true
}

// Move scrolls by delta lines.
func (ss *ScrollState) Move(delta int, fetch func(start, end int) ([]string, error)) {
	ss.setPos(ss.pos+delta, fetch)
}

// Page scrolls by roughly half a viewport.
func (ss *ScrollState) Page(delta int, fetch func(start, end int) ([]string, error)) {
	ss.setPos(ss.pos+delta*(ss.viewH/2), fetch)
}

// Top jumps to the oldest line; Bottom to the live screen.
func (ss *ScrollState) Top(fetch func(start, end int) ([]string, error)) {
	ss.setPos(0, fetch)
}

// Bottom jumps to the live screen.
func (ss *ScrollState) Bottom(fetch func(start, end int) ([]string, error)) {
	ss.setPos(ss.maxPos(), fetch)
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
	// The pane's visible rows: the window was sized to the view +1 row for a
	// status bar; assume the pane is viewH+1 rows, cropped to viewH.
	a.scroll.Enter(history, viewH+1, viewH, width)
	a.scroll.Bottom(a.fetchScrollback)
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// exitScrollMode returns to the live fullscreen view.
func (a *App) exitScrollMode() {
	a.scroll.Exit()
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// fetchScrollback captures one viewport of the pane history and truncates
// lines to the viewport width.
func (a *App) fetchScrollback(start, end int) ([]string, error) {
	target := a.fullscreen.Target()
	preview, err := a.svc.CaptureScrollback(context.Background(), target, start, end)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(preview.Content, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > a.scroll.width {
			lines[i] = ansi.Truncate(line, a.scroll.width, "")
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
	pos := a.scroll.pos + 1
	return " " + presentation.Bold + name + presentation.Reset + "  " +
		presentation.FgDimGray + fmt.Sprintf("scroll %d/%d", pos, a.scroll.total()) + presentation.Reset + "  " +
		presentation.StyledKey("j/k", "move") + "  " +
		presentation.StyledKey("pgup/pgdn", "page") + "  " +
		presentation.StyledKey("g/G", "top/bottom") + "  " +
		presentation.StyledKey("esc", "live")
}
