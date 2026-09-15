package gui

import (
	"context"
	"fmt"

	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/gui/presentation"
	"github.com/avalgott/Lazytmux/internal/session"
)

// renderSessions draws the session list. The selected row is highlighted by
// gocui via SetCursor; an attached session gets a green marker. When the list
// is empty (or tmux is unreachable) only the create hint is shown — the raw
// tmux error is too noisy for the panel and is left out deliberately.
func renderSessions(v *gocui.View, sessions []session.Info, cursor int) {
	if len(sessions) == 0 {
		fmt.Fprintln(v, "")
		fmt.Fprintln(v, "  Press "+presentation.Bold+"n"+presentation.Reset+" to create a session")
		return
	}

	for _, s := range sessions {
		marker := " "
		if s.Attached {
			marker = " " + presentation.FgGreen + presentation.IconAttached + presentation.Reset
		}
		fmt.Fprintf(v, "  %-18s%s\n", s.Name, marker)
	}

	v.SetCursor(0, cursor)
}

// renderPreview draws the live capture of the selected session's active pane.
// Captures run in a goroutine and are cached; the layout cycle triggers a new
// capture when the selection changes or the cache is stale. With no sessions
// the panel stays empty — the create hint lives in the left panel only.
func (a *App) renderPreview(v *gocui.View) {
	sess := a.currentSession()
	if sess == nil {
		v.Title = " Preview "
		return
	}

	v.Title = fmt.Sprintf(" %s ", sess.Name)

	previewW := v.InnerWidth()
	previewH := v.InnerHeight()
	if previewW < 1 {
		previewW = 1
	}
	if previewH < 1 {
		previewH = 1
	}

	// Only fetch if: not busy, AND (cursor changed OR cache is stale).
	// Fullscreen mode uses a tighter threshold so forwarded keystrokes are
	// echoed quickly.
	staleAfter := previewStaleAfter
	if a.fullscreen.IsActive() {
		staleAfter = fullscreenStaleAfter
	}
	a.preview.Lock()
	cache := a.preview.Content()
	cachedCursor := a.preview.Cursor()
	paneCursorX := a.preview.CursorX()
	paneCursorY := a.preview.CursorY()
	needFetch := !a.preview.Busy() && (cachedCursor != a.cursor || a.preview.Stale(staleAfter))
	if needFetch {
		a.preview.SetBusy(true)
	}
	a.preview.Unlock()

	if needFetch {
		name := sess.Name
		cursorSnapshot := a.cursor
		go func() {
			result, err := a.svc.Capture(context.Background(), name, previewW, previewH)
			a.preview.Lock()
			if err == nil {
				a.preview.Update(result.Content, cursorSnapshot, result.CursorX, result.CursorY)
			} else {
				// Failed capture (e.g. session died between refresh cycles) —
				// mark fetched so we don't retry on every render.
				a.preview.MarkFetched(cursorSnapshot)
			}
			a.preview.Unlock()
			a.g.Update(func(*gocui.Gui) error { return nil })
		}()
	}

	if cache != "" && cachedCursor == a.cursor {
		fmt.Fprint(v, cache)
		v.SetCursor(clampInt(paneCursorX, 0, previewW-1), clampInt(paneCursorY, 0, previewH-1))
		return
	}

	// Fallback while loading.
	fmt.Fprintln(v, "")
	fmt.Fprintln(v, "  Loading preview...")
	if sess.Path != "" {
		fmt.Fprintln(v, "")
		fmt.Fprintf(v, "  %s\n", presentation.Dim+sess.Path+presentation.Reset)
	}
}

// renderOptionsBar draws the keybinding hints.
func renderOptionsBar(v *gocui.View) {
	hints := presentation.StyledKey("j/k", "move") + "  " +
		presentation.StyledKey("n", "new") + "  " +
		presentation.StyledKey("d", "delete") + "  " +
		presentation.StyledKey("r", "rename") + "  " +
		presentation.StyledKey("Enter", "open") + "  " +
		presentation.StyledKey("a", "attach") + "  " +
		presentation.StyledKey("q", "quit")
	fmt.Fprintln(v, " "+hints)
}

// renderLogs draws the most recent status/error entries, newest at the
// bottom, trimmed to the panel height.
func renderLogs(v *gocui.View, logs []logEntry) {
	h := v.InnerHeight()
	if h < 1 {
		return
	}
	start := 0
	if len(logs) > h {
		start = len(logs) - h
	}
	for _, e := range logs[start:] {
		prefix := " " + presentation.FgDimGray + e.at.Format("15:04:05") + presentation.Reset + " "
		if e.isErr {
			fmt.Fprintln(v, prefix+presentation.FgRed+e.msg+presentation.Reset)
		} else {
			fmt.Fprintln(v, prefix+e.msg)
		}
	}
}

// clampInt bounds n to [min, max].
func clampInt(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
