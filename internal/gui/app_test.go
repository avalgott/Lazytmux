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
	altOn        bool                // value returned by PaneInputFlags
	mouseAny     bool
	cursorX      int
	cursorY      int
	wheels       []wheelCall // recorded ForwardMouseWheel calls
	err          error
}

type renameCall struct{ from, to string }

type resizeCall struct {
	name   string
	width  int
	height int
}

type scrollRange struct{ start, end int }

type wheelCall struct {
	name string
	up   bool
	x, y int
}

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

func (f *fakeProvider) CaptureScrollback(_ context.Context, _ string) (session.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := -f.history
	end := f.paneHeight - 1
	f.scrollRanges = append(f.scrollRanges, scrollRange{start, end})
	var sb strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return session.Preview{Content: sb.String(), PaneHeight: f.paneHeight}, f.err
}

func (f *fakeProvider) PaneInputFlags(_ context.Context, _ string) (bool, bool, int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.altOn, f.mouseAny, f.cursorX, f.cursorY, f.err
}

func (f *fakeProvider) ForwardMouseWheel(_ context.Context, name string, up bool, x, y int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wheels = append(f.wheels, wheelCall{name: name, up: up, x: x, y: y})
	return f.err
}

// wheelSnapshot returns copies of the recorded wheel calls for race-free
// assertions.
func (f *fakeProvider) wheelSnapshot() []wheelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wheelCall(nil), f.wheels...)
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

	app.applyScrollLoad(seq, nil, 0, assert.AnError)
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

	app.applyScrollLoad(seq, []string{"a", "b", "c"}, 0, nil)
	assert.True(t, app.scroll.IsActive())
	assert.True(t, app.scroll.loaded)
	assert.Equal(t, 3, app.scroll.total)

	// A stale load is ignored.
	app.applyScrollLoad(seq-1, []string{"stale"}, 0, nil)
	assert.Equal(t, 3, app.scroll.total, "stale loads must not overwrite the snapshot")
}

func TestScrollTopPendingWhileLoading(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(20, 80) // snapshot not loaded yet

	// g pressed during loading must be honored once the load completes.
	ss.Top()
	assert.True(t, ss.pendingTop)

	ss.lines = make([]string, 40)
	ss.total = 40
	ss.loaded = true
	// The load callback resolves the pending request.
	ss.pendingTop = false
	ss.offsetFromBottom = ss.maxOffset()
	assert.Equal(t, 20, ss.offsetFromBottom, "pending top lands on the oldest line")

	// Bottom cancels a pending top.
	ss.Enter(20, 80)
	ss.Top()
	ss.Bottom()
	assert.False(t, ss.pendingTop)
}

func TestScrollPositionClampedToOne(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(20, 80)
	ss.lines = []string{"a", "b"}
	ss.total = 2
	ss.loaded = true
	ss.offsetFromBottom = 0

	assert.Equal(t, 1, ss.position(), "short snapshots must not show position 0 or negative")
}

// --- Dashboard preview scrolling ---

// loadPreviewSnapshot simulates a completed snapshot load in headless mode
// (gui.Update never runs there, so the loader goroutine's result is applied
// via the direct applier with the current seq). A load that lands at the
// live bottom (offset 0) exits the mode by design, so a first upward gesture
// is simulated before the load completes.
func loadPreviewSnapshot(t *testing.T, app *App, lines []string) {
	t.Helper()
	app.previewScroll.Move(-1)
	seq := app.previewScroll.seq
	app.applyPreviewScrollLoad(seq, lines, 0, nil)
	require.True(t, app.previewScroll.loaded)
}

// loadScrollState simulates a completed fullscreen scroll load.
func loadScrollState(t *testing.T, ss *ScrollState, lines []string) {
	t.Helper()
	ss.lines = lines
	ss.total = len(lines)
	ss.loaded = true
	ss.clampOffset()
}

