// Package gui implements the lazytmux terminal UI: a session list on the
// left, a live pane preview on the right, and a keybinding bar at the bottom.
// The rendering and keybinding idioms are adapted from lazyclaude's gui
// package (same gocui fork), with all Claude-specific panels removed.
package gui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jesseduffield/gocui"

	"github.com/avalgott/Lazytmux/internal/session"
)

// isUnknownView checks for gocui's ErrUnknownView.
// jesseduffield/gocui uses go-errors Wrap, so == and errors.Is don't work.
func isUnknownView(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown view")
}

// DialogKind identifies the active dialog overlay.
type DialogKind int

const (
	DialogNone DialogKind = iota
	DialogCreate
	DialogRename
	DialogConfirmDelete
)

// refreshInterval is how often the session list is re-read from tmux.
const refreshInterval = 300 * time.Millisecond

// previewStaleAfter is how old a captured preview must be before it is
// refreshed (combined with the ticker, previews update roughly twice a second).
const previewStaleAfter = 500 * time.Millisecond

// fullscreenStaleAfter is the staleness threshold in fullscreen mode. The
// tighter threshold keeps the passthrough view responsive to the forwarded
// keystrokes.
const fullscreenStaleAfter = 100 * time.Millisecond

// App is the root TUI application (lazygit Gui equivalent).
type App struct {
	g          *gocui.Gui
	svc        session.Provider
	sessions   []session.Info // cached session list, refreshed periodically
	cursor     int            // selected session index
	preview    *PreviewCache
	fullscreen *FullScreenState
	scroll     *ScrollState
	editor     *inputEditor // fullscreen key-forwarding editor (lazily created)
	// Dashboard preview scrolling: a second ScrollState instance (same frozen
	// snapshot model as fullscreen scroll mode) plus the session it belongs
	// to and the Tab-focus state.
	previewScroll         *ScrollState
	previewScrollTarget   string
	previewScrollTargetID string // tmux ID of the session the snapshot belongs to
	focusMain             bool   // dashboard focus: true = main preview panel, false = sessions
	dialog                DialogKind
	// createField is the active input field of the create dialog
	// (0=name, 1=directory, 2=command).
	createField    int
	renameTarget   string // session being renamed
	confirmTarget  string // session being killed (confirmation pending)
	lastWidth      int
	lastHeight     int
	lastFullscreen bool   // fullscreen state at the previous layout cycle
	lastResizeName string // session whose window was last resized for fullscreen
	// fullscreenNoScrollback marks the fullscreen target's pane as having no
	// tmux scrollback history (alternate-screen programs like Claude Code),
	// so the status bar can say so and the wheel forwards to the pane.
	fullscreenNoScrollback     bool
	fullscreenNoScrollbackPane string        // the pane the verdict belongs to (a pane change hides it)
	fullscreenIdent            string        // identity of the fullscreen target (a recreation must drop scroll mode)
	fullscreenGen              atomic.Uint64 // bumped on enter/exit; async callbacks compare against it
	wheelGen                   atomic.Uint64 // bumped on scroll-mode transitions; stale forwards compare against it
	wheelExitGen               atomic.Uint64 // bumped on scroll-mode exit; stale fallbacks compare against it
	userScrollGen              atomic.Uint64 // bumped when the USER enters scroll mode; queued wheel events compare against it
	modeMu                     sync.Mutex    // guards the cached pane input mode below
	modeTarget                 string
	modeAlt, modeSgr           bool
	modeX, modeY               int
	modePane                   string
	modeAt                     time.Time      // when the cached mode was refreshed
	wheelQueue                 chan wheelTask // ordered queue of wheel events (worker-owned)
	fsMu                       sync.Mutex     // serializes wheel injection with fullscreen transitions
	lastResizeW                int            // and the size it was resized to
	lastResizeH                int
	logs                       []logEntry  // recent status/error messages, shown in the logs panel
	refreshBusy                atomic.Bool // true while a background session refresh is in flight
	attachTarget               string      // session to attach to; set on Enter, main() acts on it
	buffers                    map[string]*LineBuffer
	bufferIDs                  map[string]string // buffer name -> "ID@Created@PID" identity it belongs to
	bufferIdentities           map[string]string // buffer name -> the session identity it was fed under
	paneIDs                    map[string]string // session name -> the active pane the buffer/preview belong to
	paneSeq                    map[string]uint64 // session -> the capture sequence that last recorded its pane
	sessionIdentities          map[string]string // session name -> identity, updated every refresh
	bufferGens                 map[string]uint64 // generation the buffer was last fed under
	buffersMu                  sync.Mutex        // guards the buffers map (LineBuffer locks itself)
	sessionGen                 atomic.Uint64
	captureSeq                 atomic.Uint64 // monotonically increasing capture identity
	lastSessionSig             string        // name=ID signature of the last applied refresh
	quitting                   atomic.Bool   // set when the main loop exits; the wheel worker drops leftovers
	quitCh                     chan struct{} // closed when the main loop exits; unblocks waiting workers

	// scrollHint is the transient preview-title hint shown after a scroll
	// attempt on a session that keeps its own scrollback (alternate-screen
	// programs like Claude Code): "press Enter to open it and scroll inside".
	scrollHintName  string
	scrollHintIdent string // session identity (ID@Created) the hint belongs to
	scrollHintPane  string // the pane the hint belongs to (a pane change invalidates it)
	scrollHintMsg   string // the message shown (depends on whether scrolling inside is possible)
	scrollHintUntil time.Time
}

