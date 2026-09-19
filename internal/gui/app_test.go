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
	mu               sync.Mutex
	infos            []session.Info
	creates          []session.CreateOpts
	killed           []string
	renames          []renameCall
	resizes          []resizeCall
	captured         session.Preview
	keys             map[string][]string // session -> forwarded tmux key names
	literals         map[string]string   // session -> forwarded literal text
	pastes           map[string]string   // session -> pasted text
	scrollRanges     []scrollRange       // (start,end) pairs passed to CaptureScrollback
	history          int                 // value returned by HistorySize
	paneHeight       int                 // value returned by PaneHeight
	altOn            bool                // value returned by PaneInputFlags
	sgrMouse         bool                // value returned by PaneInputFlags (SGR mouse)
	wheelErr         error               // error for ForwardMouseWheel only
	cursorX          int
	cursorY          int
	wheels           []wheelCall     // recorded ForwardMouseWheel calls
	wheelBlock       chan struct{}   // when set, ForwardMouseWheel blocks until closed
	wheelStarted     chan struct{}   // when set, signaled when ForwardMouseWheel is entered
	flagsCalls       int             // PaneInputFlags invocation count
	flagsGate        chan struct{}   // when set, PaneInputFlags blocks until closed
	flagsGates       []chan struct{} // when set, each PaneInputFlags call waits on the next gate
	paneID           string          // pane ID returned by PaneInputFlags
	scrollbackPaneID string          // pane ID returned by CaptureScrollback
	err              error
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
	return session.Preview{Content: sb.String(), PaneHeight: f.paneHeight, PaneID: f.scrollbackPaneID}, f.err
}

func (f *fakeProvider) PaneInputFlags(ctx context.Context, _ string) (bool, bool, int, int, string, error) {
	f.mu.Lock()
	f.flagsCalls++
	if len(f.flagsGates) > 0 {
		gate := f.flagsGates[0]
		f.flagsGates = f.flagsGates[1:]
		f.mu.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return false, false, 0, 0, "", ctx.Err()
		}
	} else if f.flagsGate != nil {
		f.mu.Unlock()
		select {
		case <-f.flagsGate:
		case <-ctx.Done():
			return false, false, 0, 0, "", ctx.Err()
		}
	} else {
		f.mu.Unlock()
	}
	return f.altOn, f.sgrMouse, f.cursorX, f.cursorY, f.paneID, f.err
}

func (f *fakeProvider) ForwardMouseWheel(_ context.Context, name string, up bool, x, y int) error {
	if f.wheelStarted != nil {
		f.wheelStarted <- struct{}{}
	}
	if f.wheelBlock != nil {
		<-f.wheelBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wheels = append(f.wheels, wheelCall{name: name, up: up, x: x, y: y})
	if f.wheelErr != nil {
		return f.wheelErr
	}
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

	app.applyScrollLoad(seq, app.sessionGen.Load(), "", "", nil, 0, assert.AnError)
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

	app.applyScrollLoad(seq, app.sessionGen.Load(), "", "", []string{"a", "b", "c"}, 0, nil)
	assert.True(t, app.scroll.IsActive())
	assert.True(t, app.scroll.loaded)
	assert.Equal(t, 3, app.scroll.total)

	// A stale load is ignored.
	app.applyScrollLoad(seq-1, app.sessionGen.Load(), "", "", []string{"stale"}, 0, nil)
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
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", lines, 0, nil)
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
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 5), 0, nil)
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
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 20), 0, nil)
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

	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", nil, 0, assert.AnError)
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

	app.applyPreviewScrollLoad(app.previewScroll.seq-1, app.sessionGen.Load(), "", "", []string{"stale"}, 0, nil)
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
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 30), 0, nil)
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
	app.applyScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 20), 20, nil)
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

	app.applyScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 25), 20, nil)
	assert.True(t, app.scroll.IsActive())
	assert.False(t, app.fullscreenNoScrollback, "a load with real history clears the hint")
}

func TestFullscreenBarShowsNoScrollbackHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g)) // settle the geometry before the resize branch would clear the flag
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

	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 5), 5, nil)
	// The hint applies on the event loop via g.Update (headless no-op) —
	// drive the applier directly.
	app.applyNoHistoryHint("devbox", false, false, false)
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "", app.previewScrollTarget)
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "No scrollback available.")
}

// --- Wheel passthrough for mouse-tracking panes in fullscreen ---

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
	app.applyScrollLoad(app.scroll.seq-1, app.sessionGen.Load(), "", "", make([]string, 25), 20, nil)
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

	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "live", Full: "live\nstreamed"}, nil)

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

	lines, paneH, _, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Equal(t, 7, paneH, "the normalized screen height travels in the pane-height slot for the noHistory check")
	assert.Equal(t, []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8"}, lines)
}

