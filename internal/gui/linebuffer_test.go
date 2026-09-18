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
	// original: the buffer must hold each line exactly once. Reverse
	// scrolling preserves the accumulated buffer, so the completion keeps
	// the rotated order — the invariant is set membership, not order.
	rot := append([]string{"A10"}, a[:10]...)
	b.Feed(screen(rot...))
	b.Feed(screen(a...))
	snap := b.Snapshot()
	assert.Len(t, snap, len(a), "rotations must not duplicate or drop lines")
	seen := map[string]bool{}
	for _, l := range snap {
		assert.False(t, seen[l], "rotation duplicated %q", l)
		seen[l] = true
	}

	// Rotate left the same way.
	b2 := NewLineBuffer(100)
	b2.Feed(screen(a...))
	rotL := append(append([]string(nil), a[1:]...), "A00")
	b2.Feed(screen(rotL...))
	b2.Feed(screen(a...))
	assert.Len(t, b2.Snapshot(), len(a), "rotations must not duplicate or drop lines")
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

func TestLineBufferShrinkReplacesOldScreen(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c", "d"))
	// The pane shrinks to two rows: the old 4-row screen region is replaced,
	// not tail-joined into a duplicate.
	b.Feed(screen("a", "b"))
	assert.Equal(t, []string{"a", "b"}, b.Snapshot(), "shrinkage must not manufacture duplicate history")

	// A shrink that reframes the same content keeps the buffer intact —
	// the scrolled-off rows are history now, not garbage.
	b2 := NewLineBuffer(100)
	b2.Feed(screen("s1", "s2", "s3", "s4"))
	b2.Feed(screen("s2", "s3", "s4", "s5"))
	b2.Feed(screen("s4", "s5"))
	assert.Equal(t, []string{"s1", "s2", "s3", "s4", "s5"}, b2.Snapshot())
}

func TestLineBufferRepeatedLineScrollAccumulates(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c"))
	// Genuine scroll with a repeated line: the shift is valid and must
	// accumulate history even though the new line is not novel.
	b.Feed(screen("b", "c", "c"))
	assert.Equal(t, []string{"a", "b", "c", "c"}, b.Snapshot())
}

func TestLineBufferCappedSeedThenShrinkReseeds(t *testing.T) {
	b := NewLineBuffer(3)
	b.Feed(screen("a", "b", "c", "d", "e")) // capped to 3, screen height 5
	b.Feed(screen("x", "y"))
	assert.Equal(t, []string{"x", "y"}, b.Snapshot(), "an unmatched shrink after a capped seed must reseed, not slice")
}

func TestLineBufferRepeatedMultiRowScrollAccumulates(t *testing.T) {
	b := NewLineBuffer(100)
	base := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	b.Feed(screen(base...))
	// A 3-row scroll whose incoming rows are repeated log lines: the shift
	// is genuine and the three rows that scrolled off must accumulate.
	b.Feed(screen("d", "e", "f", "g", "h", "i", "j", "L", "L", "L"))
	want := append(append([]string(nil), base...), "L", "L", "L")
	assert.Equal(t, want, b.Snapshot())
}

func TestLineBufferReverseScrollDoesNotDuplicate(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	// Reverse scroll back up: the re-revealed L04 is already known — it must
	// not be prepended a second time, and the accumulated newer row L14 must
	// survive (the screen moved within known content).
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	snap := b.Snapshot()
	assert.Equal(t, []string{"L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}, snap,
		"reverse scrolling must not duplicate re-revealed lines or drop newer history")
}

func TestLineBufferReverseScrollMultiRowOverlap(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c", "d", "e"))
	b.Feed(screen("b", "c", "d", "e", "f"))
	b.Feed(screen("c", "d", "e", "f", "g"))
	// Reverse scroll by two: the re-revealed head overlaps the history
	// prefix on BOTH rows and must not duplicate either; the accumulated
	// newer rows survive.
	b.Feed(screen("a", "b", "c", "d", "e"))
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f", "g"}, b.Snapshot(),
		"reverse scrolling keeps the accumulated history without duplicates")
}

func TestLineBufferBlankLineScrollKeepsHistory(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c"))
	// A printed blank line scrolls "a" off while the screen shrinks a row:
	// the scrolled-off line must stay in history.
	b.Feed(screen("b", "c"))
	assert.Equal(t, []string{"a", "b", "c"}, b.Snapshot(), "a scroll hidden by blank-line stripping must not lose history")

	b2 := NewLineBuffer(100)
	b2.Feed(screen("a", "b", "c"))
	// An interior blank row grows the screen mid-scroll: the history line
	// still survives.
	b2.Feed(screen("b", "c", "", "d"))
	assert.Equal(t, []string{"a", "b", "c", "", "d"}, b2.Snapshot())
}

func TestLineBufferConsecutiveReverseScrollsRetainAll(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	b.Feed(screen("L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12"))
	snap := b.Snapshot()
	want := []string{"L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}
	assert.Equal(t, want, snap, "every newly revealed older row must be retained across consecutive reverse scrolls")
}

func TestLineBufferEqualHeightRedrawNotHistory(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("a", "b", "c", "d"))
	// An equal-height redraw that incidentally overlaps one row (the tail's
	// "d" equals the new head's "d") is a redraw, not a scroll.
	b.Feed(screen("d", "x", "y", "z"))
	assert.Equal(t, []string{"d", "x", "y", "z"}, b.Snapshot(),
		"an equal-height redraw must replace the screen, not manufacture history")
}

func TestLineBufferBlankScreenKeepsHistory(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L01", "L02", "L03"))
	b.Feed(screen("L02", "L03", "L04"))
	// The screen becomes entirely blank (cursor rows only, possibly resized):
	// the accumulated history must survive.
	b.Feed(screen("", ""))
	b.Feed(screen("", "", ""))
	assert.Equal(t, []string{"L01", "L02", "L03", "L04"}, b.Snapshot(),
		"all-blank screens must not erase retained history")
}

func TestLineBufferRepeatedLineReverseScroll(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("A", "X", "B", "C", "D"))
	// A valid one-row reverse shift whose revealed row repeats existing
	// text: the new X is a DISTINCT row and must be retained.
	b.Feed(screen("X", "A", "X", "B", "C"))
	assert.Equal(t, []string{"X", "A", "X", "B", "C", "D"}, b.Snapshot(),
		"a repeated line revealed by reverse scrolling is a distinct row")
}

func TestLineBufferReverseThenForwardKeepsHistory(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	snap := b.Snapshot()
	assert.Equal(t, []string{"L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}, snap,
		"reverse-then-forward must neither drop nor duplicate rows")
}

func TestLineBufferReverseReverseForwardKeepsHistory(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	b.Feed(screen("L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12"))
	b.Feed(screen("L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13"))
	snap := b.Snapshot()
	assert.Equal(t, []string{"L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}, snap,
		"intermediate forward steps must not duplicate rows")
}

func TestLineBufferHalfScreenJumpKeepsAllRows(t *testing.T) {
	b := NewLineBuffer(100)
	b.Feed(screen("L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09"))
	// A burst scrolls the pane by half a screen between captures.
	b.Feed(screen("L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"))
	want := []string{"L00", "L01", "L02", "L03", "L04", "L05", "L06", "L07", "L08", "L09", "L10", "L11", "L12", "L13", "L14"}
	assert.Equal(t, want, b.Snapshot(), "a half-screen jump must retain the scrolled-off rows")
}
