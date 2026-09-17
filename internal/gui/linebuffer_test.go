package gui

import (
	"fmt"
	"strings"
	"sync"
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

func TestLineBufferInPlaceEditDoesNotGrow(t *testing.T) {
	b := NewLineBuffer(100)
	base := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09"}
	b.Feed(screen(base...))
	for i := 0; i < 20; i++ {
		scr := append([]string(nil), base...)
		scr[5] = "spin-" + string(rune('a'+i%26))
		b.Feed(screen(scr...))
	}
	assert.Len(t, b.Snapshot(), 10, "a spinner repaint must update the tail, not append")
	assert.Equal(t, "spin-t", b.Snapshot()[5])
}

func TestLineBufferSpinnerInTallScreen(t *testing.T) {
	n := 63
	base := make([]string, n)
	for i := range base {
		base[i] = "L" + string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	b := NewLineBuffer(400)
	b.Feed(screen(base...))
	for i := 0; i < 10; i++ {
		scr := append([]string(nil), base...)
		scr[40] = "spin-" + string(rune('a'+i%26))
		b.Feed(screen(scr...))
	}
	assert.Len(t, b.Snapshot(), n, "an edit mid-screen in a tall pane must not append")
}

func TestLineBufferFullRedrawReplacesTail(t *testing.T) {
	b := NewLineBuffer(100)
	base := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09"}
	b.Feed(screen(base...))
	redraw := []string{"R00", "R01", "R02", "R03", "R04", "R05", "R06", "R07", "R08", "R09"}
	b.Feed(screen(redraw...))
	assert.Equal(t, redraw, b.Snapshot(), "a completely different screen replaces the tail")
}

func TestLineBufferScrollDownPrepends(t *testing.T) {
	b := NewLineBuffer(100)
	cur := []string{"L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}
	b.Feed(screen(cur...))
	// The app scrolled back into its own history: one older row re-appears on top.
	cur = append([]string{"L04"}, cur[:9]...)
	b.Feed(screen(cur...))
	assert.Equal(t, []string{"L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}, b.Snapshot())
}

func TestLineBufferShorterFeedSeedsAndGrows(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("only"))
	assert.Equal(t, []string{"only"}, b.Snapshot())
	b.Feed(screen("only", "second"))
	assert.Equal(t, []string{"only", "second"}, b.Snapshot())
}

func TestLineBufferKeepsBlankLastRow(t *testing.T) {
	b := NewLineBuffer(100)
	// The cursor row is stripped; filling it in place appends the new row.
	b.Feed(screen("a", "b", ""))
	assert.Equal(t, []string{"a", "b"}, b.Snapshot(), "the cursor row is not part of the scrollback")

	b.Feed(screen("a", "b", "c"))
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot())
	b.Feed(screen("b", "c", "d"))
	assert.Equal(t, []string{"a", "b", "c", "d"}, b.Snapshot())
}

func TestLineBufferStripsTrailingBlankRows(t *testing.T) {
	b := NewLineBuffer(100)
	// A shell cursor row is always blank at the bottom; it never scrolls
	// with the content and must not break shift alignment.
	b.Feed(screen("a", "b", "c", ""))
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot(), "seed drops the cursor row")

	b.Feed(screen("b", "c", "d", ""))
	assert.Equal(t, []string{"a", "b", "c", "d"}, b.Snapshot(), "scroll-up aligns across the blank cursor row")

	b.Feed(screen("c", "d", "e", ""))
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, b.Snapshot())
}

func TestLineBufferRotationsDoNotDuplicate(t *testing.T) {
	b := NewLineBuffer(100)
	a := []string{"A00", "A01", "A02", "A03", "A04", "A05", "A06", "A07", "A08", "A09", "A10"}
	b.Feed(screen(a...))

	// Rotate right (an older line reappears at the top) then back to the
	// original: the buffer must hold each line exactly once.
	rot := append([]string{"A10"}, a[:10]...)
	b.Feed(screen(rot...))
	b.Feed(screen(a...))
	snap := b.Snapshot()
	assert.Equal(t, a, snap, "rotations must not duplicate or drop lines")

	// Rotate left the same way.
	b2 := NewLineBuffer(100)
	b2.Feed(screen(a...))
	rotL := append(append([]string(nil), a[1:]...), "A00")
	b2.Feed(screen(rotL...))
	b2.Feed(screen(a...))
	assert.Equal(t, a, b2.Snapshot(), "rotations must not duplicate or drop lines")
}

func TestLineBufferPaneResizeReplacesScreenRegion(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("h1", "h2", "h3", "s1", "s2", "s3")) // history + 3-row screen
	// The pane grows: the new 5-row screen supersedes the old one; the
	// history lines stay.
	b.Feed(screen("s1", "s2", "s3", "s4", "s5"))
	snap := b.Snapshot()
	assert.Equal(t, []string{"h1", "h2", "h3", "s1", "s2", "s3", "s4", "s5"}, snap,
		"a resize replaces the old screen region instead of appending a duplicate")
}

func TestLineBufferIdleFeedIsNoop(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c"))
	b.Feed(screen("a", "b", "c")) // byte-identical: nothing to do
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot())
}

func TestSnapshotWithHeightIsConsistentUnderFeeds(t *testing.T) {
	b := NewLineBuffer(400)
	b.Feed(screen("a", "b", "c"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				b.Feed(fmt.Sprintf("g%d-%d-a\ng%d-%d-b\ng%d-%d-c", i, j, i, j, i, j))
				snap, h := b.SnapshotWithHeight()
				if len(snap) < h {
					t.Errorf("inconsistent pair: %d lines with screen height %d", len(snap), h)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
