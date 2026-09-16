package gui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jesseduffield/gocui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/avalgott/Lazytmux/internal/session"
)

// fakeProvider implements session.Provider for tests. Mutations are recorded
// by background goroutines (the dialog handlers), so access is synchronized.
type fakeProvider struct {
	mu           sync.Mutex
	infos        []session.Info
	creates      []session.CreateOpts
	killed       []string
	renames      []renameCall
	resizes      []resizeCall
	captured     session.Preview
	keys         map[string][]string // session -> forwarded tmux key names
	literals     map[string]string   // session -> forwarded literal text
	pastes       map[string]string   // session -> pasted text
	scrollRanges []scrollRange       // (start,end) pairs passed to CaptureScrollback
	history      int                 // value returned by HistorySize
	paneHeight   int                 // value returned by PaneHeight
	err          error
}

type renameCall struct{ from, to string }

type resizeCall struct {
	name   string
	width  int
	height int
}

type scrollRange struct{ start, end int }

func (f *fakeProvider) List(context.Context) ([]session.Info, error) {
	return f.infos, f.err
}

func (f *fakeProvider) Create(_ context.Context, opts session.CreateOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, opts)
	return f.err
}

func (f *fakeProvider) Kill(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, name)
	return f.err
}

func (f *fakeProvider) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, renameCall{from, to})
	return f.err
}

func (f *fakeProvider) Capture(_ context.Context, _ string, _, _ int) (session.Preview, error) {
	return f.captured, f.err
}

func (f *fakeProvider) CaptureScrollback(_ context.Context, _ string, start, end int) (session.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scrollRanges = append(f.scrollRanges, scrollRange{start, end})
	var sb strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return session.Preview{Content: sb.String()}, f.err
}

func (f *fakeProvider) HistorySize(_ context.Context, _ string) (int, error) {
	return f.history, f.err
}

func (f *fakeProvider) PaneHeight(_ context.Context, _ string) (int, error) {
	return f.paneHeight, f.err
}

func (f *fakeProvider) SendKeys(_ context.Context, name string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = make(map[string][]string)
	}
	f.keys[name] = append(f.keys[name], keys...)
	return f.err
}

func (f *fakeProvider) SendLiteral(_ context.Context, name, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.literals == nil {
		f.literals = make(map[string]string)
	}
	f.literals[name] += text
	return f.err
}

func (f *fakeProvider) Paste(_ context.Context, name, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pastes == nil {
		f.pastes = make(map[string]string)
	}
	f.pastes[name] += text
	return f.err
}

func (f *fakeProvider) ResizeWindow(_ context.Context, name string, w, h int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, resizeCall{name, w, h})
	return f.err
}

func (f *fakeProvider) Attach(name string) error {
	return nil
}

// snapshot returns copies of the recorded calls for race-free assertions.
func (f *fakeProvider) snapshot() (creates []session.CreateOpts, killed []string, renames []renameCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]session.CreateOpts(nil), f.creates...),
		append([]string(nil), f.killed...),
		append([]renameCall(nil), f.renames...)
}

func newTestApp(t *testing.T, p session.Provider) *App {
	t.Helper()
	app, err := NewAppHeadless(p, 120, 40)
	require.NoError(t, err)
	t.Cleanup(func() { app.g.Close() })
	return app
}

func TestLayoutRendersSessions(t *testing.T) {
	p := &fakeProvider{infos: []session.Info{
		{Name: "devbox", Attached: true},
		{Name: "logs"},
		{Name: "local"},
	}}
	app := newTestApp(t, p)
	app.sessions = p.infos

	require.NoError(t, app.layout(app.g))

	v, err := app.g.View("sessions")
	require.NoError(t, err)
	buf := v.Buffer()
	assert.Contains(t, buf, "devbox")
	assert.Contains(t, buf, "logs")
	assert.Contains(t, buf, "local")
	assert.Contains(t, buf, "●", "attached session should be marked")

	for _, name := range []string{"logs", "main", "options"} {
		_, err := app.g.View(name)
		assert.NoError(t, err, "view %q should exist", name)
	}
}