func TestScrollSnapshotPrefersRealHistory(t *testing.T) {
	p := &fakeProvider{history: 30, paneHeight: 5}
	app := newTestApp(t, p)
	app.feedBuffer("devbox", "buffered-1\nbuffered-2")

	lines, _, _, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Contains(t, lines, "line 0", "tmux history wins over the synthetic buffer")
	assert.NotContains(t, lines, "buffered-1")
}

func TestScrollSnapshotZeroHistoryWithoutBuffer(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)

	lines, paneH, _, _, err := app.fetchScrollSnapshot("devbox", 80)
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

	lines, _, _, _, err := app.fetchScrollSnapshot("devbox", 20)
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
				_, _, _, _, _ = app.fetchScrollSnapshot("devbox", 80)
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

	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 5), 5, nil)
	app.applyNoHistoryHint("devbox", false, false, false)
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "devbox", app.scrollHintName)
	assert.True(t, app.scrollHintUntil.After(time.Now()), "the hint is transient, starting now")
}

func TestPreviewTitleShowsScrollHint(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintIdent = "@0@0" // ID, Created and PID default to empty/0
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
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintIdent = sessionIdentity(app.sessions[0])
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
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintIdent = "other@0@0"
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
	app.renderPreviewCapture("session-A", 0, app.sessionGen.Load(), 0, p.captured, nil)

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

	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{}, p.err)
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

func TestTabPreservesPreviewScroll(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	app.focusMain = true

	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.True(t, app.previewScroll.IsActive(), "Tab focus change keeps the frozen scroll position")
	assert.Equal(t, "devbox", app.previewScrollTarget)
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

	lines, _, _, _, err := app.fetchScrollSnapshot("devbox", 80)
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

	// The capture started before a session refresh changed the world.
	gen := app.sessionGen.Load()
	app.sessionGen.Add(1)
	app.renderPreviewCapture("gone", 0, gen, 0, session.Preview{Full: "stale"}, nil)
	assert.Nil(t, app.bufferLookup("gone"), "a stale capture must not recreate a vanished session's buffer")
}

func TestSyntheticHistorySurvivesApplierCheck(t *testing.T) {
	p := &fakeProvider{paneHeight: 10}
	app := newTestApp(t, p)
	rows := make([]string, 9)
	for i := range rows {
		rows[i] = fmt.Sprintf("r%02d", i)
	}
	app.feedBuffer("devbox", screen(append(append([]string(nil), rows...), "")...))
	scrolled := append(append([]string(nil), rows[1:]...), "r09", "")
	app.feedBuffer("devbox", screen(scrolled...))

	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(8, 78)
	seq := app.previewScroll.seq
	lines, paneH, _, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", lines, paneH, nil)

	assert.True(t, app.previewScroll.IsActive(), "the applier must accept synthetic history the fetch accepted")
	assert.True(t, app.previewScroll.loaded)
}

// --- Copilot balanced-review follow-ups ---

func TestTopThenDownExitsWhileLoading(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	app.focusMain = true
	// g while loading: pendingTop, offset pinned at zero.
	app.previewScrollTop()
	assert.True(t, app.previewScroll.IsActive())

	// A quick j is a downward gesture at the bottom: return to live.
	require.NoError(t, app.cursorMoveHandler(1)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "g then j while loading must not freeze the preview at the bottom")
}

// --- Copilot round-5 fixes ---

func TestMarkFetchedClearsForeignContent(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.preview.Lock()
	app.preview.Update("session-A", "A-CONTENT", 0, "", 0, 0, 0)
	app.preview.MarkFetched("session-B", 0, "", 0)
	assert.Equal(t, "", app.preview.Content(), "a failed fetch for another session must not retag the old content")
	app.preview.Unlock()
}

func TestMarkFetchedKeepsOwnContent(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.preview.Lock()
	app.preview.Update("session-A", "A-CONTENT", 0, "", 0, 0, 0)
	app.preview.MarkFetched("session-A", 0, "", 0)
	assert.Equal(t, "A-CONTENT", app.preview.Content(), "a failed fetch for the same session keeps the cached content")
	app.preview.Unlock()
}

func TestStaleGenerationDoesNotInstallResult(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox"}}
	gen := app.sessionGen.Load()
	app.sessionGen.Add(1) // a refresh happened while the capture was in flight

	app.renderPreviewCapture("devbox", 0, gen, 0, session.Preview{Content: "STALE", Full: "STALE"}, nil)
	app.preview.Lock()
	content := app.preview.Content()
	app.preview.Unlock()
	assert.Equal(t, "", content, "a stale completion must not install its result into the render cache")
}

// --- Copilot round-6 fix: identity-bound buffers ---

func TestBufferResetWhenSessionIdentityChanges(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1"}}, nil)
	app.feedBuffer("devbox", "old-session-output")

	// The buffer survives a refresh that sees the same identity.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1"}}, nil)
	assert.NotNil(t, app.bufferLookup("devbox"))

	// A recreated session (same name, new ID) must not inherit the old
	// session's captured scrollback.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2"}}, nil)
	assert.Nil(t, app.bufferLookup("devbox"), "a recreated session must start with a fresh buffer")
}