// logEntry is one line in the logs panel.
type logEntry struct {
	at    time.Time
	msg   string
	isErr bool
}

// maxLogEntries caps the in-memory log.
const maxLogEntries = 100

// NewApp creates an App for the given session provider. Call Run() to start
// the event loop.
func NewApp(svc session.Provider) (*App, error) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode:      gocui.OutputTrue,
		SupportOverlaps: true,
	})
	if err != nil {
		return nil, fmt.Errorf("init gocui: %w", err)
	}
	return newApp(g, svc)
}

// NewAppHeadless creates an App in headless mode for testing.
func NewAppHeadless(svc session.Provider, width, height int) (*App, error) {
	g, err := gocui.NewGui(gocui.NewGuiOpts{
		OutputMode: gocui.OutputTrue,
		Headless:   true,
		Width:      width,
		Height:     height,
	})
	if err != nil {
		return nil, fmt.Errorf("init gocui headless: %w", err)
	}
	return newApp(g, svc)
}

func newApp(g *gocui.Gui, svc session.Provider) (*App, error) {
	app := &App{
		g:                 g,
		svc:               svc,
		preview:           &PreviewCache{},
		fullscreen:        &FullScreenState{},
		scroll:            &ScrollState{},
		previewScroll:     &ScrollState{},
		buffers:           make(map[string]*LineBuffer),
		bufferIDs:         make(map[string]string),
		bufferGens:        make(map[string]uint64),
		bufferIdentities:  make(map[string]string),
		paneIDs:           make(map[string]string),
		paneSeq:           make(map[string]uint64),
		sessionIdentities: make(map[string]string),
		wheelQueue:        make(chan wheelTask, 64),
		quitCh:            make(chan struct{}),
	}

	g.Highlight = true
	g.SelFrameColor = gocui.ColorCyan
	// Mouse reporting enables wheel scrolling in fullscreen scroll mode.
	g.Mouse = true

	g.SetManagerFunc(app.layout)

	// In fullscreen mode, a complete bracketed paste is forwarded to the
	// target pane as a paste (not keystrokes) so multiline text arrives
	// intact. The dashboard ignores pastes.
	g.OnPasteContent = func(text string) error {
		if text == "" {
			return nil
		}
		// Pastes are forwarded in live fullscreen mode only — in scroll mode
		// they would land in the history being browsed, which is confusing.
		if app.fullscreen.IsActive() && !app.scroll.IsActive() {
			target := app.fullscreen.Target()
			if target != "" {
				_ = svc.Paste(context.Background(), target, text)
				app.refreshPreviewSoon()
			}
		}
		return nil
	}

	if err := app.setupKeybindings(); err != nil {
		g.Close()
		return nil, err
	}

	return app, nil
}

