package gocui

import (
	"bytes"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pngFixture is a minimal valid PNG header; the tests only care that the
// bytes travel unmodified.
var pngFixture = []byte("\x89PNG\r\n\x1a\nfakedata")

// TestPollEventClipboard drives a clipboard event through the real
// tcell-to-gocui conversion: the simulation screen stores clipboard bytes,
// GetClipboard posts the tcell event, and pollEvent must translate it into
// a binary-safe eventClipboard GocuiEvent.
func TestPollEventClipboard(t *testing.T) {
	g, err := NewGui(NewGuiOpts{OutputMode: OutputTrue, Headless: true})
	require.NoError(t, err)
	defer g.Close()

	sim, ok := g.screen.(tcell.SimulationScreen)
	require.True(t, ok, "headless mode must use the tcell simulation screen")

	sim.SetClipboard(pngFixture)
	sim.GetClipboard()

	ev := g.pollEvent()
	assert.Equal(t, eventClipboard, ev.Type)
	assert.True(t, bytes.Equal(pngFixture, ev.ClipboardData), "clipboard bytes must travel unmodified")
}

// TestHandleEventClipboard verifies the event reaches OnClipboard with the
// exact bytes, and that a nil callback is a safe no-op.
func TestHandleEventClipboard(t *testing.T) {
	var got []byte
	g := &Gui{OnClipboard: func(data []byte) error {
		got = data
		return nil
	}}
	err := g.handleEvent(&GocuiEvent{Type: eventClipboard, ClipboardData: pngFixture})
	require.NoError(t, err)
	assert.True(t, bytes.Equal(pngFixture, got))

	gNil := &Gui{}
	require.NoError(t, gNil.handleEvent(&GocuiEvent{Type: eventClipboard, ClipboardData: pngFixture}))
}

// TestTtyWriteHeadlessNoTty verifies TtyWrite reports the missing tty
// instead of panicking on the simulation screen.
func TestTtyWriteHeadlessNoTty(t *testing.T) {
	g, err := NewGui(NewGuiOpts{OutputMode: OutputTrue, Headless: true})
	require.NoError(t, err)
	defer g.Close()

	require.Error(t, g.TtyWrite([]byte("\x1b]52;c;?\x1b\\")))
}