// --- Copilot round-7 fix: generation advances only on identity change ---

func TestSessionGenStableAcrossIdenticalRefreshes(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	list := []session.Info{{Name: "devbox", ID: "$1"}}

	app.applySessionRefresh(list, nil)
	gen := app.sessionGen.Load()
	app.applySessionRefresh(list, nil)
	assert.Equal(t, gen, app.sessionGen.Load(), "an unchanged session list must not invalidate in-flight captures")

	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2"}}, nil)
	assert.NotEqual(t, gen, app.sessionGen.Load(), "a recreated session (new ID) invalidates in-flight captures")
}

// --- Copilot round-7 fixes: identity-bound state and shrink handling ---

func TestStaleGenCacheNotRendered(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "OLD-SCREEN", Full: "OLD-SCREEN"}, nil)

	// The session is recreated; the cached entry belongs to the old world.
	app.sessionGen.Add(1)
	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Buffer(), "OLD-SCREEN", "a cache entry from a previous generation must not render")
}

func TestScrollHintBoundToSessionID(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$2"}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintIdent = "$1@100@0" // the hint belongs to the previous incarnation
	app.scrollHintUntil = time.Now().Add(time.Minute)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter", "a recreated session must not inherit the old scroll hint")
}

func TestPreviewScrollExitsWhenSessionIDChanges(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.previewScrollTarget = "devbox"
	app.previewScrollTargetID = "$1@0@0"
	app.previewScroll.Enter(10, 78)

	// Same name, recreated session: the frozen snapshot belongs to the dead
	// pane and must be dropped.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2"}}, nil)
	assert.False(t, app.previewScroll.IsActive(), "a recreated session must not keep browsing the dead snapshot")
	assert.Equal(t, "", app.previewScrollTarget)
	assert.Equal(t, "", app.previewScrollTargetID)
}

// --- Copilot round-8 fix: unbound buffers must not adopt a new identity ---

func TestUnboundBufferDroppedOnRecreation(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1"}}, nil)
	// A feed lands between polls, before any ID binding exists.
	app.feedBuffer("devbox", "stale-output")

	// The session died and was recreated before the next poll.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2"}}, nil)
	assert.Nil(t, app.bufferLookup("devbox"), "an unbound buffer from the previous incarnation must not adopt the new identity")
}

func TestUnboundBufferBoundWhenIdentityUnchanged(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1"}}, nil)
	app.feedBuffer("devbox", "live-output")

	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1"}}, nil)
	assert.NotNil(t, app.bufferLookup("devbox"), "an unbound buffer binds when the session identity did not change")
}

// --- Copilot round-8 fixes: repeated content, cap reseed, fetch throttle, wheel coords ---

func TestMarkFetchedRecordsGeneration(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.preview.Lock()
	app.preview.MarkFetched("devbox", 7, "", 0)
	assert.Equal(t, uint64(7), app.preview.Gen(), "the capture generation rides through a failed fetch so the throttle holds")
	app.preview.Unlock()
}

// --- Copilot round-9 fixes: conditional hint wording, single-impulse wheel ---

func TestScrollHintOffersEnterOnlyWhenForwardingAvailable(t *testing.T) {
	p := &fakeProvider{altOn: true, sgrMouse: true}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 5), 5, nil)
	app.applyNoHistoryHint("devbox", true, true, false)
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "Hit Enter", "an alt-screen pane can be scrolled inside, so the hint says how")
}

func TestScrollHintPlainForPlainPanes(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "", "", make([]string, 5), 5, nil)
	app.applyNoHistoryHint("devbox", false, false, false)
	require.NotEmpty(t, app.logs)
	assert.NotContains(t, app.logs[len(app.logs)-1].msg, "Hit Enter", "a plain shell cannot be scrolled inside; the hint must not send the user there")
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "No scrollback available.")
}

// --- Copilot round-10 fixes ---

func TestSessionGenAdvancesOnRecreatedIDWithNewCreated(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 100}}, nil)
	gen := app.sessionGen.Load()

	// A tmux server restart recycles session IDs: same name and ID, but the
	// creation timestamp differs — that must still invalidate.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 200}}, nil)
	assert.NotEqual(t, gen, app.sessionGen.Load(), "a recycled ID with a new creation time is a new session")
}

func TestClampWheelCoordsWithinView(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	require.NoError(t, app.layout(app.g))

	x, y := app.clampWheelCoords(-1, 500)
	assert.Equal(t, 0, x, "frame coordinates clamp to the content area")
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.Equal(t, v.InnerHeight()-1, y)

	x, y = app.clampWheelCoords(5, 5)
	assert.Equal(t, 5, x)
	assert.Equal(t, 5, y)
}

// --- Copilot round-11 fixes ---

