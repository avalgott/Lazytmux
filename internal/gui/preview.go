package gui

import (
	"sync"
	"time"
)

// PreviewCache manages async capture of tmux pane content for the preview panel.
// All fields are guarded by mu; callers must acquire the lock before access.
type PreviewCache struct {
	mu      sync.Mutex
	content string    // last successfully captured pane content
	name    string    // session name the content was captured from
	gen     uint64    // session generation the content belongs to
	paneID  string    // the pane the content was captured from
	cursor  int       // session cursor index when content was captured
	cursorX int       // tmux pane cursor column
	cursorY int       // tmux pane cursor row
	busy    bool      // async capture in progress
	fetchAt time.Time // when content was last fetched (zero = needs refresh)
}

// Lock acquires the mutex. Caller must call Unlock when done.
func (pc *PreviewCache) Lock() { pc.mu.Lock() }

// Unlock releases the mutex.
func (pc *PreviewCache) Unlock() { pc.mu.Unlock() }

// Content returns the cached pane content. Caller must hold lock.
func (pc *PreviewCache) Content() string { return pc.content }

// Name returns the session name the cached content belongs to. Caller must
// hold lock.
func (pc *PreviewCache) Name() string { return pc.name }

// Gen returns the session generation the cached content belongs to. Caller
// must hold lock.
func (pc *PreviewCache) Gen() uint64 { return pc.gen }

// PaneID returns the pane the cached content came from. Caller must hold
// lock.
func (pc *PreviewCache) PaneID() string { return pc.paneID }

// Cursor returns the session cursor index at capture time. Caller must hold lock.
func (pc *PreviewCache) Cursor() int { return pc.cursor }

// CursorX returns the tmux pane cursor column. Caller must hold lock.
func (pc *PreviewCache) CursorX() int { return pc.cursorX }

// CursorY returns the tmux pane cursor row. Caller must hold lock.
func (pc *PreviewCache) CursorY() int { return pc.cursorY }

// Busy returns whether an async capture is in progress. Caller must hold lock.
func (pc *PreviewCache) Busy() bool { return pc.busy }

// Stale returns whether the cache is older than the given threshold. Caller must hold lock.
func (pc *PreviewCache) Stale(threshold time.Duration) bool {
	return time.Since(pc.fetchAt) > threshold
}

// SetBusy marks whether an async capture is in progress. Caller must hold lock.
func (pc *PreviewCache) SetBusy(b bool) { pc.busy = b }

// Update stores the result of a successful capture. Caller must hold lock.
func (pc *PreviewCache) Update(name, content string, gen uint64, paneID string, cursorIdx, cursorX, cursorY int) {
	pc.content = content
	pc.name = name
	pc.gen = gen
	pc.paneID = paneID
	pc.cursor = cursorIdx
	pc.cursorX = cursorX
	pc.cursorY = cursorY
	pc.busy = false
	pc.fetchAt = time.Now()
}

// Invalidate clears the cached content and resets the fetch timestamp, forcing
// the next render to request a fresh capture. Caller must NOT hold lock.
func (pc *PreviewCache) Invalidate() {
	pc.mu.Lock()
	pc.content = ""
	pc.fetchAt = time.Time{}
	pc.mu.Unlock()
}

// ClearContent drops the cached content (the session landscape changed and
// it belongs to a previous world). Caller must hold lock.
func (pc *PreviewCache) ClearContent() {
	pc.content = ""
}

// InvalidateTimestamp resets only the fetch timestamp so the next render
// triggers a new capture even if content is still present.
// Caller must hold lock.
func (pc *PreviewCache) InvalidateTimestamp() {
	pc.fetchAt = time.Time{}
}

// MarkFetched records the current time as the last fetch time and clears busy,
// without updating the cached content. Use after a fetch that returned no
// useful data to prevent tight retry loops. The session name is recorded so
// the fetch gate does not keep firing over the stale-name mismatch; content
// captured for a different session is dropped — retagging it would display
// one session's screen under another's name.
// Caller must hold lock.
func (pc *PreviewCache) MarkFetched(name string, gen uint64, cursorIdx int) {
	// A different name OR generation is a different session incarnation:
	// retagging its cached screen would display one pane's content under
	// another's. Same-incarnation refresh failures keep the cache.
	if pc.name != name || pc.gen != gen {
		pc.content = ""
	}
	pc.name = name
	pc.gen = gen
	pc.cursor = cursorIdx
	pc.busy = false
	pc.fetchAt = time.Now()
}
