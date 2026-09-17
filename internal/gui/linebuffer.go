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
	lastRaw string // last raw capture; an idle pane feeds identical content
	screenH int    // normalized height of the current screen (blank rows stripped)
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
func (b *LineBuffer) update(scr []string) []string {
	// A shell's cursor row is always blank at the bottom and never scrolls
	// with the content — it is re-created each line. Strip it so the shift
	// alignment compares content only (blank rows carry no information).
	for len(scr) > 0 && scr[len(scr)-1] == "" {
		scr = scr[:len(scr)-1]
	}
	n := len(scr)
	b.screenH = n
	if len(b.lines) == 0 {
		return capLines(scr, b.cap)
	}
	tail := b.lines
	if len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	// Scroll-up: suffix of tail matches prefix of scr.
	if d, ok := shiftUp(tail, scr); ok {
		added := scr[n-d:]
		if majorityFresh(added, b.lines) {
			return capLines(append(b.lines, added...), b.cap)
		}
	}
	// Scroll-down: prefix of tail matches suffix of scr.
	if d, ok := shiftDown(tail, scr); ok {
		added := scr[:d]
		if majorityFresh(added, b.lines) {
			return capLines(append(append([]string(nil), added...), b.lines...), b.cap)
		}
	}
	// In-place edit or full redraw: replace the tail window.
	if len(b.lines) >= n {
		return capLines(append(append([]string(nil), b.lines[:len(b.lines)-n]...), scr...), b.cap)
	}
	// Buffer shorter than the screen: extend without duplicating the overlap.
	if m := len(b.lines); m > 0 && slices.Equal(b.lines, scr[:m]) {
		return capLines(append(b.lines, scr[m:]...), b.cap)
	}
	return capLines(append(b.lines, scr...), b.cap)
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
	// Clip the excess capacity: a plain re-slice would keep the discarded
	// string references alive in the backing array for the buffer's lifetime.
	return slices.Clip(lines[len(lines)-cap:])
}