func TestBufferResetOnRecycledIDWithNewCreated(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 100}}, nil)
	app.feedBuffer("devbox", "old-server-output")

	// tmux restarted: the ID recycled to $0 but the session is new.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 200}}, nil)
	assert.Nil(t, app.bufferLookup("devbox"), "a recycled ID with a new creation time is a new session")
}

func TestApplyNoHistoryHintPlain(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)

	app.applyNoHistoryHint("devbox", false, false, false)
	assert.False(t, app.previewScroll.IsActive())
	assert.Equal(t, "devbox", app.scrollHintName)
	assert.Equal(t, "$1@0@0", app.scrollHintIdent)
	require.NotEmpty(t, app.logs)
	assert.NotContains(t, app.logs[len(app.logs)-1].msg, "Hit Enter")
}

func TestApplyNoHistoryHintWithForwarding(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1"}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)

	app.applyNoHistoryHint("devbox", true, true, false)
	require.NotEmpty(t, app.logs)
	assert.Contains(t, app.logs[len(app.logs)-1].msg, "Hit Enter")
}

// --- Copilot round-12 fixes ---

func TestScrollHintNotInheritedByRecycledID(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$0", Created: 200}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintIdent = "$0@100@0" // same ID, previous server incarnation
	app.scrollHintUntil = time.Now().Add(time.Minute)

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter", "a recycled ID with a new creation time must not inherit the hint")
}

// --- Copilot round-13 fixes ---

func TestBufferBindingClearedOnCreation(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}}, nil)
	app.feedBuffer("devbox", "first")
	// Identity mismatch: the buffer dies, the binding moves to the new identity.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 200}}, nil)
	// The session vanishes; its binding entry would linger unpruned.
	app.applySessionRefresh(nil, nil)
	// A new incarnation feeds a fresh buffer and must keep it.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 300}}, nil)
	app.feedBuffer("devbox", "second")
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 300}}, nil)
	assert.NotNil(t, app.bufferLookup("devbox"), "a fresh buffer must not be deleted by a lingering stale binding")
}

func TestNoHistoryHintStaleLoadDiscarded(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(10, 78)
	seq := app.previewScroll.seq

	// The user leaves scroll browsing before the async flags query returns.
	app.exitPreviewScroll()
	assert.False(t, app.noHistoryHintCurrent("devbox", seq), "a stale hint callback must not touch the newer state")
}

func TestPreviewScrollExitsOnRecycledID(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$0", Created: 100}}
	app.previewScrollTarget = "devbox"
	app.previewScrollTargetID = "$0@100@0"
	app.previewScroll.Enter(10, 78)

	// Same name, same recycled ID, new creation time: a different session.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 200}}, nil)
	assert.False(t, app.previewScroll.IsActive(), "a recycled ID with a new creation time must drop the frozen snapshot")
}

// --- Copilot round-14 fix: no input injection after leaving fullscreen ---

// --- Copilot round-15 fixes ---

func TestPositionlessFullscreenWheelIgnored(t *testing.T) {
	p := &fakeProvider{altOn: true, sgrMouse: true, cursorX: 10, cursorY: 5}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.fullscreen.Enter("devbox")

	// A global wheel event (mouse over the status bar) has no position:
	// nothing may be forwarded at the pane cursor.
	app.wheel(-3, 0, 0, false)
	assert.Empty(t, p.wheelSnapshot(), "positionless fullscreen events must not inject input")
	assert.False(t, app.scroll.IsActive())
}

// --- Copilot round-16 fixes ---

// --- Copilot round-17 fix: forwards invalidated by scroll-mode transitions ---

// --- Copilot round-18 fixes: ordered wheel processing ---

// --- Copilot round-19 fixes ---

// --- Copilot round-20 fixes ---

func TestFullscreenScrollLoadRejectedAfterRecreation(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq
	sGen := app.sessionGen.Load()

	// The session is recreated under the same name while the load is in flight.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 200}}, nil)

	app.applyScrollLoad(seq, sGen, "", "", make([]string, 25), 20, nil)
	assert.False(t, app.scroll.loaded, "a stale fullscreen load must not install the old pane's snapshot")
}

// --- Copilot round-21 fixes ---

func TestSessionGenAdvancesOnServerRestart(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 100, ServerPID: 1111}}, nil)
	gen := app.sessionGen.Load()

	// A same-second restart recycles the ID and the creation second — only
	// the server PID distinguishes the incarnations.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$0", Created: 100, ServerPID: 2222}}, nil)
	assert.NotEqual(t, gen, app.sessionGen.Load(), "a server restart must invalidate even with recycled ID and same-second creation")
}

// --- Copilot round-22 fixes ---