func TestTabCyclesPanelFocus(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	require.NoError(t, app.layout(app.g))
	assert.Equal(t, "sessions", app.g.CurrentView().Name())

	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.True(t, app.focusMain)
	require.NoError(t, app.layout(app.g))
	assert.Equal(t, "main", app.g.CurrentView().Name())

	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.False(t, app.focusMain)
	require.NoError(t, app.layout(app.g))
	assert.Equal(t, "sessions", app.g.CurrentView().Name())
}

func TestTabGuardedInDialogAndFullscreen(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.dialog = DialogCreate
	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.False(t, app.focusMain, "Tab must not change focus while a dialog is open")

	app.dialog = DialogNone
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.False(t, app.focusMain, "Tab must not change focus in fullscreen")
}

func TestPreviewFocusedJScrollsInsteadOfMovingCursor(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	require.NoError(t, app.layout(app.g))
	app.focusMain = true

	// k with the preview focused enters scroll mode (j at the live bottom is
	// a no-op by design); the session cursor must not move either way.
	require.NoError(t, app.cursorMoveHandler(-1)(app.g, nil))
	assert.Equal(t, 0, app.cursor)
	assert.True(t, app.previewScroll.IsActive())
	assert.Equal(t, "a", app.previewScrollTarget)

	// The loader goroutine requests the history capture.
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.scrollRanges) > 0
	}, time.Second, 5*time.Millisecond)

	loadPreviewSnapshot(t, app, make([]string, 40))
	require.NoError(t, app.cursorMoveHandler(-1)(app.g, nil))
	assert.Equal(t, 3, app.previewScroll.offsetFromBottom)
	assert.Equal(t, 0, app.cursor, "cursor must not move while the preview is focused")
}

func TestPreviewScrollViewportRendering(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.focusMain = true
	require.NoError(t, app.layout(app.g))
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(37, 78)

	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i)
	}
	loadPreviewSnapshot(t, app, lines)
	app.previewScroll.offsetFromBottom = 0 // state-only: bottom viewport

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	buf := v.Buffer()
	assert.Contains(t, buf, "line-39", "viewport shows the live bottom")
	assert.NotContains(t, buf, "line-02", "lines above the viewport are not rendered")
	assert.Contains(t, v.Title, "scroll 4/40", "title shows the scroll position")
}

func TestPreviewScrollMoveExitsAtBottom(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	app.previewScroll.Move(-1)
	assert.Equal(t, 2, app.previewScroll.offsetFromBottom)
	app.previewScroll.Move(1)
	assert.Equal(t, 1, app.previewScroll.offsetFromBottom)

	// previewScrollMove exits the mode when the live bottom is reached —
	// dispatch through the preview-focused path (focusMain = true) so the
	// bottom-exit, not the session-change hook, is what is exercised.
	app.focusMain = true
	require.NoError(t, app.cursorMoveHandler(1)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "", app.previewScrollTarget)
}

func TestPreviewScrollLoadAtBottomStays(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	// A snapshot shorter than the viewport clamps the offset to the bottom;
	// the mode stays active (G or a downward gesture returns to live).
	app.applyPreviewScrollLoad(seq, make([]string, 5), 0, nil)
	assert.True(t, app.previewScroll.IsActive(), "small snapshots keep scroll mode active at the bottom")
	assert.True(t, app.previewScroll.loaded)
	assert.Equal(t, 0, app.previewScroll.offsetFromBottom)
}

func TestDashboardWheelScrollsPreview(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	require.NoError(t, app.layout(app.g))

	// Wheel-up enters scroll mode and scrolls up.
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.True(t, app.previewScroll.IsActive())
	loadPreviewSnapshot(t, app, make([]string, 40))
	// 40-line snapshot, 37-row viewport: the accumulated offset clamps to 3.
	assert.Equal(t, 3, app.previewScroll.offsetFromBottom)

	// Wheel-down scrolls towards the live bottom and exits there.
	app.previewScroll.offsetFromBottom = 0
	require.NoError(t, app.wheelHandler(3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "wheel-down at the live bottom returns to live")

	// Wheel-down while inactive does nothing (no expensive load).
	require.NoError(t, app.wheelHandler(3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive())
}

func TestPreviewScrollGExits(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	app.previewScrollBottom()
	assert.False(t, app.previewScroll.IsActive())
}

func TestPreviewScrollTopWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)

	app.previewScrollTop()
	assert.True(t, app.previewScroll.pendingTop)

	// Apply the load directly (the shared helper simulates a first upward
	// gesture, which would cancel the pending top).
	seq := app.previewScroll.seq
	app.applyPreviewScrollLoad(seq, make([]string, 20), 0, nil)
	assert.True(t, app.previewScroll.IsActive())
	assert.Equal(t, 10, app.previewScroll.offsetFromBottom, "pending top lands on the oldest line")
}

func TestPreviewScrollResetsOnSessionChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "a"}, {Name: "b"}}
	app.previewScrollTarget = "a"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))
	app.focusMain = false

	app.moveCursor(1)
	assert.False(t, app.previewScroll.IsActive(), "session change returns the preview to live")
	assert.Equal(t, "", app.previewScrollTarget)
	assert.Equal(t, 1, app.cursor)
}

func TestPreviewScrollResetsOnResize(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	// Seed the stored dimensions so the first layout is NOT seen as a
	// resize; it must preserve the active scroll state.
	app.lastWidth = 120
	app.lastHeight = 40
	require.NoError(t, app.layout(app.g))
	assert.True(t, app.previewScroll.IsActive(), "the initial layout must preserve the scroll state")

	app.lastWidth = 0 // force resize detection
	require.NoError(t, app.layout(app.g))
	assert.False(t, app.previewScroll.IsActive(), "resize returns the preview to live")
}

func TestPreviewScrollResetsWhenTargetDisappears(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}, {Name: "logs"}}
	app.cursor = 1
	app.previewScrollTarget = "logs"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	app.applySessionRefresh([]session.Info{{Name: "devbox"}}, nil)
	assert.False(t, app.previewScroll.IsActive(), "target disappearance returns the preview to live")
	assert.Equal(t, "", app.previewScrollTarget)
}

func TestPreviewScrollLoadFailureExits(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	app.applyPreviewScrollLoad(seq, nil, 0, assert.AnError)
	assert.False(t, app.previewScroll.IsActive())
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "scrollback:")
}

func TestPreviewScrollStaleLoadIgnored(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	app.applyPreviewScrollLoad(app.previewScroll.seq-1, []string{"stale"}, 0, nil)
	assert.Equal(t, 20, app.previewScroll.total, "stale loads must not overwrite the snapshot")
}

func TestPageHandlerDashboardDispatch(t *testing.T) {
	p := &fakeProvider{history: 50, paneHeight: 20}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.focusMain = true
	require.NoError(t, app.layout(app.g))

	// PgUp with the preview focused enters scroll mode and pages.
	require.NoError(t, app.pageHandler("PageUp")(app.g, nil))
	assert.True(t, app.previewScroll.IsActive())
	loadPreviewSnapshot(t, app, make([]string, 40))
	app.previewScroll.offsetFromBottom = 0 // state-only reset before paging
	require.NoError(t, app.pageHandler("PageUp")(app.g, nil))
	// Half the 37-row viewport is 18, but the 40-line snapshot clamps the
	// offset to maxOffset = 40-37 = 3.
	assert.Equal(t, 3, app.previewScroll.offsetFromBottom, "PgUp pages half the viewport, clamped to the snapshot top")
	assert.Equal(t, 0, app.cursor, "paging must not move the session cursor")
}

func TestPageHandlerFullscreenUntouched(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(10, 78)
	loadScrollState(t, app.scroll, make([]string, 20))

	require.NoError(t, app.pageHandler("PageUp")(app.g, nil))
	assert.Equal(t, 5, app.scroll.offsetFromBottom, "fullscreen paging still works")
	assert.False(t, app.previewScroll.IsActive(), "dashboard preview scroll must stay inactive")
}