func TestLayoutEmptyState(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = nil

	require.NoError(t, app.layout(app.g))

	v, err := app.g.View("sessions")
	require.NoError(t, err)
	assert.Contains(t, v.Buffer(), "Press n to create a session")
	assert.NotContains(t, v.Buffer(), "tmux:", "the raw tmux error must not be shown")

	// The preview panel stays empty.
	m, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, m.Buffer(), "Press n")
}

func TestLogsPanelRendersMessages(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.setStatus(`Created "devbox"`)
	app.setStatus(`Killed "logs"`)
	app.setError("tmux: session not found")

	require.NoError(t, app.layout(app.g))

	v, err := app.g.View("logs")
	require.NoError(t, err)
	buf := v.Buffer()
	assert.Contains(t, buf, `Created "devbox"`)
	assert.Contains(t, buf, `Killed "logs"`)
	assert.Contains(t, buf, "session not found")
	// gocui parses ANSI colors into cell attributes, so the buffer holds the
	// plain text; the error styling is asserted via the isErr flag elsewhere.
	assert.Contains(t, buf, ":", "entries should have timestamps")
}

func TestLogsCap(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	for i := 0; i < maxLogEntries+20; i++ {
		app.setStatus(fmt.Sprintf("entry %d", i))
	}
	assert.Len(t, app.logs, maxLogEntries)
	assert.Equal(t, "entry 20", app.logs[0].msg, "oldest entries should be trimmed")
}

func TestLayoutListErrorKeepsPreviousList(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0

	// A failed refresh must not blank the list.
	app.applySessionRefresh(nil, assert.AnError)
	assert.Equal(t, []session.Info{{Name: "devbox"}}, app.sessions)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("sessions")
	require.NoError(t, err)
	assert.Contains(t, v.Buffer(), "devbox")
	assert.NotContains(t, v.Buffer(), "tmux:")
}

func TestCursorNavigationClamps(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	app.cursor = 2

	app.moveCursor(1)
	assert.Equal(t, 2, app.cursor, "down past the last session should clamp")

	app.moveCursor(-10)
	assert.Equal(t, 0, app.cursor, "up past the first session should clamp")

	app.moveCursor(1)
	assert.Equal(t, 1, app.cursor)
}

func TestAttachSetsTarget(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}, {Name: "logs"}}
	app.cursor = 1

	err := app.attach(app.g, nil)
	assert.Equal(t, gocui.ErrQuit, err)
	assert.Equal(t, "logs", app.AttachTarget())
}

func TestQuit(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	err := app.quit(app.g, nil)
	assert.Equal(t, gocui.ErrQuit, err)
	assert.Equal(t, "", app.AttachTarget())
}

func TestGlobalActionsIgnoredWhileDialogOpen(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "a"}, {Name: "b"}}
	app.cursor = 0
	app.dialog = DialogCreate

	require.NoError(t, app.cursorMoveHandler(1)(app.g, nil))
	assert.Equal(t, 0, app.cursor, "j should be ignored while a dialog is open")

	err := app.openCreateHandler(app.g, nil)
	assert.NoError(t, err)
	assert.Equal(t, DialogCreate, app.dialog)

	err = app.openRenameHandler(app.g, nil)
	assert.NoError(t, err)
	assert.Equal(t, "", app.renameTarget)
}