// Run starts the main event loop. Blocks until the user quits (q / Ctrl+C)
// or requests an attach (Enter). AttachTarget() reports which session the
// user wants to attach to.
func (a *App) Run() error {
	defer a.g.Close()

	// The wheel worker drains the ordered wheel queue (production only —
	// headless tests drive processWheelTask directly).
	go a.wheelWorker()

	// Refresh loop: re-read the session list and mark the preview stale so
	// the next layout cycle captures fresh pane content. Local tmux calls are
	// fast, but the list fetch runs in a goroutine to keep the event loop
	// responsive even if tmux is slow.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				a.refreshSessionsAsync()
				a.preview.Lock()
				if !a.preview.Busy() {
					a.preview.InvalidateTimestamp()
				}
				a.preview.Unlock()
				a.g.Update(func(*gocui.Gui) error { return nil })
			}
		}
	}()

	err := a.g.MainLoop()
	close(done)
	// Stop the wheel worker: the app is recreated after every attach/detach
	// cycle, and an unclosed queue would leak the worker and its app.
	a.quitting.Store(true)
	close(a.wheelQueue)
	close(a.quitCh)
	if err != nil {
		if strings.Contains(err.Error(), "quit") {
			return nil
		}
		return err
	}
	return nil
}

// AttachTarget returns the session the user chose to attach to, or "" if the
// user quit with q.
func (a *App) AttachTarget() string {
	return a.attachTarget
}

// Gui returns the underlying gocui.Gui (for testing).
func (a *App) Gui() *gocui.Gui {
	return a.g
}

// refreshPaneMode refreshes the cached pane input mode for the current
// fullscreen target, off the event loop. The wheel uses this cache instead
// of a per-event blocking lookup, so forwarded input stays in one ordered
// stream with keyboard input.
func (a *App) refreshPaneMode() {
	if !a.fullscreen.IsActive() {
		return
	}
	target := a.fullscreen.Target()
	if target == "" {
		return
	}
	go func() {
		alt, sgr, cx, cy, pane, err := a.svc.PaneInputFlags(context.Background(), target)
		if err != nil {
			return
		}
		a.modeMu.Lock()
		a.modeTarget, a.modeAlt, a.modeSgr = target, alt, sgr
		a.modeX, a.modeY, a.modePane = cx, cy, pane
		a.modeAt = time.Now()
		a.modeMu.Unlock()
	}()
}

// refreshSessionsAsync fetches the session list in a background goroutine and
// updates the cache via gui.Update. Skipped if a refresh is already in flight.
// The busy flag is atomic: it is read here from the ticker goroutine and
// cleared on the event-loop goroutine inside the Update callback.
//
// The selection follows the session by name across refreshes so the cursor
// does not drift when sessions are created or killed (the list is sorted by
// name, so indexes shift).
func (a *App) refreshSessionsAsync() {
	if !a.refreshBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		sessions, err := a.svc.List(context.Background())
		a.g.Update(func(*gocui.Gui) error {
			a.applySessionRefresh(sessions, err)
			a.refreshBusy.Store(false)
			return nil
		})
	}()
}