func TestStaleGenFullscreenLoadRestarts(t *testing.T) {
	p := &fakeProvider{paneHeight: 10}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))
	app.scroll.Enter(20, 80)
	seq := app.scroll.seq
	sGen := app.sessionGen.Load()

	p.mu.Lock()
	n0 := len(p.scrollRanges)
	p.mu.Unlock()
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$9"}}, nil)
	app.applyScrollLoad(seq, sGen, "", "", make([]string, 25), 10, nil)

	assert.True(t, app.scroll.IsActive(), "the rejected load must restart, not strand the panel")
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.scrollRanges) > n0
	}, time.Second, 10*time.Millisecond, "a replacement load must be requested under the current generation")
}

func TestStaleGenPreviewLoadRestarts(t *testing.T) {
	p := &fakeProvider{paneHeight: 10}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	seq := app.previewScroll.seq
	sGen := app.sessionGen.Load()

	app.previewScrollTargetID = sessionIdentity(session.Info{ID: "$1", Created: 100})
	p.mu.Lock()
	n0 := len(p.scrollRanges)
	p.mu.Unlock()
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$9"}}, nil)
	app.applyPreviewScrollLoad(seq, sGen, "", "", make([]string, 25), 10, nil)

	assert.True(t, app.previewScroll.IsActive(), "the rejected load must restart, not strand the panel")
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.scrollRanges) > n0
	}, time.Second, 10*time.Millisecond, "a replacement load must be requested under the current generation")
}

// --- Copilot round-23 fixes ---

func TestMarkFetchedClearsForeignGeneration(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.preview.Lock()
	app.preview.Update("devbox", "OLD-SCREEN", 1, "", 0, 0, 0)
	// A failed capture for the recreated incarnation must not retag the
	// previous pane's content.
	app.preview.MarkFetched("devbox", 2, "", 0)
	assert.Equal(t, "", app.preview.Content(), "a generation change is a different session incarnation")
	app.preview.Unlock()
}

// --- Copilot round-24 fix: renames must invalidate ---

func TestSessionGenAdvancesOnRename(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}}, nil)
	gen := app.sessionGen.Load()

	// Same ID, creation time, and server: only the name changed.
	app.applySessionRefresh([]session.Info{{Name: "devbox2", ID: "$1", Created: 100}}, nil)
	assert.NotEqual(t, gen, app.sessionGen.Load(), "a rename must invalidate in-flight captures")
}

// --- Copilot round-25 fixes ---

func TestPageStepAtLeastOneLine(t *testing.T) {
	ss := &ScrollState{}
	ss.Enter(1, 80)
	ss.lines = make([]string, 10)
	ss.total = 10
	ss.loaded = true
	ss.Page(-1)
	assert.Equal(t, 1, ss.offsetFromBottom, "a one-row viewport still pages one line")
	ss.Page(1)
	assert.Equal(t, 0, ss.offsetFromBottom)
}

// --- Copilot round-26 fixes ---

// --- Copilot round-27 fixes ---

// --- Copilot round-30 fixes ---

func TestFullscreenCachesResetOnIdentityChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreenNoScrollback = true
	app.lastResizeName = "devbox"

	// Any identity change must clear the per-target fullscreen caches.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$9"}}, nil)
	assert.False(t, app.fullscreenNoScrollback, "the no-scrollback verdict must not describe a dead pane")
	assert.Equal(t, "", app.lastResizeName, "the resize cache must not skip resizing a recreated window")
}

func TestScrollHintSkippedOnlyWhileBufferEmpty(t *testing.T) {
	p := &fakeProvider{paneHeight: 5}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.scrollHintName = "devbox"
	app.scrollHintIdent = sessionIdentity(app.sessions[0])
	app.scrollHintUntil = time.Now().Add(time.Minute)

	// Empty buffer: the hint still suppresses the expensive load.
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.False(t, app.previewScroll.IsActive(), "an empty buffer keeps the hint in force")

	// The program starts streaming: the buffer gains history, and the next
	// gesture must browse instead of being suppressed.
	require.NoError(t, app.layout(app.g))
	app.feedBuffer("devbox", "h1\nh2\nh3\nh4\nh5\nh6\nh7")
	app.feedBuffer("devbox", "h2\nh3\nh4\nh5\nh6\nh7\nh8")
	require.NoError(t, app.wheelHandler(-3)(app.g, nil))
	assert.True(t, app.previewScroll.IsActive(), "history in the buffer must override the stale hint")
	assert.True(t, app.scrollHintUntil.IsZero(), "the stale hint must expire when the buffer gains history")
	assert.Equal(t, "", app.scrollHintMsg)
}

// --- Copilot round-32 fixes: pane identity ---

func TestBufferResetOnActivePaneChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0

	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P1", Full: "pane-one-content", PaneID: "%1"}, nil)
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P2", Full: "pane-two-content", PaneID: "%2"}, nil)

	snap := app.bufferFor("devbox").Snapshot()
	assert.Equal(t, []string{"pane-two-content"}, snap, "the active pane changed: the old pane's buffer must be dropped")
}

// --- Copilot round-33 fixes ---

