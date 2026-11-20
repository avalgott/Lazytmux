package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/jesseduffield/gocui"
	"github.com/stretchr/testify/require"

	"github.com/avalgott/Lazytmux/internal/session"
)

// pngClipboard is a minimal valid PNG payload; the tests assert the bytes
// travel unmodified to the provider.
var pngClipboard = []byte("\x89PNG\r\n\x1a\nclipboard fixture")

// runMainLoop drives app.g.MainLoop in a goroutine and returns the quit
// helper: it injects ErrQuit and waits for the loop to end.
func runMainLoop(t *testing.T, app *App) func() {
	t.Helper()
	// Build the views first: the first event must not race the initial
	// layout, or view-scoped bindings (Ctrl+G on "main") have nothing to
	// dispatch to and the event is lost.
	require.NoError(t, app.layout(app.g))
	done := make(chan error, 1)
	go func() { done <- app.g.MainLoop() }()
	return func() {
		t.Helper()
		app.g.Update(func(*gocui.Gui) error { return gocui.ErrQuit })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("MainLoop did not stop after quit")
		}
	}
}

func fullscreenApp(t *testing.T, p *fakeProvider) *App {
	t.Helper()
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0
	app.enterFullScreen()
	return app
}

// TestFullscreenCtrlGPasteImageClipboardRoundTrip drives the complete
// chain through the real event pipeline: injected Ctrl+G, the simulation
// screen's clipboard event, pollEvent, the event loop, and the provider.
func TestFullscreenCtrlGPasteImageClipboardRoundTrip(t *testing.T) {
	p := &fakeProvider{}
	app := fullscreenApp(t, p)
	quit := runMainLoop(t, app)
	// Deferred, not inline: a failed assertion must still stop the loop
	// before cleanup closes the screen under it.
	defer quit()

	sim, ok := gocui.Screen.(tcell.SimulationScreen)
	require.True(t, ok, "headless mode must use the tcell simulation screen")

	sim.InjectKey(tcell.KeyCtrlG, 0, tcell.ModNone)
	sim.SetClipboard(pngClipboard)
	sim.GetClipboard()

	require.Eventually(t, func() bool {
		return len(p.pasteImageSnapshot()) == 1
	}, 5*time.Second, 20*time.Millisecond)

	calls := p.pasteImageSnapshot()
	require.Equal(t, "devbox", calls[0].name)
	require.Equal(t, pngClipboard, calls[0].data, "clipboard bytes must travel unmodified")
}

// TestFullscreenPasteImageFallbackToPlatformTool proves the two-stage OSC
// wait times out (headless has no tty and no clipboard reply) and the
// platform tool path delivers the image instead.
func TestFullscreenPasteImageFallbackToPlatformTool(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "img.png")
	require.NoError(t, os.WriteFile(fixture, pngClipboard, 0o644))
	stub := filepath.Join(dir, "wl-paste")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\n/bin/cat '"+fixture+"'\n"), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	p := &fakeProvider{}
	app := fullscreenApp(t, p)
	app.pasteImageReadWait = 20 * time.Millisecond
	quit := runMainLoop(t, app)
	defer quit()

	sim, ok := gocui.Screen.(tcell.SimulationScreen)
	require.True(t, ok)
	sim.InjectKey(tcell.KeyCtrlG, 0, tcell.ModNone)
	// No clipboard event: both queries time out and the platform tool runs.

	require.Eventually(t, func() bool {
		return len(p.pasteImageSnapshot()) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, pngClipboard, p.pasteImageSnapshot()[0].data)
}

// TestFullscreenPasteImageIgnoresUnexpectedClipboard verifies a stray
// clipboard reply never types anything into a pane.
func TestFullscreenPasteImageIgnoresUnexpectedClipboard(t *testing.T) {
	p := &fakeProvider{}
	app := fullscreenApp(t, p)
	quit := runMainLoop(t, app)
	defer quit()

	sim, ok := gocui.Screen.(tcell.SimulationScreen)
	require.True(t, ok)
	sim.SetClipboard(pngClipboard)
	sim.GetClipboard()

	time.Sleep(100 * time.Millisecond)
	require.Empty(t, p.pasteImageSnapshot(), "a clipboard reply without a pending paste must be ignored")
}

// TestPasteImageHandlerGuards verifies the handler does nothing outside
// fullscreen or while browsing a scroll snapshot, and that a provider
// failure lands in the bar message.
func TestPasteImageHandlerGuards(t *testing.T) {
	p := &fakeProvider{}
	app := newTestApp(t, p)
	app.sessions = []session.Info{{Name: "devbox"}}
	app.cursor = 0

	// Not in fullscreen: nothing happens.
	require.NoError(t, app.pasteImageHandler(app.g, nil))
	require.False(t, app.pasteImageWaiting)
	require.Empty(t, p.pasteImageSnapshot())

	// In fullscreen but scroll mode active: nothing happens.
	app.enterFullScreen()
	app.scroll.Enter(10, 78)
	require.NoError(t, app.pasteImageHandler(app.g, nil))
	require.False(t, app.pasteImageWaiting)
	require.Empty(t, p.pasteImageSnapshot())
	app.scroll.Exit()

	// Provider error surfaces in the bar message. The full chain needs the
	// real event loop, so this part drives MainLoop and injects the key and
	// the clipboard reply.
	p.pasteImageErr = fmt.Errorf("boom")
	app.pasteImageReadWait = 5 * time.Millisecond
	quit := runMainLoop(t, app)
	defer quit()
	sim, ok := gocui.Screen.(tcell.SimulationScreen)
	require.True(t, ok)
	sim.InjectKey(tcell.KeyCtrlG, 0, tcell.ModNone)
	sim.SetClipboard(pngClipboard)
	sim.GetClipboard()

	// barMsg is event-loop state: poll it THROUGH the loop, never from the
	// test goroutine (the event loop writes it concurrently).
	getBarMsg := func() string {
		ch := make(chan string, 1)
		app.g.Update(func(*gocui.Gui) error { ch <- app.barMsg; return nil })
		select {
		case m := <-ch:
			return m
		case <-time.After(time.Second):
			return ""
		}
	}
	var satisfied string
	require.Eventually(t, func() bool {
		satisfied = getBarMsg()
		return satisfied != "" && satisfied != "reading clipboard image..."
	}, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, satisfied, "boom")
}

// TestFullscreenBarShowsPasteImageHint verifies the bar advertises the new
// key and that a transient message overrides the hints while active.
func TestFullscreenBarShowsPasteImageHint(t *testing.T) {
	p := &fakeProvider{}
	app := fullscreenApp(t, p)
	require.NoError(t, app.layout(app.g))

	v, err := app.g.View("fullscreen-bar")
	require.NoError(t, err)
	require.Contains(t, v.Buffer(), "img", "the bar must advertise ctrl+g")

	app.showBarMsg("transient status", time.Minute)
	require.NoError(t, app.layout(app.g))
	require.Contains(t, v.Buffer(), "transient status")
	require.NotContains(t, v.Buffer(), "img", "the transient message replaces the hints")
}