// applySessionRefresh installs a freshly fetched session list. Runs on the
// event loop (inside a gui.Update closure). A failed refresh keeps the
// previous list untouched. The selection follows the previously selected
// session by name; fullscreen mode exits automatically when its target
// session disappeared.
func (a *App) applySessionRefresh(sessions []session.Info, err error) {
	if err != nil {
		return
	}
	selected := ""
	if sess := a.currentSession(); sess != nil {
		selected = sess.Name
	}
	a.sessions = sessions

	// Invalidate in-flight captures and drop synthetic scrollback buffers of
	// sessions that no longer exist, under one lock. Captures check their
	// generation under the same lock before feeding (feedBufferIfCurrent),
	// so a refresh can never interleave between the check and the feed —
	// which would recreate a pruned buffer with stale content.
	//
	// Buffers are also bound to the session's stable tmux ID: a session
	// killed and recreated under the same name between refreshes gets a
	// fresh buffer instead of inheriting the old pane's scrollback.
	// Lock order is fsMu -> buffersMu everywhere; the bump is serialized
	// with fsMu so a wheel forward (which holds fsMu across its check and
	// the send) can never race a recreate.
	a.fsMu.Lock()
	a.buffersMu.Lock()
	// Invalidate in-flight captures only when the session identity landscape
	// changed — an ordinary poll must not starve captures that run longer
	// than one refresh interval on a slow tmux server.
	if sig := sessionListSig(sessions); sig != a.lastSessionSig {
		a.lastSessionSig = sig
		a.sessionGen.Add(1)
		// The per-target fullscreen caches are keyed by name: a same-name
		// recreation must not inherit the dead pane's verdicts or skip the
		// replacement window's resize, and an already-loaded scroll mode
		// must not keep showing the old session's frozen snapshot.
		a.lastResizeName = ""
		a.fullscreenNoScrollback = false
		if a.fullscreen.IsActive() {
			for _, s := range a.sessions {
				if s.Name == a.fullscreen.Target() {
					if ident := sessionIdentity(s); a.fullscreenIdent != "" && ident != a.fullscreenIdent {
						a.fullscreenIdent = ident
						a.scroll.Exit()
					}
					break
				}
			}
		}
	}
	for name := range a.buffers {
		found := false
		for _, s := range a.sessions {
			if s.Name == name {
				found = true
				// Buffers are bound to the (ID, Created) identity — tmux
				// recycles IDs after a server restart. A bound buffer
				// belongs to a dead incarnation when its identity changed.
				// An UNBOUND buffer (fed between polls) may only adopt the
				// current identity if it was fed under the current
				// generation — otherwise it holds the previous
				// incarnation's output and is dropped.
				identity := sessionIdentity(s)
				// A bound buffer belongs to a dead incarnation when its
				// identity changed. An UNBOUND buffer is dropped only when
				// THIS session's identity changed since the feed — an
				// unrelated session appearing or vanishing must not destroy
				// the selected session's history.
				if (a.bufferIDs[name] != "" && a.bufferIDs[name] != identity) ||
					(a.bufferIDs[name] == "" && a.bufferIdentities[name] != "" && a.bufferIdentities[name] != identity) {
					delete(a.buffers, name)
					delete(a.bufferIDs, name)
					delete(a.bufferGens, name)
					delete(a.bufferIdentities, name)
					delete(a.paneIDs, name)
					delete(a.paneSeq, name)
				} else {
					a.bufferIDs[name] = identity
				}
				break
			}
		}
		if !found {
			delete(a.buffers, name)
			delete(a.bufferIDs, name)
			delete(a.bufferGens, name)
			delete(a.bufferIdentities, name)
			delete(a.paneIDs, name)
			delete(a.paneSeq, name)
		}
	}
	// Pane metadata prunes independently of buffers: a recreated session
	// (or one whose old captures never created a buffer) must not keep its
	// predecessor's pane ID, or wheel validation would reject the new pane
	// until another live capture happens.
	for name := range a.paneIDs {
		found := false
		for _, s := range a.sessions {
			if s.Name == name {
				found = true
				if a.sessionIdentities[name] != "" && a.sessionIdentities[name] != sessionIdentity(s) {
					delete(a.paneIDs, name)
					delete(a.paneSeq, name)
				}
				break
			}
		}
		if !found {
			delete(a.paneIDs, name)
			delete(a.paneSeq, name)
		}
	}
	// Record every live session's identity, so feeds can bind themselves to
	// it (the capture goroutine cannot read the session list).
	for _, s := range a.sessions {
		a.sessionIdentities[s.Name] = sessionIdentity(s)
	}
	a.buffersMu.Unlock()
	a.fsMu.Unlock()

	// Leave fullscreen automatically when the target session disappeared
	// (e.g. its shell exited, or it was killed elsewhere).
	if a.fullscreen.IsActive() {
		found := false
		for _, s := range a.sessions {
			if s.Name == a.fullscreen.Target() {
				found = true
				break
			}
		}
		if !found {
			a.exitFullScreen()
		}
	}

	if selected != "" {
		for i, s := range a.sessions {
			if s.Name == selected {
				a.cursor = i
				break
			}
		}
	}

	// The preview snapshot is tied to a session; when the selection no longer
	// points at it (killed, renamed, or the list reshuffled), return to the
	// live capture. A name comparison keeps this a no-op during the regular
	// 300ms refresh ticks while browsing.
	if a.previewScroll.IsActive() {
		cur, curID := "", ""
		if sess := a.currentSession(); sess != nil {
			cur = sess.Name
			curID = sessionIdentity(*sess)
		}
		if cur != a.previewScrollTarget || curID != a.previewScrollTargetID {
			a.previewScroll.Exit()
			a.previewScrollTarget = ""
			a.previewScrollTargetID = ""
			a.preview.Invalidate()
		}
	}
}

