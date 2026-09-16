package gui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func screen(rows ...string) string { return strings.Join(rows, "\n") }

func TestLineBufferSeedsFromFirstFeed(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c"))
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot())
}

func TestLineBufferNoopOnIdenticalFeed(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c"))
	b.Feed(screen("a", "b", "c"))
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot())
}

func TestLineBufferAppendsScrolledInLines(t *testing.T) {
	b := NewLineBuffer(100)
	base := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09"}
	b.Feed(screen(base...))
	for i := 0; i < 3; i++ {
		base = append(append([]string(nil), base[1:]...), "out-0"+string(rune('0'+i)))
		b.Feed(screen(base...))
	}
	want := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09", "out-00", "out-01", "out-02"}
	assert.Equal(t, want, b.Snapshot())
}

func TestLineBufferAppendsMultiLineJumps(t *testing.T) {
	b := NewLineBuffer(100)
	base := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09"}
	b.Feed(screen(base...))
	base = append(append([]string(nil), base[3:]...), "jump-0", "jump-1", "jump-2")
	b.Feed(screen(base...))
	assert.Equal(t, []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09", "jump-0", "jump-1", "jump-2"}, b.Snapshot())
}

func TestLineBufferCapsAtLimit(t *testing.T) {
	b := NewLineBuffer(3)
	b.Feed(screen("a", "b", "c"))
	b.Feed(screen("b", "c", "d"))
	b.Feed(screen("c", "d", "e"))
	snap := b.Snapshot()
	assert.Len(t, snap, 3)
	assert.Equal(t, []string{"c", "d", "e"}, snap, "oldest lines evicted")
}

func TestLineBufferIgnoresEmptyFeed(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b"))
	b.Feed("")
	assert.Equal(t, []string{"a", "b"}, b.Snapshot())
}