func TestDialogCloseRestoresFocus(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.focusMain = true
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.openCreateHandler(app.g, nil))
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.cancelDialog(app.g, nil))
	require.NoError(t, app.layout(app.g))
	assert.Equal(t, "main", app.g.CurrentView().Name(), "focus returns to the preview panel")

	app.focusMain = false
	require.NoError(t, app.openRenameHandler(app.g, nil))
	app.sessions = []session.Info{{Name: "devbox"}}
	require.NoError(t, app.layout(app.g))
	require.NoError(t, app.cancelDialog(app.g, nil))
	require.NoError(t, app.layout(app.g))
	assert.Equal(t, "sessions", app.g.CurrentView().Name(), "focus returns to the sessions panel")
}

func TestEnterFullScreenExitsPreviewScroll(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))

	require.NoError(t, app.openFullScreenHandler(app.g, nil))
	assert.False(t, app.previewScroll.IsActive())
	assert.True(t, app.fullscreen.IsActive())
}

func TestPreviewScrollEmptySessionsNoop(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.focusMain = true
	require.NoError(t, app.cursorMoveHandler(1)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive())
}

func TestPreviewScrollNotResetWhenCursorClamped(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "a"}, {Name: "b"}}
	app.cursor = 0
	app.previewScrollTarget = "a"
	app.previewScroll.Enter(10, 78)
	loadPreviewSnapshot(t, app, make([]string, 20))
	app.previewScroll.offsetFromBottom = 0
	app.focusMain = false

	// k at the first session: the cursor clamps to 0 (no change), so the
	// frozen preview must survive.
	app.moveCursor(-1)
	assert.Equal(t, 0, app.cursor)
	assert.True(t, app.previewScroll.IsActive(), "a clamped no-op must not reset the preview")

	// An actual session change still resets it.
	app.moveCursor(1)
	assert.Equal(t, 1, app.cursor)
	assert.False(t, app.previewScroll.IsActive())
}

func TestPendingTopCancelledByLaterNavigation(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(10, 78) // snapshot not loaded yet

	// g while loading records a top request...
	ss.Top()
	assert.True(t, ss.pendingTop)

	// ...but a later j/k/Page gesture is the latest intent.
	ss.Move(-1)
	assert.False(t, ss.pendingTop, "navigation after g cancels the pending top")

	ss.lines = make([]string, 30)
	ss.total = 30
	ss.loaded = true
	ss.clampOffset()
	assert.Equal(t, 1, ss.offsetFromBottom, "the load honors the latest gesture, not the stale top request")
}

func TestPreviewScrollTopThenMoveWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)

	app.previewScrollTop()
	assert.True(t, app.previewScroll.pendingTop)
	app.previewScroll.Move(-2)
	assert.False(t, app.previewScroll.pendingTop)

	seq := app.previewScroll.seq
	app.applyPreviewScrollLoad(seq, make([]string, 30), 0, nil)
	assert.True(t, app.previewScroll.IsActive(), "offset 2 is not the bottom, so browsing continues")
	assert.Equal(t, 2, app.previewScroll.offsetFromBottom, "the later navigation wins over the stale top")
}

// --- Zero-history snapshots (alternate-screen panes like Claude Code) ---

func TestFullscreenScrollZeroHistoryExitsWithHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq

	// 20 lines, pane height 20: the snapshot holds nothing but the visible
	// screen, so scroll mode exits with the no-scrollback hint.
	app.applyScrollLoad(seq, make([]string, 20), 20, nil)
	assert.False(t, app.scroll.IsActive(), "a snapshot without history must not keep scroll mode active")
	assert.True(t, app.fullscreenNoScrollback, "the hint flags the pane as having no scrollback")
}

func TestFullscreenScrollRealHistoryClearsHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.fullscreenNoScrollback = true
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq

	app.applyScrollLoad(seq, make([]string, 25), 20, nil)
	assert.True(t, app.scroll.IsActive())
	assert.False(t, app.fullscreenNoScrollback, "a load with real history clears the hint")
}

func TestFullscreenBarShowsNoScrollbackHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.fullscreenNoScrollback = true

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("fullscreen-bar")
	require.NoError(t, err)
	assert.Contains(t, v.Buffer(), "no scrollback")
}