func TestUnboundBufferKeptWhenUnrelatedSessionChanges(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}}, nil)
	app.feedBuffer("devbox", "live-output")

	// An unrelated session appears: the global generation advances, but
	// devbox's identity did not — its buffer must survive.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$9"}}, nil)
	assert.NotNil(t, app.bufferLookup("devbox"), "an unrelated session change must not drop this session's history")
}

// --- Copilot round-34 fixes ---

func TestStaleCaptureDoesNotRecordPane(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	gen := app.sessionGen.Load()
	app.sessionGen.Add(1) // the session changed while the capture was in flight

	app.renderPreviewCapture("devbox", 0, gen, 0, session.Preview{Content: "P", Full: "P", PaneID: "%9"}, nil)
	app.buffersMu.Lock()
	_, recorded := app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.False(t, recorded, "a stale capture must not repopulate pane metadata")
}

// --- Copilot round-35 fixes ---

func TestScrollSnapshotBufferUsedOnlyForMatchingPane(t *testing.T) {
	p := &fakeProvider{paneHeight: 5, scrollbackPaneID: "%1"}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	// Record pane %1 as the live pane and accumulate history.
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.feedBuffer("devbox", "h1\nh2\nh3\nh4\nh5\nh6\nh7")
	app.feedBuffer("devbox", "h2\nh3\nh4\nh5\nh6\nh7\nh8")

	// The scrollback capture comes from the same pane: the buffer is used.
	lines, _, _, _, err := app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.Contains(t, lines[0], "h1")

	// The active pane changed: the old pane's history must not be shown.
	p.scrollbackPaneID = "%2"
	lines, _, _, _, err = app.fetchScrollSnapshot("devbox", 80)
	require.NoError(t, err)
	assert.NotContains(t, lines[0], "h1", "the previous pane's history must not appear under the new pane")
}

// --- Copilot round-37 fixes ---

func TestPaneSwitchBeforeScrollingRebindsAndApplies(t *testing.T) {
	p := &fakeProvider{paneHeight: 5, scrollbackPaneID: "%2"}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	seq := app.previewScroll.seq
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "%2", "%1", make([]string, 25), 5, nil)

	assert.True(t, app.previewScroll.loaded, "the freshly captured snapshot is authoritative for its pane and must apply")
	app.buffersMu.Lock()
	rebound := app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.Equal(t, "%2", rebound, "the pane observed by the capture rebinds the session")
}

// --- Copilot round-38 fix ---

func TestStalePreviewLoadDoesNotRebindPane(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$2", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	// The new target has its own recorded pane and a buffer.
	app.renderPreviewCapture("other", 0, app.sessionGen.Load(), 0, session.Preview{Content: "O", Full: "O", PaneID: "%2"}, nil)
	app.feedBuffer("other", "other-history")

	// Enter and exit scrolling, then re-enter for another session.
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	oldSeq := app.previewScroll.seq
	app.exitPreviewScroll()
	app.cursor = 1
	app.previewScrollTarget = "other"
	app.previewScroll.Enter(20, 78)

	// A stale load from the previous session must not rebind "other" to the
	// old pane or discard its buffer.
	app.applyPreviewScrollLoad(oldSeq, app.sessionGen.Load(), "", "%9", make([]string, 25), 5, nil)
	app.buffersMu.Lock()
	recorded := app.paneIDs["other"]
	_, hasBuffer := app.buffers["other"]
	app.buffersMu.Unlock()
	assert.Equal(t, "%2", recorded, "a stale load must not rebind the new target's pane")
	assert.True(t, hasBuffer, "a stale load must not discard the new target's buffer")
}

// --- Copilot round-39 fixes ---

func TestFullscreenScrollExitsOnTargetRecreation(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	app.fullscreenIdent = sessionIdentity(app.sessions[0])
	app.scroll.Enter(20, 80)
	loadScrollState(t, app.scroll, make([]string, 25))

	// The target is recreated under the same name.
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 200}}, nil)
	assert.False(t, app.scroll.IsActive(), "the old session's frozen scrollback must not survive its recreation")
}

func TestPaneIDsPrunedOnIdentityChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$1", Created: 100}}, nil)
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	app.applySessionRefresh([]session.Info{{Name: "devbox", ID: "$2", Created: 200}}, nil)
	app.buffersMu.Lock()
	_, recorded := app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.False(t, recorded, "a recreated session must not keep its predecessor's pane ID")
}

func TestAdoptPaneDoesNotOverwriteNewerBinding(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%2"}, nil)

	// A live capture recorded a NEWER pane after the scroll fetch.
	app.adoptPaneIfStale("devbox", "%2", "%9")
	app.buffersMu.Lock()
	recorded := app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.Equal(t, "%9", recorded, "the fetch-time binding adopts the snapshot's pane")

	// A capture that started AFTER the adoption rebinds to its pane.
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "Q", Full: "Q", PaneID: "%3"}, nil)
	app.buffersMu.Lock()
	recorded = app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.Equal(t, "%3", recorded, "a capture newer than the adoption wins")
}