func TestCreateDialogFlow(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	require.NoError(t, app.layout(app.g))

	// n opens the dialog
	require.NoError(t, app.openCreateHandler(app.g, nil))
	assert.Equal(t, DialogCreate, app.dialog)

	// Layout creates the three input views.
	require.NoError(t, app.layout(app.g))
	for _, name := range createFieldViews {
		_, err := app.g.View(name)
		assert.NoError(t, err, "view %q should exist", name)
	}

	// Fill the fields: name + command; clear the prefilled directory.
	require.NoError(t, setInputContentView(app.g, "create-name", "devbox"))
	require.NoError(t, setInputContentView(app.g, "create-command", "ssh devbox"))
	dirView, err := app.g.View("create-dir")
	require.NoError(t, err)
	dirView.TextArea.Clear()
	dirView.RenderTextArea()

	require.NoError(t, app.confirmCreate(app.g, nil))
	assert.Equal(t, DialogNone, app.dialog, "dialog should close on confirm")

	require.Eventually(t, func() bool {
		creates, _, _ := p.snapshot()
		return len(creates) == 1
	}, time.Second, 5*time.Millisecond)
	creates, _, _ := p.snapshot()
	assert.Equal(t, session.CreateOpts{Name: "devbox", Command: "ssh devbox"}, creates[0])
}

func TestCreateDialogRejectsEmptyName(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.openCreateHandler(app.g, nil))
	require.NoError(t, app.layout(app.g))

	nameView, err := app.g.View("create-name")
	require.NoError(t, err)
	nameView.TextArea.Clear()
	nameView.RenderTextArea()

	require.NoError(t, app.confirmCreate(app.g, nil))
	assert.Equal(t, DialogCreate, app.dialog, "dialog should stay open on invalid name")
	require.NotEmpty(t, app.logs, "the validation error should be logged")
	assert.True(t, app.logs[len(app.logs)-1].isErr)
	creates, _, _ := p.snapshot()
	assert.Empty(t, creates)
}

func TestCreateDialogCancel(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.openCreateHandler(app.g, nil))
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.cancelDialog(app.g, nil))
	assert.Equal(t, DialogNone, app.dialog)
	_, err := app.g.View("create-name")
	assert.Error(t, err, "dialog views should be deleted")
}

func TestRenameDialogFlow(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.openRenameHandler(app.g, nil))
	assert.Equal(t, DialogRename, app.dialog)
	require.NoError(t, app.layout(app.g))

	// The input should be prefilled with the current name.
	v, err := app.g.View("rename-input")
	require.NoError(t, err)
	assert.Equal(t, "devbox", v.TextArea.GetContent())

	setInputContent(v, "devbox2")
	require.NoError(t, app.confirmRename(app.g, nil))
	assert.Equal(t, DialogNone, app.dialog)

	require.Eventually(t, func() bool {
		_, _, renames := p.snapshot()
		return len(renames) == 1
	}, time.Second, 5*time.Millisecond)
	_, _, renames := p.snapshot()
	assert.Equal(t, renameCall{from: "devbox", to: "devbox2"}, renames[0])
}

func TestDeleteDialogFlow(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "logs"}}
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.openConfirmDeleteHandler(app.g, nil))
	assert.Equal(t, DialogConfirmDelete, app.dialog)
	require.NoError(t, app.layout(app.g))

	// n cancels without killing.
	require.NoError(t, app.cancelDialog(app.g, nil))
	assert.Equal(t, DialogNone, app.dialog)
	_, killed, _ := p.snapshot()
	assert.Empty(t, killed)

	// y kills.
	require.NoError(t, app.openConfirmDeleteHandler(app.g, nil))
	require.NoError(t, app.confirmDelete(app.g, nil))
	assert.Equal(t, DialogNone, app.dialog)
	require.Eventually(t, func() bool {
		_, killed, _ := p.snapshot()
		return len(killed) == 1
	}, time.Second, 5*time.Millisecond)
	_, killed, _ = p.snapshot()
	assert.Equal(t, "logs", killed[0])
}

func TestEnterOpensFullscreenNotAttach(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}

	require.NoError(t, app.openFullScreenHandler(app.g, nil))
	assert.True(t, app.fullscreen.IsActive())
	assert.Equal(t, "devbox", app.fullscreen.Target())
	assert.Equal(t, "", app.AttachTarget(), "Enter must not trigger a real attach anymore")
}