func TestPreviewScrollZeroHistoryExitsWithStatus(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	app.applyPreviewScrollLoad(seq, make([]string, 5), 5, nil)
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "", app.previewScrollTarget)
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "No scrollback available. Hit Enter to open the session and scrollback inside of it.")
}

// --- Wheel passthrough for mouse-tracking panes in fullscreen ---

func TestFullscreenWheelForwardsToMousePane(t *testing.T) {
	p := &fakeProvider{altOn: true, mouseAny: true, cursorX: 10, cursorY: 5}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")

	// Wheel-up goes to the pane's program, not to scroll mode.
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.False(t, app.scroll.IsActive(), "the wheel must not enter scroll mode over a mouse-tracking pane")
	require.Len(t, p.wheelSnapshot(), 1)
	assert.Equal(t, wheelCall{name: "devbox", up: true, x: 10, y: 5}, p.wheelSnapshot()[0])

	// Wheel-down too.
	require.NoError(t, app.wheelHandler(3)(app.g, nil))
	require.Len(t, p.wheelSnapshot(), 2)
	assert.Equal(t, wheelCall{name: "devbox", up: false, x: 10, y: 5}, p.wheelSnapshot()[1])
}

func TestFullscreenWheelFallsBackToScrollModeWithoutMouse(t *testing.T) {
	app := newTestApp(t, &fakeProvider{altOn: true, mouseAny: false})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.True(t, app.scroll.IsActive(), "an alternate-screen pane without mouse tracking falls back to scroll mode")
}

func TestFullscreenWheelFallsBackOnFlagError(t *testing.T) {
	app := newTestApp(t, &fakeProvider{err: assert.AnError})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.True(t, app.scroll.IsActive(), "a failed flags query falls back to scroll mode")
}

// --- Log dedupe ---

func TestLogsDedupeConsecutiveMessages(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.setStatus("No scrollback available. Hit Enter to open the session and scrollback inside of it.")
	app.setStatus("No scrollback available. Hit Enter to open the session and scrollback inside of it.")
	assert.Len(t, app.logs, 1, "consecutive identical messages collapse into one")

	app.setStatus("No scrollback available. Hit Enter to open the session and scrollback inside of it. other")
	assert.Len(t, app.logs, 2, "a different message appends")

	app.setError("No scrollback available. Hit Enter to open the session and scrollback inside of it. other")
	assert.Len(t, app.logs, 3, "same text with a different kind is kept separate")
}

func TestFullscreenScrollStaleLoadKeepsHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	app.fullscreenNoScrollback = true
	app.scroll.Enter(20, 80)

	// A superseded load (seq bumped by a reload) must not touch the hint.
	app.applyScrollLoad(app.scroll.seq-1, make([]string, 25), 20, nil)
	assert.True(t, app.fullscreenNoScrollback, "a stale load must not clear the no-scrollback hint")
}

// --- Synthetic scrollback buffers ---

func TestFeedBufferAccumulatesPerSession(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.feedBuffer("devbox", "row-a\nrow-b\nrow-c")
	app.feedBuffer("devbox", "row-b\nrow-c\nrow-d")
	assert.Equal(t, []string{"row-a", "row-b", "row-c", "row-d"}, app.bufferFor("devbox").Snapshot())
}

func TestFeedBufferSeparatesSessions(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.feedBuffer("devbox", "one")
	app.feedBuffer("logs", "two")
	assert.Equal(t, []string{"one"}, app.bufferFor("devbox").Snapshot())
	assert.Equal(t, []string{"two"}, app.bufferFor("logs").Snapshot())
}

func TestBuffersPrunedWhenSessionsDisappear(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.feedBuffer("gone", "content")
	app.applySessionRefresh([]session.Info{{Name: "alive"}}, nil)
	assert.NotContains(t, app.buffers, "gone", "buffers of vanished sessions are dropped")
}