// --- Copilot round-40 fixes ---

func TestStalePaneCacheNotRendered(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "OLD-PANE-SCREEN", Full: "P", PaneID: "%1"}, nil)

	// The active pane changed after the cache was populated.
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.buffersMu.Unlock()

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Buffer(), "OLD-PANE-SCREEN", "the old pane's cached screen must not render under the new pane")
}

func TestStalePaneFeedSkipped(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	// The active pane changed; the in-flight capture for the old pane lands
	// (the newer binding carries a higher capture sequence).
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.paneSeq["devbox"] = 1
	app.buffersMu.Unlock()
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 0, session.Preview{Content: "Q", Full: "old-pane-history", PaneID: "%1"}, nil)

	snap := app.bufferFor("devbox").Snapshot()
	assert.Equal(t, []string{"P"}, snap, "the old pane's feed must not land after the pane changed")
}

// --- Copilot round-41 fixes ---

func TestTabAppliesFocusImmediately(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	require.NoError(t, app.layout(app.g))

	require.NoError(t, app.cycleFocusHandler(app.g, nil))
	assert.Equal(t, "main", app.g.CurrentView().Name(), "the view focus must follow the toggle before any redraw")
}

// --- Copilot round-42 fixes ---

func TestPreviewLoadDiscardedWhenNewerPaneBinding(t *testing.T) {
	p := &fakeProvider{paneHeight: 5, scrollbackPaneID: "%1"}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 1, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	// A newer live capture rebinds the pane while the scroll fetch is in
	// flight; the fetched snapshot still sees the old pane.
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.paneSeq["devbox"] = 2
	app.buffersMu.Unlock()

	app.previewScrollTarget = "devbox"
	app.previewScrollTargetID = sessionIdentity(app.sessions[0])
	app.previewScroll.Enter(20, 78)
	seq := app.previewScroll.seq
	p.mu.Lock()
	n0 := len(p.scrollRanges)
	p.mu.Unlock()
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "%1", "%1", make([]string, 25), 5, nil)

	assert.False(t, app.previewScroll.loaded, "the stale pane's snapshot must not install")
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.scrollRanges) > n0
	}, time.Second, 10*time.Millisecond, "the load must restart under the newer binding")
}

// --- Copilot round-43 fixes ---

func TestHintInvalidatedByPaneChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), 1, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	app.applyNoHistoryHint("devbox", false, false, false)
	assert.Equal(t, "%1", app.scrollHintPane)

	// The pane changes: the hint belongs to the old pane and must not
	// suppress the new pane's scrolling.
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.paneSeq["devbox"] = 2
	app.buffersMu.Unlock()
	require.NoError(t, app.layout(app.g))
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	seq := app.previewScroll.seq
	app.applyPreviewScrollLoad(seq, app.sessionGen.Load(), "%2", "%2", make([]string, 25), 5, nil)
	assert.True(t, app.previewScroll.loaded, "a pane change must invalidate the old pane's hint")
}

func TestAdoptPaneReservesCaptureSequence(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	cs := app.captureSeq.Add(1)
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), cs, session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	app.adoptPaneIfStale("devbox", "%1", "%2")
	// A live capture started before the adoption completes afterwards: its
	// pane must not rebind the session back.
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), cs, session.Preview{Content: "Q", Full: "Q", PaneID: "%1"}, nil)
	app.buffersMu.Lock()
	recorded := app.paneIDs["devbox"]
	app.buffersMu.Unlock()
	assert.Equal(t, "%2", recorded, "an in-flight capture predating the adoption must not rebind the session")
}

// --- Copilot round-44 fixes ---

func TestFullscreenStaleSeqSkipsPaneAdoption(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}, {Name: "other", ID: "$2", Created: 100}}
	app.fullscreen.Enter("devbox")
	app.scroll.Enter(20, 80)
	oldSeq := app.scroll.seq
	app.exitFullScreen()
	app.fullscreen.Enter("other")

	app.applyScrollLoad(oldSeq, app.sessionGen.Load(), "%9", "%1", make([]string, 25), 5, nil)
	app.buffersMu.Lock()
	_, recorded := app.paneIDs["other"]
	app.buffersMu.Unlock()
	assert.False(t, recorded, "a stale load must not bind its pane to the new target")
}

func TestNoHistoryRechecksPaneAtApply(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.previewScrollTarget = "devbox"
	app.previewScroll.Enter(20, 78)
	seq := app.previewScroll.seq

	// A live capture rebinds the pane after the snapshot fetch.
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.paneSeq["devbox"] = app.captureSeq.Add(1)
	app.buffersMu.Unlock()

	app.applyNoHistory("devbox", seq, false, false, "%1", "%1", false)
	assert.Equal(t, "", app.scrollHintName, "the stale pane's verdict must not install against the replacement")
}