// enterFullScreen switches the UI to fullscreen passthrough mode for the
// selected session.
func (a *App) enterFullScreen() {
	sess := a.currentSession()
	if sess == nil {
		return
	}
	a.fsMu.Lock()
	defer a.fsMu.Unlock()
	a.scroll.Exit()
	a.previewScroll.Exit()
	a.previewScrollTarget = ""
	a.previewScrollTargetID = ""
	a.fullscreenNoScrollback = false
	a.fullscreenGen.Add(1)
	a.scrollHintName = ""
	a.scrollHintIdent = ""
	a.scrollHintMsg = ""
	a.scrollHintUntil = time.Time{}
	a.preview.Invalidate()
	a.fullscreenIdent = sessionIdentity(*sess)
	a.fullscreen.Enter(sess.Name)
	// Warm the pane-mode cache immediately: the first wheel events must
	// forward without waiting for the next ticker pass.
	a.refreshPaneMode()
}

// exitFullScreen returns to the dashboard layout.
func (a *App) exitFullScreen() {
	a.fsMu.Lock()
	defer a.fsMu.Unlock()
	a.scroll.Exit()
	a.fullscreen.Exit()
	a.fullscreenNoScrollback = false
	a.fullscreenIdent = ""
	a.fullscreenGen.Add(1)
	a.preview.Invalidate()
}

// currentSession returns the session under the cursor, or nil.
func (a *App) currentSession() *session.Info {
	if a.cursor < 0 || a.cursor >= len(a.sessions) {
		return nil
	}
	return &a.sessions[a.cursor]
}

// clampCursor keeps the cursor within the session list.
func (a *App) clampCursor() {
	if len(a.sessions) == 0 {
		a.cursor = 0
		return
	}
	if a.cursor >= len(a.sessions) {
		a.cursor = len(a.sessions) - 1
	}
	if a.cursor < 0 {
		a.cursor = 0
	}
}

// moveCursor moves the selection by delta and marks the preview stale so it
// refreshes for the newly selected session. Moving to a different session
// returns the preview panel to its live capture; a clamped no-op (k at the
// first session, j at the last) does not, since the selection did not change.
func (a *App) moveCursor(delta int) {
	if len(a.sessions) == 0 {
		return
	}
	before := a.cursor
	a.cursor += delta
	a.clampCursor()
	if a.cursor != before && a.previewScroll.IsActive() {
		a.previewScroll.Exit()
		a.previewScrollTarget = ""
		a.previewScrollTargetID = ""
	}
	a.preview.Invalidate()
}

// setStatus appends a success message to the log.
func (a *App) setStatus(msg string) {
	a.appendLog(logEntry{at: time.Now(), msg: msg})
}

// setError appends an error message to the log.
func (a *App) setError(msg string) {
	a.appendLog(logEntry{at: time.Now(), msg: msg, isErr: true})
}

// scrollBufferCap bounds each session's synthetic scrollback buffer (the
// user asked for "a couple of hundred lines"; 400 is comfortably within
// memory limits at pane-line sizes).
const scrollBufferCap = 400

