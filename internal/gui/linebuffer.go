package gui

import (
	"slices"
	"strings"
	"sync"
)

// LineBuffer accumulates observed pane content into a synthetic scrollback
// for panes that keep no tmux history (alternate-screen programs). Each Feed
// diffs the new screen against the tail: scrolled-in lines are appended,
// in-place repaints update the tail, full redraws replace it.
type LineBuffer struct {
	mu      sync.Mutex
	lines   []string
	cap     int
	lastRaw string   // last raw capture; an idle pane feeds identical content
	screenH int      // normalized height of the current screen (blank rows stripped)
	screen  []string // the last normalized screen — alignment diffs against this
}

// NewLineBuffer creates a buffer that keeps at most cap lines.
func NewLineBuffer(cap int) *LineBuffer {
	return &LineBuffer{cap: cap}
}

// Feed merges one full capture of the pane (raw content, "\n"-separated)
// into the buffer. Safe for concurrent use.
//
// The content comes from CapturePaneANSIWithCursor, which has no trailing
// newline — a trailing "\n" here is a real blank last row and must be kept,
// or the screen height wobbles and shift alignment never matches.
func (b *LineBuffer) Feed(content string) {
	if content == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if content == b.lastRaw {
		return // idle pane: nothing changed since the last capture
	}
	b.lastRaw = content
	scr := strings.Split(content, "\n")
	// Split returns substrings that alias the whole capture; cloning keeps
	// each retained line from pinning its entire capture in memory.
	for i, l := range scr {
		scr[i] = strings.Clone(l)
	}
	b.lines = b.update(scr)
}

// Snapshot returns a copy of the buffered lines, oldest first.
func (b *LineBuffer) Snapshot() []string {
	lines, _ := b.SnapshotWithHeight()
	return lines
}

// SnapshotWithHeight returns the buffered lines and the current screen's
// normalized height (the live portion of the buffer; lines beyond it are
// accumulated history) under one lock, so callers never pair a snapshot
// with a screen height from a different moment.
func (b *LineBuffer) SnapshotWithHeight() ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...), b.screenH
}

// update applies the diff-append algorithm. The caller holds the lock.
//
// Alignment diffs against the last normalized SCREEN (b.screen), not the
// buffer tail: reverse scrolling prepends rows, so the tail would drift
// away from the actual screen and misalign the next feed.
func (b *LineBuffer) update(scr []string) []string {
	// A shell's cursor row is always blank at the bottom and never scrolls
	// with the content — it is re-created each line. Strip it so the shift
	// alignment compares content only (blank rows carry no information).
	for len(scr) > 0 && scr[len(scr)-1] == "" {
		scr = scr[:len(scr)-1]
	}
	n := len(scr)
	prev := b.screenH
	b.screenH = n
	if len(b.lines) == 0 {
		b.screen = scr
		return capLines(scr, b.cap)
	}
	// An all-blank screen (e.g. a resize that only changes the number of
	// blank cursor rows) has nothing to replace the screen region with:
	// keep the accumulated history instead of erasing it.
	if n == 0 {
		return capLines(b.lines, b.cap)
	}
	prevScreen := b.screen
	if len(prevScreen) == 0 {
		prevScreen = b.lines
		if len(prevScreen) > n {
			prevScreen = prevScreen[len(prevScreen)-n:]
		}
	}
	b.screen = scr

	// Scroll-up: suffix of the previous screen matches the prefix of scr.
	if d, ok := shiftUp(prevScreen, scr); ok && d > 0 {
		added := scr[n-d:]
		// Accept a shift when most added lines are novel; when they repeat
		// recent output, still accept small blocks that do not replay the
		// dropped lines (rotations replay them), and larger blocks unless
		// they repaint the same screen region (a mid-screen edit masquerades
		// as a large shift with an all-known bottom block).
		if majorityFresh(added, b.lines) ||
			(!majorityEqual(added, prevScreen[:d]) &&
				(len(added) <= max(2, n/5) || !majorityEqual(added, prevScreen[n-d:]))) {
			return capLines(append(b.lines, added...), b.cap)
		}
	}
	// Scroll-down: prefix of the previous screen matches the suffix of scr.
	// Reverse scrolling re-reveals lines the buffer already holds. Repeated
	// text is NOT a global identity: the revealed block is reconciled
	// positionally against the buffer's head (a consecutive reverse step's
	// overlap), and a rotation — where the revealed block replays the rows
	// it dropped from the tail — keeps the buffer unchanged.
	if d, ok := shiftDown(prevScreen, scr); ok && d > 0 {
		added := scr[:d]
		if majorityFresh(added, b.lines) {
			return capLines(append(append([]string(nil), added...), b.lines...), b.cap)
		}
		if majorityEqual(added, prevScreen[len(prevScreen)-d:]) {
			return capLines(b.lines, b.cap)
		}
		k := len(added)
		if k > len(b.lines) {
			k = len(b.lines)
		}
		for k > 0 && !slices.Equal(added[len(added)-k:], b.lines[:k]) {
			k--
		}
		if missing := added[:len(added)-k]; len(missing) > 0 {
			return capLines(append(append([]string(nil), missing...), b.lines...), b.cap)
		}
		return capLines(b.lines, b.cap)
	}
	// In-place edit, full redraw, or a resized pane: replace the PREVIOUS
	// screen region (its height, not the new one — a shrunk pane must not
	// leave the old screen's tail behind as fake history).
	//
	// A size mismatch can also hide a genuine scroll (blank-line stripping
	// shrinks the screen; an interior blank grows it): when the old screen's
	// suffix overlaps the new screen's head, the transformation was a scroll
	// in disguise — append the new portion instead of replacing. Equal-height
	// redraws are handled by the shift checks above; an incidental one-row
	// overlap must not manufacture history, so this path requires a height
	// change.
	if n != prev && len(b.lines) >= prev {
		o := 0
		for k := 1; k <= len(prevScreen) && k <= len(scr); k++ {
			if slices.Equal(prevScreen[len(prevScreen)-k:], scr[:k]) {
				o = k
			}
		}
		if o > 0 {
			if o == len(scr) {
				// The new screen lies entirely inside the old one: a
				// re-framing of the same content — keep the buffer as is.
				return capLines(b.lines, b.cap)
			}
			// A scroll in disguise — unless the new portion replays the
			// dropped head (that is a rotation, handled by the replace).
			if !majorityEqual(scr[o:], prevScreen[:len(prevScreen)-o]) {
				return capLines(append(b.lines, scr[o:]...), b.cap)
			}
		}
	}
	if len(b.lines) >= prev {
		prefix := b.lines[:len(b.lines)-prev]
		// The largest suffix of the history prefix that equals the head of
		// the new screen is one shared region: drop the duplicated head.
		ov := 0
		for k := 1; k <= len(prefix) && k <= len(scr); k++ {
			if slices.Equal(prefix[len(prefix)-k:], scr[:k]) {
				ov = k
			}
		}
		return capLines(append(append([]string(nil), prefix...), scr[ov:]...), b.cap)
	}
	// The buffer holds less than the previous screen (a capped seed): only a
	// truncated tail is retained — reseed from the current screen.
	return capLines(scr, b.cap)
}

