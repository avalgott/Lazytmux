package gui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/gui/presentation"
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

// Move scrolls by delta lines (positive = towards newer content). A pending
// top request is cancelled: the latest gesture wins.
func (ss *ScrollState) Move(delta int) {
	ss.pendingTop = false
	ss.offsetFromBottom -= delta
	ss.clampOffset()
}

// Page scrolls by roughly half a viewport (at least one line — a one-row
// viewport must still page). A pending top request is cancelled: the latest
// gesture wins.
func (ss *ScrollState) Page(delta int) {
	ss.pendingTop = false
	step := ss.viewH / 2
	if step < 1 {
		step = 1
	}
	ss.offsetFromBottom -= delta * step
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
	// Serialize with wheel forwarding: an in-flight send holds fsMu, and
	// entering scroll mode must wait for it rather than activating after
	// the injection lands.
	a.fsMu.Lock()
	defer a.fsMu.Unlock()
	if !a.fullscreen.IsActive() || a.scroll.IsActive() {
		return
	}
	a.wheelGen.Add(1) // invalidate in-flight wheel forwards
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
func (a *App) restartScrollLoadState(ss *ScrollState, target string, apply func(seq int64, sGen uint64, paneID, fetchRecorded string, lines []string, paneH int, loadErr error)) {
	if target == "" || !ss.IsActive() {
		return
	}
	width := ss.width

	ss.seq++
	seq := ss.seq
	sGen := a.sessionGen.Load()
	go func() {
		lines, paneH, paneID, fetchRecorded, err := a.fetchScrollSnapshot(target, width)
		if err != nil {
			a.finishScrollLoadFor(apply, seq, sGen, paneID, fetchRecorded, nil, 0, err)
			return
		}
		a.finishScrollLoadFor(apply, seq, sGen, paneID, fetchRecorded, lines, paneH, nil)
	}()
}

// finishScrollLoadFor applies a snapshot load (or its failure) on the event
// loop via the given mode-specific applier.
func (a *App) finishScrollLoadFor(apply func(seq int64, sGen uint64, paneID, fetchRecorded string, lines []string, paneH int, loadErr error), seq int64, sGen uint64, paneID, fetchRecorded string, lines []string, paneH int, loadErr error) {
	a.g.Update(func(*gocui.Gui) error {
		apply(seq, sGen, paneID, fetchRecorded, lines, paneH, loadErr)
		return nil
	})
}

// applyScrollLoad is the fullscreen event-loop applier, split out so tests
// can drive it directly (headless mode never runs gui.Update). A failed load
// leaves scroll mode instead of stranding the user in a perpetual "loading"
// state, and records the reason in the dashboard log. A snapshot with no
// history beyond the visible screen (alternate-screen panes like Claude Code
// have no saved history at all) leaves scroll mode too, with the
// no-scrollback hint.
func (a *App) applyScrollLoad(seq int64, sGen uint64, paneID, fetchRecorded string, lines []string, paneH int, loadErr error) {
	// A superseded load must not touch pane metadata at all — the mode was
	// exited and possibly re-entered for another target.
	if a.scroll.seq != seq {
		return
	}
	// A session recreated under the same name while the capture was in
	// flight must not receive the old pane's snapshot. When the rejection
	// hits the latest load, restart it under the current generation — the
	// panel must not stay on "Loading scrollback..." forever. The pane may
	// also have changed between the fetch and this apply: the snapshot
	// belongs to the pane that produced it.
	if a.sessionGen.Load() != sGen {
		if a.scroll.IsActive() && a.scroll.seq == seq {
			a.restartScrollLoad()
		}
		return
	}
	// The pane must be settled BEFORE the snapshot installs: if a newer live
	// capture owns the pane, this snapshot is stale and the load restarts.
	if !a.paneMatches(a.fullscreen.Target(), paneID) {
		if !a.adoptPaneIfStale(a.fullscreen.Target(), fetchRecorded, paneID) {
			if a.scroll.IsActive() && a.scroll.seq == seq {
				a.restartScrollLoad()
			}
			return
		}
	}
	applied, noHistory, err := a.applyScrollLoadState(a.scroll, seq, lines, paneH, loadErr)
	if err != nil {
		a.setError(fmt.Sprintf("scrollback: %v", err))
		a.exitScrollMode()
		return
	}
	if !applied {
		return // superseded or the mode already exited — leave the hint alone
	}
	if noHistory {
		a.fullscreenNoScrollback = true
		a.buffersMu.Lock()
		a.fullscreenNoScrollbackPane = a.paneIDs[a.fullscreen.Target()]
		a.buffersMu.Unlock()
		a.exitScrollMode()
		return
	}
	a.fullscreenNoScrollback = false
}

// applyScrollLoadState applies a snapshot load to the given ScrollState.
// applied reports whether the load landed on the active state (a superseded
// load or one landing after the mode exited changes nothing). noHistory
// reports a snapshot that contains nothing beyond the visible screen (no
// more lines than the pane height): such a snapshot has nothing to browse.
// Returns non-nil when a load failed for an active scroll state (the caller
// exits the mode).
func (a *App) applyScrollLoadState(ss *ScrollState, seq int64, lines []string, paneH int, loadErr error) (applied, noHistory bool, err error) {
	if seq != ss.seq {
		return false, false, nil // superseded
	}
	if loadErr != nil {
		if ss.IsActive() {
			return false, false, loadErr
		}
		return false, false, nil
	}
	if !ss.IsActive() {
		return false, false, nil
	}
	if len(lines) <= paneH {
		return true, true, nil
	}
	ss.lines = lines
	ss.total = len(lines)
	ss.loaded = true
	if ss.pendingTop {
		ss.pendingTop = false
		ss.offsetFromBottom = ss.maxOffset()
	} else {
		ss.clampOffset()
	}
	return true, false, nil
}

// exitScrollMode returns to the live fullscreen view.
func (a *App) exitScrollMode() {
	a.wheelGen.Add(1)     // invalidate in-flight wheel forwards
	a.wheelExitGen.Add(1) // invalidate in-flight wheel fallbacks
	a.scroll.Exit()
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

// fetchScrollSnapshot builds the scrollback snapshot for a target: tmux's
// real pane history when it has any, else the synthetic buffer accumulated
// from observed captures (alternate-screen panes keep no tmux history).
// Lines are truncated to the given width. Safe to run from a goroutine.
func (a *App) fetchScrollSnapshot(target string, width int) ([]string, int, string, string, error) {
	// Sample the binding BEFORE the tmux snapshot: a concurrent rebinding
	// must make this snapshot fail the compare-and-adopt at apply time.
	a.buffersMu.Lock()
	recorded := a.paneIDs[target]
	a.buffersMu.Unlock()
	preview, err := a.svc.CaptureScrollback(context.Background(), target)
	if err != nil {
		return nil, 0, "", recorded, err
	}
	lines := splitScrollback(preview.Content, width)
	if len(lines) > preview.PaneHeight {
		return lines, preview.PaneHeight, preview.PaneID, recorded, nil // real tmux history
	}
	// No tmux history: fall back to the synthetic buffer. bufferLookup does
	// not create — an empty buffer must not exist for every attempted scroll.
	// History is anything beyond the current screen, whose normalized height
	// (blank cursor rows stripped) is smaller than the tmux pane height.
	// The normalized height is what the applier's noHistory check compares
	// against, so it must travel in the pane-height slot. The buffer belongs
	// to a specific pane: if the active pane changed since it was recorded,
	// its history must not be shown under the new pane.
	if b := a.bufferLookup(target); b != nil {
		if recorded == "" || recorded == preview.PaneID {
			if snap, screenH := b.SnapshotWithHeight(); len(snap) > screenH {
				return truncateLines(snap, width), screenH, preview.PaneID, recorded, nil
			}
		}
	}
	return lines, preview.PaneHeight, preview.PaneID, recorded, nil // nothing to browse — hint path
}

// adoptPaneIfStale adopts the pane a scrollback capture observed as the
// session's live pane — but only when the binding is still the one seen at
// fetch time. A concurrent live capture that recorded a NEWER pane in
// between wins: the stale snapshot must not overwrite it or drop its
// buffer. The check and the adoption are one critical section.
func (a *App) adoptPaneIfStale(name, fetchRecorded, paneID string) bool {
	if paneID == "" {
		return true
	}
	a.fsMu.Lock()
	defer a.fsMu.Unlock()
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	if a.paneIDs[name] != fetchRecorded {
		return false // a newer capture rebound the pane — this snapshot is stale
	}
	if fetchRecorded != "" {
		delete(a.buffers, name)
		delete(a.bufferIDs, name)
		delete(a.bufferGens, name)
	}
	a.paneIDs[name] = paneID
	// Reserve a fresh capture sequence: live captures already in flight
	// started before this adoption and must not rebind the session back.
	a.paneSeq[name] = a.captureSeq.Add(1)
	return true
}

// paneMatches reports whether the pane a scrollback capture came from is the
// one the session's buffer belongs to. An unrecorded pane (no live capture
// yet, or an old tmux) cannot conflict and passes.
func (a *App) paneMatches(name, paneID string) bool {
	if paneID == "" {
		return true
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	recorded := a.paneIDs[name]
	return recorded == "" || recorded == paneID
}

// truncateLines clips each line to the given width (same rule as
// splitScrollback, for already-split buffer lines).
func truncateLines(lines []string, width int) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			out[i] = ansi.Truncate(line, width, "")
		} else {
			out[i] = line
		}
	}
	return out
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
	// A recent no-scrollback verdict for this session suppresses the
	// expensive whole-history load while the hint is showing — but only
	// while the buffer is still empty. Live captures keep feeding it, and
	// the program may have started streaming within the hint window.
	a.buffersMu.Lock()
	hintPane := a.paneIDs[sess.Name]
	a.buffersMu.Unlock()
	skip := a.scrollHintIdent != "" && a.scrollHintName == sess.Name &&
		a.scrollHintIdent == sessionIdentity(*sess) && a.scrollHintPane == hintPane &&
		time.Now().Before(a.scrollHintUntil)
	if skip {
		if b := a.bufferLookup(sess.Name); b != nil {
			if snap, h := b.SnapshotWithHeight(); len(snap) > h {
				skip = false // the buffer gained history: browse it
			}
		}
	}
	if skip {
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
	a.previewScrollTargetID = sessionIdentity(*sess)
	a.previewScroll.Enter(viewH, width)
	a.restartPreviewScrollLoad()
}

// exitPreviewScroll returns the preview panel to its live capture.
func (a *App) exitPreviewScroll() {
	a.previewScroll.Exit()
	a.previewScrollTarget = ""
	a.previewScrollTargetID = ""
	a.preview.Invalidate()
	a.g.Update(func(*gocui.Gui) error { return nil })
}

func (a *App) restartPreviewScrollLoad() {
	a.restartScrollLoadState(a.previewScroll, a.previewScrollTarget, a.applyPreviewScrollLoad)
}

// applyPreviewScrollLoad is the dashboard event-loop applier; a failed load
// logs the reason and returns the panel to the live capture.
//
// A load that clamps the offset to the bottom (e.g. a pane smaller than the
// viewport, with a snapshot shorter than it) keeps the mode active — the
// panel shows the frozen snapshot at the bottom until G or a downward
// gesture returns to live. Downward gestures while inactive are already
// filtered before entering, so no expensive load can be started just to
// exit again.
//
// A snapshot with no history beyond the visible screen (alternate-screen
// panes like Claude Code) returns to the live capture with a status note
// instead: there is nothing to browse.
func (a *App) applyPreviewScrollLoad(seq int64, sGen uint64, paneID, fetchRecorded string, lines []string, paneH int, loadErr error) {
	// A superseded load must not touch pane metadata at all — the mode was
	// exited and possibly re-entered for another target.
	if a.previewScroll.seq != seq {
		return
	}
	if a.sessionGen.Load() != sGen {
		// Same dead end as the fullscreen applier: restart the load under
		// the current generation instead of stranding the loading panel.
		if a.previewScroll.IsActive() && a.previewScroll.seq == seq {
			a.restartPreviewScrollLoad()
		}
		return
	}
	// The pane must be settled BEFORE the snapshot installs: if a newer live
	// capture owns the pane, this snapshot is stale and the load restarts.
	if !a.paneMatches(a.previewScrollTarget, paneID) {
		if !a.adoptPaneIfStale(a.previewScrollTarget, fetchRecorded, paneID) {
			if a.previewScroll.IsActive() && a.previewScroll.seq == seq {
				a.restartPreviewScrollLoad()
			}
			return
		}
	}
	applied, noHistory, err := a.applyScrollLoadState(a.previewScroll, seq, lines, paneH, loadErr)
	if err != nil {
		a.setError(fmt.Sprintf("scrollback: %v", err))
		a.exitPreviewScroll()
		return
	}
	if !applied {
		return // superseded or the mode already exited
	}
	if noHistory {
		name := a.previewScrollTarget
		// The pane-flags query can block for the client timeout: run it off
		// the event loop and apply the hint back on it — but only if this
		// load is still the current one (the user may have left browsing or
		// switched sessions while the query was in flight).
		seq := a.previewScroll.seq
		go func() {
			alt, sgr, _, _, qPane, ferr := a.svc.PaneInputFlags(context.Background(), name)
			a.g.Update(func(*gocui.Gui) error {
				a.applyNoHistory(name, seq, alt, sgr, qPane, paneID, ferr != nil)
				return nil
			})
		}()
	}
}

// applyNoHistory applies the no-history verdict for a scroll load — unless
// the load is stale, or the pane switched while the flags query was in
// flight (the verdict belongs to the snapshot's pane, not the replacement).
func (a *App) applyNoHistory(name string, seq int64, alt, sgr bool, qPane, paneID string, flagErr bool) {
	if !a.noHistoryHintCurrent(name, seq) {
		return
	}
	// The pane may have changed since the query returned (or the query may
	// have failed with an empty pane): recheck the recorded binding before
	// the verdict installs, so it never lands against the replacement pane.
	a.buffersMu.Lock()
	recorded := a.paneIDs[name]
	a.buffersMu.Unlock()
	if recorded != "" && paneID != "" && recorded != paneID {
		a.restartPreviewScrollLoad()
		return
	}
	if qPane != "" && paneID != "" && qPane != paneID {
		// The active pane changed while the query ran: the verdict must not
		// suppress the replacement pane's browsing.
		a.restartPreviewScrollLoad()
		return
	}
	a.applyNoHistoryHint(name, alt, sgr, flagErr)
}

// noHistoryHintCurrent reports whether the scroll load that launched the
// hint query is still the live preview scroll.
func (a *App) noHistoryHintCurrent(name string, seq int64) bool {
	return a.previewScrollTarget == name && a.previewScroll.IsActive() && a.previewScroll.seq == seq
}

// applyNoHistoryHint shows the no-scrollback hint for a session whose
// program keeps its own scrollback. The hint is bound to the session ID so
// a same-name recreation cannot inherit a stale verdict, and its wording
// depends on whether scrolling inside is actually possible (alt screen
// with SGR mouse tracking).
func (a *App) applyNoHistoryHint(name string, alt, sgr, flagErr bool) {
	a.scrollHintName = name
	a.scrollHintIdent = ""
	if sess := a.currentSession(); sess != nil && sess.Name == name {
		a.scrollHintIdent = sessionIdentity(*sess)
	}
	a.scrollHintMsg = "No scrollback available."
	if !flagErr && alt && sgr {
		a.scrollHintMsg = scrollHintText
	}
	a.buffersMu.Lock()
	a.scrollHintPane = a.paneIDs[name]
	a.buffersMu.Unlock()
	a.scrollHintUntil = time.Now().Add(scrollHintDuration)
	a.setStatus(a.scrollHintMsg)
	a.exitPreviewScroll()
}

// scrollHintText is the friendly hint shown when a session has no scrollback
// to browse (its program keeps its own). Used in the log and the preview
// title.
const scrollHintText = "No scrollback available. Hit Enter to open the session and scrollback inside of it."

// scrollHintDuration is how long the preview title shows the scroll hint
// after a scroll attempt on a session without scrollback.
const scrollHintDuration = 4 * time.Second

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
	// A downward gesture reaching the live bottom returns to the live
	// capture — loaded or not, so the result is independent of load timing.
	if a.previewScroll.offsetFromBottom == 0 && delta > 0 {
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