// feedBuffer appends one full-pane capture to the session's synthetic
// scrollback buffer, creating it on first use.
func (a *App) feedBuffer(name, content string) {
	if name == "" || content == "" {
		return
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	a.feedBufferLocked(name, content)
	a.bufferGens[name] = a.sessionGen.Load()
}

// feedBufferIfCurrent feeds the session's synthetic buffer only if the
// session generation still matches. The check and the feed run under the
// same lock that applySessionRefresh holds while bumping the generation and
// pruning buffers, so a refresh cannot interleave between them (a stale
// completion would otherwise recreate a pruned buffer).
func (a *App) feedBufferIfCurrent(name string, gen uint64, paneID, content string) {
	if name == "" || content == "" {
		return
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	if a.sessionGen.Load() != gen {
		return
	}
	// The capture belongs to a specific pane: if the binding moved on while
	// it was in flight, its content must not enter the new pane's history.
	if paneID != "" && a.paneIDs[name] != "" && a.paneIDs[name] != paneID {
		return
	}
	a.feedBufferLocked(name, content)
	a.bufferGens[name] = gen
}

// feedBufferLocked feeds the buffer without taking the map lock — the caller
// holds it.
func (a *App) feedBufferLocked(name, content string) {
	if a.buffers == nil {
		a.buffers = make(map[string]*LineBuffer)
	}
	b := a.buffers[name]
	if b == nil {
		// A fresh buffer starts unbound, bound to the session identity the
		// last refresh observed — any lingering binding from a previous
		// incarnation would otherwise get the buffer deleted on the next
		// refresh.
		delete(a.bufferIDs, name)
		a.bufferIdentities[name] = a.sessionIdentities[name]
		b = NewLineBuffer(scrollBufferCap)
		a.buffers[name] = b
	}
	b.Feed(content)
}

// bufferFor returns the session's synthetic scrollback buffer, creating an
// empty one on first use. The map is guarded; the LineBuffer locks itself.
func (a *App) bufferFor(name string) *LineBuffer {
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	if a.buffers == nil {
		a.buffers = make(map[string]*LineBuffer)
	}
	b := a.buffers[name]
	if b == nil {
		b = NewLineBuffer(scrollBufferCap)
		a.buffers[name] = b
	}
	return b
}

// bufferLookup returns the session's synthetic scrollback buffer without
// creating one, or nil.
func (a *App) bufferLookup(name string) *LineBuffer {
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	return a.buffers[name]
}

// sessionIdentity is the stable identity of a session incarnation: tmux
// recycles IDs after a server restart, and Created has second granularity,
// so the server PID — which changes on every restart — is part of it.
func sessionIdentity(s session.Info) string {
	return s.ID + "@" + strconv.FormatInt(s.Created, 10) + "@" + strconv.FormatInt(s.ServerPID, 10)
}

// sessionListSig is a cheap identity signature of the session list: the
// name=identity pairs joined in list order (the service sorts by name, so
// the order is stable). It changes exactly when a session appears,
// disappears, is renamed, or is recreated — the events that invalidate
// in-flight captures.
func sessionListSig(sessions []session.Info) string {
	var sb strings.Builder
	for _, s := range sessions {
		sb.WriteString(s.Name)
		sb.WriteByte('=')
		sb.WriteString(sessionIdentity(s))
		sb.WriteByte(';')
	}
	return sb.String()
}

// appendLog adds an entry to the log, trimming the oldest entries when the
// log grows past maxLogEntries. A message identical to the previous one
// (e.g. the no-scrollback note on every wheel gesture) refreshes the
// timestamp instead of stacking duplicates.
func (a *App) appendLog(e logEntry) {
	if n := len(a.logs); n > 0 && a.logs[n-1].msg == e.msg && a.logs[n-1].isErr == e.isErr {
		a.logs[n-1].at = e.at
		return
	}
	a.logs = append(a.logs, e)
	if len(a.logs) > maxLogEntries {
		a.logs = a.logs[len(a.logs)-maxLogEntries:]
	}
}

// quit exits the main loop.
func (a *App) quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrQuit
}

// attach sets the attach target and exits the main loop; main() then attaches
// and returns to the dashboard after detach.
func (a *App) attach(g *gocui.Gui, v *gocui.View) error {
	if a.dialog != DialogNone {
		return nil
	}
	sess := a.currentSession()
	if sess == nil {
		return nil
	}
	a.attachTarget = sess.Name
	return gocui.ErrQuit
}