func TestFullScreenLayout(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}, {Name: "logs"}}
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.openFullScreenHandler(app.g, nil))
	require.NoError(t, app.layout(app.g))

	_, err := app.g.View("sessions")
	assert.Error(t, err, "sessions view should be deleted in fullscreen")
	_, err = app.g.View("options")
	assert.Error(t, err, "options view should be deleted in fullscreen")
	for _, name := range []string{"main", "fullscreen-bar"} {
		_, err = app.g.View(name)
		assert.NoError(t, err, "view %q should exist", name)
	}
	bar, _ := app.g.View("fullscreen-bar")
	assert.Contains(t, bar.Buffer(), "back")
	assert.Contains(t, bar.Buffer(), "eof")

	// The fullscreen view is framed with the session name as its title,
	// like lazyclaude's fullscreen.
	main, _ := app.g.View("main")
	assert.True(t, main.Frame)
	assert.Equal(t, " devbox ", main.Title)

	// Ctrl+D exits; the dashboard layout returns.
	require.NoError(t, app.exitFullScreenHandler(app.g, nil))
	assert.False(t, app.fullscreen.IsActive())
	require.NoError(t, app.layout(app.g))
	_, err = app.g.View("sessions")
	assert.NoError(t, err, "sessions view should be back after exiting fullscreen")
}

func TestFullScreenEditorForwards(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	editor := &inputEditor{app: app}

	assert.True(t, editor.Edit(nil, 0, 'h', 0))
	assert.True(t, editor.Edit(nil, 0, 'i', 0))
	assert.True(t, editor.Edit(nil, gocui.KeyEnter, 0, 0))
	assert.True(t, editor.Edit(nil, gocui.KeyArrowUp, 0, 0))
	assert.False(t, editor.Edit(nil, 0, 0, 0), "unmapped key should not be consumed")

	app.fullscreen.Exit()
	assert.False(t, editor.Edit(nil, 0, 'x', 0), "no forwarding outside fullscreen")

	p.mu.Lock()
	literals := p.literals["devbox"]
	keys := append([]string(nil), p.keys["devbox"]...)
	p.mu.Unlock()
	assert.Equal(t, "hi", literals)
	assert.Equal(t, []string{"Enter", "Up"}, keys)
}

func TestFullScreenEOFAndCtrlC(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")

	// Ctrl+O sends a literal Ctrl+D (EOF) to the pane.
	require.NoError(t, app.forwardEOFHandler(app.g, nil))

	// Ctrl+C is forwarded in fullscreen (the pane's program gets SIGINT)...
	require.NoError(t, app.ctrlCHandler(app.g, nil))

	p.mu.Lock()
	keys := append([]string(nil), p.keys["devbox"]...)
	p.mu.Unlock()
	assert.Equal(t, []string{"C-d", "C-c"}, keys)

	// ...and quits on the dashboard.
	app.fullscreen.Exit()
	err := app.ctrlCHandler(app.g, nil)
	assert.Equal(t, gocui.ErrQuit, err)
}

func TestFullScreenResizesTargetWindow(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	require.NoError(t, app.layout(app.g))

	// Entering fullscreen resizes the target window to exactly the view size.
	// Headless screen is 120x40; the fullscreen main view spans (0,0)-(119,38)
	// and its inner size is 118x37 (the fork's unconditional -2 inset).
	require.NoError(t, app.openFullScreenHandler(app.g, nil))
	require.NoError(t, app.layout(app.g))

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.resizes) == 1
	}, time.Second, 5*time.Millisecond)
	p.mu.Lock()
	resizes := append([]resizeCall(nil), p.resizes...)
	p.mu.Unlock()
	assert.Equal(t, resizeCall{name: "devbox", width: 118, height: 37}, resizes[0])

	// A second layout at the same size must not resize again.
	require.NoError(t, app.layout(app.g))
	time.Sleep(50 * time.Millisecond)
	p.mu.Lock()
	assert.Len(t, p.resizes, 1)
	p.mu.Unlock()
}