// --- Copilot round-45 fix ---

func TestSettlePaneLockedStaleSnapshotRejected(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)

	// A fetch-time binding of "%1" adopts "%2"...
	app.fsMu.Lock()
	ok := app.settlePaneLocked("devbox", "%1", "%2")
	app.fsMu.Unlock()
	assert.True(t, ok)

	// ...and a snapshot fetched against "%1" is now stale.
	app.fsMu.Lock()
	ok = app.settlePaneLocked("devbox", "%1", "%3")
	app.fsMu.Unlock()
	assert.False(t, ok, "a snapshot whose fetch-time binding is gone must be rejected")
}

// --- Copilot round-46 fix ---

func TestTitleHintHiddenOnPaneChange(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.scrollHintName = "devbox"
	app.scrollHintIdent = sessionIdentity(app.sessions[0])
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintPane = "%1"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	// The active pane changes during the hint window.
	app.buffersMu.Lock()
	app.paneIDs["devbox"] = "%2"
	app.buffersMu.Unlock()

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter", "the replacement pane must not inherit the old pane's verdict")
}

// --- Copilot round-47 fix ---

func TestWheelForwardsSynchronously(t *testing.T) {
	p := &fakeProvider{altOn: true, sgrMouse: true, paneID: "%5"}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	app.wheelHandlerAt(-3, 12, 7)
	require.Len(t, p.wheelSnapshot(), 1, "the wheel must query and forward synchronously")
	assert.Equal(t, wheelCall{name: "%5", up: true, x: 12, y: 7}, p.wheelSnapshot()[0])
}

// --- Copilot round-49 fixes ---

// --- Copilot round-50 fix ---

// --- Copilot round-51 fix ---

// --- Copilot round-52 fix ---

// --- Copilot round-56 fixes ---

func TestTitleHintHiddenWhenBufferGainedHistory(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.cursor = 0
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.scrollHintName = "devbox"
	app.scrollHintIdent = sessionIdentity(app.sessions[0])
	app.scrollHintMsg = "No scrollback available. Hit Enter to open the session and scrollback inside of it."
	app.scrollHintPane = "%1"
	app.scrollHintUntil = time.Now().Add(time.Minute)

	// The program starts streaming within the hint window.
	app.feedBuffer("devbox", "h1\nh2\nh3\nh4\nh5\nh6\nh7")
	app.feedBuffer("devbox", "h2\nh3\nh4\nh5\nh6\nh7\nh8")

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("main")
	require.NoError(t, err)
	assert.NotContains(t, v.Title, "Hit Enter", "the stale verdict must not reappear once history is available")
}

// --- Copilot round-57 fix ---

func TestFullscreenBarHidesNoScrollbackWhenHistoryAvailable(t *testing.T) {
	app := newTestApp(t, &fakeProvider{})
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g)) // settle geometry
	app.renderPreviewCapture("devbox", 0, app.sessionGen.Load(), app.captureSeq.Add(1), session.Preview{Content: "P", Full: "P", PaneID: "%1"}, nil)
	app.fullscreenNoScrollback = true
	app.fullscreenNoScrollbackPane = "%1"

	// The program starts streaming: the synthetic buffer gains history.
	app.feedBuffer("devbox", "h1\nh2\nh3\nh4\nh5\nh6\nh7")
	app.feedBuffer("devbox", "h2\nh3\nh4\nh5\nh6\nh7\nh8")

	require.NoError(t, app.layout(app.g))
	v, err := app.g.View("fullscreen-bar")
	require.NoError(t, err)
	assert.NotContains(t, v.Buffer(), "no scrollback", "the badge must not survive history becoming available")
}

// --- Copilot round-64 fixes ---

func TestWheelForwardFailureFallsBackToScrollMode(t *testing.T) {
	p := &fakeProvider{altOn: true, sgrMouse: true, paneID: "%5", wheelErr: assert.AnError}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	app.wheelHandlerAt(-3, 5, 5)
	assert.True(t, app.scroll.IsActive(), "a failed wheel injection must fall back to lazytmux scroll mode")
	assert.Equal(t, 3, app.scroll.offsetFromBottom, "the upward wheel still scrolls the snapshot")
}

// --- Copilot round-67 fix ---

func TestWheelPathDoesNotBlockOnSlowTmux(t *testing.T) {
	p := &fakeProvider{altOn: true, sgrMouse: true, paneID: "%5", flagsGate: make(chan struct{})}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox", ID: "$1", Created: 100}}
	app.fullscreen.Enter("devbox")
	require.NoError(t, app.layout(app.g))

	start := time.Now()
	app.wheelHandlerAt(-3, 5, 5)
	elapsed := time.Since(start)
	assert.True(t, app.scroll.IsActive(), "a timed-out query falls back to scroll mode")
	assert.Less(t, elapsed, time.Second, "the wheel path must not block for the full client timeout")
	close(p.flagsGate)
}
