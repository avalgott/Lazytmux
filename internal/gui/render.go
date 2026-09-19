package gui

import (
	"context"
	"fmt"
	"time"

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
	// A recent scroll attempt on a session without scrollback explains
	// itself in the title for a few seconds (the log keeps a record too).
	// Dashboard only — the fullscreen frame title carries the session name.
	// The verdict belongs to a specific pane: a replacement pane must not
	// inherit the old pane's title.
	a.buffersMu.Lock()
	hintPane := a.paneIDs[sess.Name]
	a.buffersMu.Unlock()
	if !a.fullscreen.IsActive() && a.scrollHintName == sess.Name && a.scrollHintIdent == sessionIdentity(*sess) &&
		a.scrollHintPane == hintPane && time.Now().Before(a.scrollHintUntil) &&
		!a.bufferHasHistory(sess.Name) {
		v.Title = " " + a.scrollHintMsg + " "
	}

	a.previewCaptureTick(v, sess)

	previewW := v.InnerWidth()
	previewH := v.InnerHeight()
	if previewW < 1 {
		previewW = 1
	}
	if previewH < 1 {
		previewH = 1
	}
	a.preview.Lock()
	cache := a.preview.Content()
	cacheName := a.preview.Name()
	cacheGen := a.preview.Gen()
	cachePane := a.preview.PaneID()
	cachedCursor := a.preview.Cursor()
	paneCursorX := a.preview.CursorX()
	paneCursorY := a.preview.CursorY()
	a.preview.Unlock()
	a.buffersMu.Lock()
	recordedPane := a.paneIDs[sess.Name]
	a.buffersMu.Unlock()

	if cache != "" && cacheName == sess.Name && cachedCursor == a.cursor && cacheGen == a.sessionGen.Load() &&
		(cachePane == "" || recordedPane == "" || cachePane == recordedPane) {
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

// previewCaptureTick starts a fresh live capture when the cache needs one.
// Split out of renderPreview so the frozen-snapshot renders can keep the
// capture pipeline (and the synthetic-buffer feeds it drives) running while
// the panel shows scrollback instead of the live screen — otherwise output
// arriving during a long browse falls into the gap between captures and
// never reaches the synthetic scrollback.
func (a *App) previewCaptureTick(v *gocui.View, sess *session.Info) {
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
	a.buffersMu.Lock()
	recordedPane := a.paneIDs[sess.Name]
	a.buffersMu.Unlock()
	a.preview.Lock()
	cacheName := a.preview.Name()
	cacheGen := a.preview.Gen()
	cachePane := a.preview.PaneID()
	cachedCursor := a.preview.Cursor()
	needFetch := !a.preview.Busy() && (cacheName != sess.Name || cachedCursor != a.cursor || a.preview.Stale(staleAfter) || cacheGen != a.sessionGen.Load() || (cachePane != "" && recordedPane != "" && cachePane != recordedPane))
	if needFetch {
		a.preview.SetBusy(true)
	}
	a.preview.Unlock()
	if needFetch {
		name := sess.Name
		cursorSnapshot := a.cursor
		gen := a.sessionGen.Load()
		cs := a.captureSeq.Add(1)
		go func() {
			result, err := a.svc.Capture(context.Background(), name, previewW, previewH)
			a.renderPreviewCapture(name, cursorSnapshot, gen, cs, result, err)
		}()
	}
}

// renderPreviewCapture installs a completed live capture into the preview
// cache and feeds the session's synthetic scrollback buffer. Split out from
// the fetch goroutine so tests can drive it directly (headless mode never
// runs gui.Update).
//
// The feed is gated on the session generation recorded when the capture
// started: a refresh in between means the session may have vanished (or its
// name been reused), so the stale completion must not feed history.
func (a *App) renderPreviewCapture(name string, cursorSnapshot int, gen uint64, cs uint64, result session.Preview, err error) {
	// The generation check and the pane recording are atomic under fsMu —
	// the refresh (which prunes pane metadata) holds the same lock, so a
	// stale capture cannot repopulate paneIDs for a removed session. Failed
	// captures record nothing: their pane identity is meaningless.
	a.fsMu.Lock()
	genOK := a.sessionGen.Load() == gen
	var paneChanged bool
	if genOK && err == nil {
		paneChanged = a.recordPaneLocked(name, result.PaneID, cs)
	}
	a.fsMu.Unlock()
	a.preview.Lock()
	if !genOK {
		// A refresh changed the session landscape while this capture was in
		// flight: discard the result and the old cache alike, and let the
		// render loop start a fresh capture (marking fetched throttles the
		// retry). Without the clear, a session recreated under the same name
		// and cursor index would inherit the old pane's screen.
		a.preview.ClearContent()
		a.preview.MarkFetched(name, gen, "", cursorSnapshot)
		a.preview.Unlock()
		a.g.Update(func(*gocui.Gui) error { return nil })
		return
	}
	if paneChanged {
		a.preview.ClearContent()
	}
	if err == nil {
		a.preview.Update(name, result.Content, gen, result.PaneID, cursorSnapshot, result.CursorX, result.CursorY)
	} else {
		// Failed capture (e.g. session died between refresh cycles) —
		// mark fetched so we don't retry on every render. The pane is
		// retagged to the CURRENT recorded binding: after a pane rebind,
		// keeping the old pane ID would trip the render-time mismatch on
		// every failure and spin captures on a persistent tmux error.
		a.buffersMu.Lock()
		curPane := a.paneIDs[name]
		a.buffersMu.Unlock()
		a.preview.MarkFetched(name, gen, curPane, cursorSnapshot)
	}
	a.preview.Unlock()
	if err == nil {
		a.feedBufferIfCurrent(name, gen, result.PaneID, result.Full)
	}
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// recordPaneLocked binds a session to the pane its capture came from.
// Returns true when the active pane changed (the previous pane's buffer is
// dropped). The caller holds fsMu; buffersMu is taken inside — the same
// order the refresh uses.
func (a *App) recordPaneLocked(name, paneID string, cs uint64) (changed bool) {
	if paneID == "" {
		return false
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	if a.paneIDs[name] != "" && a.paneIDs[name] != paneID {
		// A capture that started BEFORE the one that recorded the current
		// binding is stale: its pane must not overwrite the newer binding.
		if cs < a.paneSeq[name] {
			return false
		}
		changed = true
		a.dropBufferLocked(name)
	}
	a.paneIDs[name] = paneID
	a.paneSeq[name] = cs
	return changed
}

// renderOptionsBar draws the keybinding hints. The bar follows panel focus,
// not scroll state: j/k scroll whenever the preview has focus (the first
// press enters scroll mode), so advertising anything else would lie in the
// transition states.
func (a *App) renderOptionsBar(v *gocui.View) {
	if a.focusMain {
		fmt.Fprintln(v, " "+presentation.StyledKey("j/k", "scroll")+"  "+
			presentation.StyledKey("PgUp/PgDn", "page")+"  "+
			presentation.StyledKey("g/G", "top/live")+"  "+
			presentation.StyledKey("Tab", "panels"))
		return
	}
	hints := presentation.StyledKey("j/k", "move") + "  " +
		presentation.StyledKey("n", "new") + "  " +
		presentation.StyledKey("d", "delete") + "  " +
		presentation.StyledKey("r", "rename") + "  " +
		presentation.StyledKey("Enter", "open") + "  " +
		presentation.StyledKey("a", "attach") + "  " +
		presentation.StyledKey("Tab", "panels") + "  " +
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