func TestApplySessionRefreshClearsStaleList(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "home"}}

	// Killing the last session exits the tmux server, and the service maps
	// that to an empty list — the stale entry must disappear.
	app.applySessionRefresh(nil, nil)
	assert.Empty(t, app.sessions)
	assert.Nil(t, app.currentSession())
}

func TestScrollStateNavigation(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(20, 80) // 20-line viewport
	// Simulate the snapshot load: 40 lines total.
	ss.lines = make([]string, 40)
	for i := range ss.lines {
		ss.lines[i] = fmt.Sprintf("line-%02d", i)
	}
	ss.total = 40
	ss.loaded = true

	// Enter starts at the live view: the viewport shows the last 20 lines.
	assert.Equal(t, 0, ss.offsetFromBottom)
	vp := ss.viewport()
	assert.Len(t, vp, 20)
	assert.Equal(t, "line-20", vp[0])
	assert.Equal(t, "line-39", vp[19])

	// Scrolling up by 5 shows lines 15-34.
	ss.Move(-5)
	vp = ss.viewport()
	assert.Equal(t, "line-15", vp[0])
	assert.Equal(t, "line-34", vp[19])

	// Top = oldest line; over-scrolling clamps.
	ss.Top()
	assert.Equal(t, 20, ss.offsetFromBottom)
	assert.Equal(t, "line-00", ss.viewport()[0])
	ss.Move(-1)
	assert.Equal(t, 20, ss.offsetFromBottom, "cannot scroll above the oldest line")

	// Bottom returns to the live view; Page moves half a viewport.
	ss.Bottom()
	assert.Equal(t, 0, ss.offsetFromBottom)
	ss.Page(-1)
	assert.Equal(t, 10, ss.offsetFromBottom, "page = half the viewport")

	// The snapshot is frozen: pane-side history changes never alter the
	// viewport (which is exactly why the snapshot exists).
	assert.Equal(t, "line-10", ss.viewport()[0])
	assert.Equal(t, "line-10", ss.viewport()[0], "viewport is stable under pane output")
}

func TestScrollModeKeyHandling(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	// Enter scroll mode; the Editor must consume j/k and not forward them.
	app.enterScrollMode()
	assert.True(t, app.scroll.IsActive())

	editor := &inputEditor{app: app}
	assert.True(t, editor.Edit(nil, 0, 'j', 0), "j scrolls in scroll mode")
	p.mu.Lock()
	literals := p.literals["devbox"]
	p.mu.Unlock()
	assert.Empty(t, literals, "keys must not be forwarded in scroll mode")

	// The async viewport fetch lands eventually (the fake captures ranges).
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.scrollRanges) > 0
	}, time.Second, 5*time.Millisecond)

	// Esc exits scroll mode and returns to live forwarding.
	assert.True(t, editor.Edit(nil, gocui.KeyEsc, 0, 0))
	assert.False(t, app.scroll.IsActive())
	assert.True(t, editor.Edit(nil, 0, 'x', 0), "back to forwarding after scroll mode")
	p.mu.Lock()
	literals = p.literals["devbox"]
	p.mu.Unlock()
	assert.Equal(t, "x", literals)
}

func TestScrollModeCtrlKeysDoNotForward(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	assert.True(t, app.scroll.IsActive())

	// Ctrl+O must not send EOF to the pane while browsing.
	require.NoError(t, app.forwardEOFHandler(app.g, nil))

	// Ctrl+C exits scroll mode (like Esc) instead of interrupting the pane.
	require.NoError(t, app.ctrlCHandler(app.g, nil))
	assert.False(t, app.scroll.IsActive())

	p.mu.Lock()
	keys := append([]string(nil), p.keys["devbox"]...)
	p.mu.Unlock()
	assert.Empty(t, keys, "nothing may be forwarded to the pane in scroll mode")
}