func TestCaptureCompletionFeedsBuffer(t *testing.T) {
	p := &fakeProvider{captured: session.Preview{Content: "live", Full: "live\nstreamed"}}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0

	app.renderPreviewCapture("devbox", 0, session.Preview{Content: "live", Full: "live\nstreamed"}, nil)

	assert.Equal(t, []string{"live", "streamed"}, app.bufferFor("devbox").Snapshot())
}

// --- Scroll snapshot sourcing ---

func TestScrollSnapshotFallsBackToBufferForAltScreenPane(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)
	// One screen, then a scrolled screen: the buffer holds one real line of
	// history beyond the current screen.
	app.feedBuffer("devbox", strings.Join([]string{"h1", "h2", "h3", "h4", "h5", "h6", "h7"}, "\n"))
	app.feedBuffer("devbox", strings.Join([]string{"h2", "h3", "h4", "h5", "h6", "h7", "h8"}, "\n"))

	lines, paneH, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Equal(t, 5, paneH, "pane height still reported for the noHistory check")
	assert.Equal(t, []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8"}, lines)
}

func TestScrollSnapshotPrefersRealHistory(t *testing.T) {
	p := &fakeProvider{history: 30, paneHeight: 5}
	app := newTestApp(t, p)
	app.feedBuffer("devbox", "buffered-1\nbuffered-2")

	lines, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Contains(t, lines, "line 0", "tmux history wins over the synthetic buffer")
	assert.NotContains(t, lines, "buffered-1")
}

func TestScrollSnapshotZeroHistoryWithoutBuffer(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)

	lines, paneH, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Len(t, lines, 5)
	assert.Equal(t, 5, paneH, "an empty buffer keeps the zero-history signal")
}

func TestScrollSnapshotTruncatesBufferToWidth(t *testing.T) {
	p := &fakeProvider{paneHeight: 2}
	app := newTestApp(t, p)
	// One screen, then a scrolled screen, so the buffer holds history.
	app.feedBuffer("devbox", "short\n"+strings.Repeat("w", 100)+"\ntail")
	app.feedBuffer("devbox", strings.Repeat("w", 100)+"\ntail\nnext")

	lines, _, err := app.fetchScrollSnapshot("devbox", 20)
	require.NoError(t, err)
	assert.Equal(t, []string{"short", strings.Repeat("w", 20), "tail", "next"}, lines)
}

func TestScrollSnapshotConcurrentWithFeeds(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				app.feedBuffer("devbox", fmt.Sprintf("g%d-%d-a\ng%d-%d-b\ng%d-%d-c", i, j, i, j, i, j))
				_, _, _ = app.fetchScrollSnapshot("devbox", 80)
			}
		}(i)
	}
	wg.Wait()
}

// --- Scroll hint for sessions that keep their own scrollback ---

func TestPreviewScrollZeroHistorySetsScrollHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	app.applyPreviewScrollLoad(seq, make([]string, 5), 5, nil)
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "devbox", app.scrollHintName)
	assert.True(t, app.scrollHintUntil.After(time.Now()), "the hint is transient, starting now")
}

func TestPreviewTitleShowsScrollHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.Contains(t, v.Title, "Hit Enter to open the session")
}

func TestPreviewTitleHintExpires(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintUntil = time.Now().Add(-time.Second)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter to open the session", "an expired hint must not linger")
}

func TestPreviewTitleHintWrongSession(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "other"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter to open the session", "the hint belongs to its own session only")
}

// --- Copilot review fixes: stale captures and loading-time bottom exits ---

func TestStaleCaptureForOldSessionNotRendered(t *testing.T) {
	p := &fakeProvider{captured: session.Preview{Content: "A-CONTENT", Full: "A-CONTENT"}}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "session-A"}, {Name: "session-B"}}
	app.cursor = 0

	// A capture for session-A (index 0) completes...
	app.renderPreviewCapture("session-A", 0, p.captured, nil)

	// ...after the list changed so index 0 now holds session-B.
	app.sessions = []session.Info{{Name: "session-B"}}
	app.cursor = 0

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Buffer(), "A-CONTENT", "a stale capture must not render under the replacement session")
}

func TestPreviewScrollMoveDownExitsWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	// Wheel-up accumulated an offset while the snapshot was still loading.
	app.previewScroll.Move(-3)

	app.focusMain = true
	require.NoError(t, app.cursorMoveHandler(3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "a downward gesture reaching the bottom exits even while loading")
	assert.Equal(t, "", app.previewScrollTarget)
}

func TestPageDownExitsWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	app.previewScroll.Move(-2) // loading, offset 2

	require.NoError(t, app.pageHandler("PageDown")(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "PageDown reaching the bottom exits even while loading")
}

func TestWheelDownExitsWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	app.previewScroll.Move(-3) // loading, offset 3

	require.NoError(t, app.wheelHandler(3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "wheel-down reaching the bottom exits even while loading")
}

// --- Deep-review fixes ---

func TestFailedCaptureRecordsSessionName(t *testing.T) {
	p := &fakeProvider{err: assert.AnError}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0

	app.renderPreviewCapture("devbox", 0, session.Preview{}, p.err)
	app.preview.Lock()
	name := app.preview.Name()
	app.preview.Unlock()
	assert.Equal(t, "devbox", name, "a failed capture must record the session name so needFetch is throttled")
}

func TestScrollHintDoesNotHijackFullscreenTitle(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintUntil = time.Now().Add(time.Minute)
	app.fullscreen.Enter("devbox")

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.Contains(t, v.Title, "devbox")
	assert.NotContains(t, v.Title, "Hit Enter", "the dashboard hint must not leak into the fullscreen title")
}

func TestEnterFullScreenClearsScrollHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	app.enterFullScreen()
	assert.Equal(t, "", app.scrollHintName, "entering fullscreen clears the dashboard scroll hint")
}

func TestFullscreenWheelDownAtBottomDoesNothing(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.wheelHandler(3)(app.g, nil))
	assert.False(t, app.scroll.IsActive(), "wheel-down at the live bottom must not start a whole-history load")
}

func TestTabExitsPreviewScroll(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	app.focusMain = true

	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "Tab focus change returns the preview to the live capture")
	assert.Equal(t, "", app.previewScrollTarget)
}

func TestWheelUpSkippedWhileHintActive(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "no expensive re-load while the no-scrollback hint is showing")
}

// --- Balanced-review fixes ---

func TestScrollSnapshotRecognizesStrippedBlankHistory(t *testing.T) {
	p := &fakeProvider{paneHeight: 10}
	app := newTestApp(t, p)
	// A 10-row pane whose bottom row is always blank: two feeds accumulate
	// exactly 10 stripped lines — one real line of history at len == paneHeight.
	rows := make([]string, 9)
	for i := range rows {
		rows[i] = fmt.Sprintf("r%02d", i)
	}
	app.feedBuffer("devbox", screen(append(append([]string(nil), rows...), "")...))
	scrolled := append(append([]string(nil), rows[1:]...), "r09", "")
	app.feedBuffer("devbox", screen(scrolled...))

	lines, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Equal(t, 10, len(lines), "one scrolled-off line is real history despite len == paneHeight")
	assert.Equal(t, "r00", lines[0])
}

func TestOptionsBarFollowsPanelFocus(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.focusMain = true // preview focused, not yet scrolling

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("options")
	require.NoError(t, err)
	assert.Contains(t, v.Buffer(), "scroll", "j/k scroll the preview while it has focus")
	assert.NotContains(t, v.Buffer(), "move")
}

func TestLineBufferSeedsWithinCap(t *testing.T) {
	b := NewLineBuffer(3)
	b.Feed(screen("a", "b", "c", "d", "e"))
	assert.Equal(t, []string{"c", "d", "e"}, b.Snapshot(), "the first feed must respect the cap")
}

func TestStaleCaptureDoesNotFeedBuffer(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0

	// A capture for a session that is no longer in the list completes.
	app.renderPreviewCapture("gone", 0, session.Preview{Full: "stale"}, nil)
	assert.Nil(t, app.bufferLookup("gone"), "a stale capture must not recreate a vanished session's buffer")
}