// shiftUp returns the scroll-up shift d (0 = identical) where
// tail[d:] == scr[:n-d] with overlap at least max(2, 60% of n).
func shiftUp(tail, scr []string) (int, bool) {
	n := len(scr)
	if len(tail) < n {
		return 0, false // a shorter tail cannot align with the whole screen
	}
	for d := 0; d < n; d++ {
		ov := n - d
		if ov < max(2, n*3/5) {
			return 0, false
		}
		if slices.Equal(tail[d:], scr[:ov]) {
			return d, true
		}
	}
	return 0, false
}

// shiftDown mirrors shiftUp: tail[:n-d] == scr[d:].
func shiftDown(tail, scr []string) (int, bool) {
	n := len(scr)
	if len(tail) < n {
		return 0, false // a shorter tail cannot align with the whole screen
	}
	for d := 0; d < n; d++ {
		ov := n - d
		if ov < max(2, n*3/5) {
			return 0, false
		}
		if slices.Equal(tail[:ov], scr[d:]) {
			return d, true
		}
	}
	return 0, false
}

// majorityEqual reports whether more than half of the candidate lines equal
// their counterparts in other — a rotation replays the lines it dropped.
func majorityEqual(a, b []string) bool {
	if len(a) == 0 || len(b) < len(a) {
		return false
	}
	eq := 0
	for i := range a {
		if a[i] == b[i] {
			eq++
		}
	}
	return eq*2 > len(a)
}

// majorityFresh reports whether more than half of the candidate lines have
// not been seen in the buffer's recent history — genuine scrolling brings
// new content, in-place edits bring mostly known lines.
func majorityFresh(cands, buf []string) bool {
	if len(cands) == 0 {
		return false
	}
	recent := len(buf) - 300
	if recent < 0 {
		recent = 0
	}
	seen := make(map[string]bool, 300)
	for _, l := range buf[recent:] {
		seen[l] = true
	}
	fresh := 0
	for _, l := range cands {
		if !seen[l] {
			fresh++
		}
	}
	return fresh*2 > len(cands)
}

func capLines(lines []string, cap int) []string {
	if len(lines) <= cap {
		return lines
	}
	// Copy the retained tail into a fresh array: Clip alone only trims the
	// slice's capacity and would keep the discarded backing array (and its
	// string references) alive until the next reallocation.
	out := make([]string, cap)
	copy(out, lines[len(lines)-cap:])
	return out
}