func TestScrollModeWheelAndToggle(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}

	// Wheel on the dashboard does nothing.
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.False(t, app.scroll.IsActive())

	// Wheel in fullscreen enters scroll mode and scrolls — the offset must
	// accumulate even while the snapshot is still loading.
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.True(t, app.scroll.IsActive())
	assert.Equal(t, 3, app.scroll.offsetFromBottom, "the first wheel gesture scrolls")

	// Simulate the snapshot load (headless mode never runs gui.Update); the
	// load clamps against the real total, keeping the accumulated offset.
	app.scroll.lines = make([]string, 50)
	for i := range app.scroll.lines {
		app.scroll.lines[i] = fmt.Sprintf("line-%02d", i)
	}
	app.scroll.total = 50
	app.scroll.loaded = true
	app.scroll.clampOffset()
	assert.Equal(t, 3, app.scroll.offsetFromBottom)

	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.Equal(t, 6, app.scroll.offsetFromBottom)

	// Ctrl+V toggles it off.
	require.NoError(t, app.toggleScrollHandler(app.g, nil))
	assert.False(t, app.scroll.IsActive())

	// Exiting fullscreen also exits scroll mode.
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(5, 80)
	assert.True(t, app.scroll.IsActive())
	app.exitFullScreen()
	assert.False(t, app.scroll.IsActive())
}

func TestFullScreenAutoExitWhenSessionDies(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}, {Name: "logs"}}
	app.fullscreen.Enter("logs")

	// Refresh without "logs": fullscreen should exit and the dashboard cursor
	// should clamp onto the remaining list.
	app.applySessionRefresh([]session.Info{{Name: "devbox"}}, nil)
	assert.False(t, app.fullscreen.IsActive(), "fullscreen should exit when its session disappears")
	assert.Equal(t, "devbox", app.currentSession().Name)

	// A failing refresh must NOT exit fullscreen (the list is unknown and
	// the previous list is kept).
	app.fullscreen.Enter("devbox")
	app.applySessionRefresh(nil, assert.AnError)
	assert.True(t, app.fullscreen.IsActive())
	assert.Equal(t, []session.Info{{Name: "devbox"}}, app.sessions)
}

// setInputContentView fills a named view with text, returning an error if the
// view does not exist.
func setInputContentView(g *gocui.Gui, name, text string) error {
	v, err := g.View(name)
	if err != nil {
		return err
	}
	setInputContent(v, text)
	return nil
}

func TestScrollSnapshotStripsPhantomLine(t *testing.T) {
	// capture-pane -p ends with a newline; it must not become an extra
	// snapshot line (which would shift total and the bottom viewport).
	lines := []string{"", "row-01", "row-02", ""} // includes a real blank first row
	content := strings.Join(lines, "\n") + "\n"
	got := splitScrollback(content, 80)
	assert.Equal(t, lines, got, "exactly one record-terminating newline is stripped")
}

func TestScrollOffsetsAccumulateWhileLoading(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(20, 80) // snapshot not loaded yet

	ss.Move(-5)
	assert.Equal(t, 5, ss.offsetFromBottom, "offsets accumulate while the snapshot loads")

	// The load callback clamps against the real total.
	ss.lines = make([]string, 20)
	ss.total = 20
	ss.loaded = true
	ss.clampOffset()
	assert.Equal(t, 0, ss.offsetFromBottom, "loaded total of 20 clamps a 5-line offset")
}

func TestScrollLoadFailureExitsScrollMode(t *testing.T) {
	p := &fakeProvider{err: assert.AnError}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq

	app.applyScrollLoad(seq, nil, assert.AnError)
	assert.False(t, app.scroll.IsActive(), "a failed load must not strand the user in scroll mode")
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "scrollback:")
}

func TestScrollLoadAppliesSnapshot(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq

	app.applyScrollLoad(seq, []string{"a", "b", "c"}, nil)
	assert.True(t, app.scroll.IsActive())
	assert.True(t, app.scroll.loaded)
	assert.Equal(t, 3, app.scroll.total)

	// A stale load is ignored.
	app.applyScrollLoad(seq-1, []string{"stale"}, nil)
	assert.Equal(t, 3, app.scroll.total, "stale loads must not overwrite the snapshot")
}
