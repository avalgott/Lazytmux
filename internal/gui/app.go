// Package gui implements the lazytmux terminal UI: a session list on the
// left, a live pane preview on the right, and a keybinding bar at the bottom.
// The rendering and keybinding idioms are adapted from lazyclaude's gui
// package (same gocui fork), with all Claude-specific panels removed.
package gui

import (
	"context"
	"fmt"
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
	previewScroll       *ScrollState
	previewScrollTarget string
	focusMain           bool // dashboard focus: true = main preview panel, false = sessions
	dialog              DialogKind
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
	fullscreenNoScrollback bool
	lastResizeW            int // and the size it was resized to
	lastResizeH            int
	logs                   []logEntry  // recent status/error messages, shown in the logs panel
	refreshBusy            atomic.Bool // true while a background session refresh is in flight
	attachTarget           string      // session to attach to; set on Enter, main() acts on it
	buffers                map[string]*LineBuffer
	buffersMu              sync.Mutex // guards the buffers map (LineBuffer locks itself)
	sessionGen             atomic.Uint64

	// scrollHint is the transient preview-title hint shown after a scroll
	// attempt on a session that keeps its own scrollback (alternate-screen
	// programs like Claude Code): "press Enter to open it and scroll inside".
	scrollHintName  string
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
		g:             g,
		svc:           svc,
		preview:       &PreviewCache{},
		fullscreen:    &FullScreenState{},
		scroll:        &ScrollState{},
		previewScroll: &ScrollState{},
		buffers:       make(map[string]*LineBuffer),
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
	a.buffersMu.Lock()
	a.sessionGen.Add(1)
	for name := range a.buffers {
		found := false
		for _, s := range a.sessions {
			if s.Name == name {
				found = true
				break
			}
		}
		if !found {
			delete(a.buffers, name)
		}
	}
	a.buffersMu.Unlock()

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
		cur := ""
		if sess := a.currentSession(); sess != nil {
			cur = sess.Name
		}
		if cur != a.previewScrollTarget {
			a.previewScroll.Exit()
			a.previewScrollTarget = ""
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
	a.scroll.Exit()
	a.previewScroll.Exit()
	a.previewScrollTarget = ""
	a.fullscreenNoScrollback = false
	a.scrollHintName = ""
	a.scrollHintUntil = time.Time{}
	a.preview.Invalidate()
	a.fullscreen.Enter(sess.Name)
}

// exitFullScreen returns to the dashboard layout.
func (a *App) exitFullScreen() {
	a.scroll.Exit()
	a.fullscreen.Exit()
	a.fullscreenNoScrollback = false
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
}

// feedBufferIfCurrent feeds the session's synthetic buffer only if the
// session generation still matches. The check and the feed run under the
// same lock that applySessionRefresh holds while bumping the generation and
// pruning buffers, so a refresh cannot interleave between them (a stale
// completion would otherwise recreate a pruned buffer).
func (a *App) feedBufferIfCurrent(name string, gen uint64, content string) {
	if name == "" || content == "" {
		return
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	if a.sessionGen.Load() != gen {
		return
	}
	a.feedBufferLocked(name, content)
}

// feedBufferLocked feeds the buffer without taking the map lock — the caller
// holds it.
func (a *App) feedBufferLocked(name, content string) {
	if a.buffers == nil {
		a.buffers = make(map[string]*LineBuffer)
	}
	b := a.buffers[name]
	if b == nil {
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
