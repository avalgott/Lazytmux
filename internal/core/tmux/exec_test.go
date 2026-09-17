package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseClients(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []ClientInfo
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "single client",
			in:   "/dev/ttys001\tmain\t200\t50\t1710000000",
			want: []ClientInfo{
				{Name: "/dev/ttys001", Session: "main", Width: 200, Height: 50, Activity: 1710000000},
			},
		},
		{
			name: "multiple clients",
			in:   "/dev/ttys001\tmain\t200\t50\t100\n/dev/ttys002\tclaude\t180\t40\t200",
			want: []ClientInfo{
				{Name: "/dev/ttys001", Session: "main", Width: 200, Height: 50, Activity: 100},
				{Name: "/dev/ttys002", Session: "claude", Width: 180, Height: 40, Activity: 200},
			},
		},
		{
			name: "malformed line",
			in:   "bad\tdata",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseClients(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []WindowInfo
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "single window",
			in:   "@1\t0\tlc-abc12345\tclaude\t1",
			want: []WindowInfo{
				{ID: "@1", Index: 0, Name: "lc-abc12345", Session: "claude", Active: true},
			},
		},
		{
			name: "multiple windows",
			in:   "@1\t0\tlc-abc\tclaude\t1\n@2\t1\tlc-def\tclaude\t0",
			want: []WindowInfo{
				{ID: "@1", Index: 0, Name: "lc-abc", Session: "claude", Active: true},
				{ID: "@2", Index: 1, Name: "lc-def", Session: "claude", Active: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseWindows(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePanes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []PaneInfo
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "alive pane",
			in:   "%1\t@1\t12345\t0",
			want: []PaneInfo{
				{ID: "%1", Window: "@1", PID: 12345, Dead: false},
			},
		},
		{
			name: "dead pane",
			in:   "%2\t@1\t0\t1",
			want: []PaneInfo{
				{ID: "%2", Window: "@1", PID: 0, Dead: true},
			},
		},
		{
			name: "multiple panes",
			in:   "%1\t@1\t1001\t0\n%2\t@2\t1002\t0\n%3\t@2\t0\t1",
			want: []PaneInfo{
				{ID: "%1", Window: "@1", PID: 1001, Dead: false},
				{ID: "%2", Window: "@2", PID: 1002, Dead: false},
				{ID: "%3", Window: "@2", PID: 0, Dead: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePanes(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Verify ExecClient implements Client interface at compile time.
var _ Client = (*ExecClient)(nil)

func TestNewExecClient(t *testing.T) {
	t.Parallel()
	c := NewExecClient()
	require.NotNil(t, c)
	assert.Equal(t, "", c.Socket())
}

func TestNewExecClientWithSocket(t *testing.T) {
	t.Parallel()
	c := NewExecClientWithSocket("lazyclaude")
	require.NotNil(t, c)
	assert.Equal(t, "lazyclaude", c.Socket())
}

func TestPrependSocket(t *testing.T) {
	t.Parallel()

	t.Run("no socket", func(t *testing.T) {
		c := NewExecClient()
		args := c.prependSocket([]string{"list-sessions"})
		assert.Equal(t, []string{"-u", "list-sessions"}, args)
	})

	t.Run("with socket", func(t *testing.T) {
		c := NewExecClientWithSocket("lc")
		args := c.prependSocket([]string{"list-sessions"})
		assert.Equal(t, []string{"-u", "-L", "lc", "list-sessions"}, args)
	})
}

func TestCaptureHistoryPreservesBlankLines(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-tmux")
	// Prints two blank lines, "x", two blank lines, then the pane-height line
	// that the display-message half of the combined command produces. The
	// leading and trailing blanks are significant for the snapshot's line
	// accounting.
	script := "#!/bin/sh\nfor f in \"$@\"; do :; done\necho; echo; echo x; echo; echo; echo 63\n"
	require.NoError(t, os.WriteFile(fake, []byte(script), 0o755))

	c := &ExecClient{tmuxBin: fake}

	content, paneH, err := c.CapturePaneANSIHistory(context.Background(), "s")
	require.NoError(t, err)
	assert.Equal(t, "\n\nx\n\n\n", content, "history captures must preserve blank lines")
	assert.Equal(t, 63, paneH, "the trailing pane-height line is parsed separately")

	trimmed, err := c.run(context.Background(), "display-message", "-p", "x")
	require.NoError(t, err)
	assert.Equal(t, "x\n\n\n63", trimmed, "run() trims the fixture's surrounding whitespace")
}

func TestSplitPaneHeightLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		content string
		height  int
		wantErr bool
	}{
		{
			name:    "content with trailing height line",
			in:      "line1\nline2\n63\n",
			content: "line1\nline2\n",
			height:  63,
		},
		{
			name:    "blank lines preserved",
			in:      "\n\nx\n\n\n63\n",
			content: "\n\nx\n\n\n",
			height:  63,
		},
		{
			name:    "no height line",
			in:      "only content\n",
			wantErr: true,
		},
		{
			name:    "non-numeric height",
			in:      "content\nabc\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, h, err := splitPaneHeightLine(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.content, content)
			assert.Equal(t, tt.height, h)
		})
	}
}

func TestParseInputFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		alt     bool
		mouse   bool
		cx, cy  int
		wantErr bool
	}{
		{name: "alt screen with SGR mouse", in: "1 1 1 12 34", alt: true, mouse: true, cx: 12, cy: 34},
		{name: "plain pane", in: "0 0 0 0 0", alt: false, mouse: false, cx: 0, cy: 0},
		{name: "alt without any mouse", in: "1 0 0 5 9", alt: true, mouse: false, cx: 5, cy: 9},
		{name: "SGR encoding without tracking", in: "1 0 1 5 9", alt: true, mouse: false, cx: 5, cy: 9},
		{name: "tracking without SGR encoding", in: "1 1 0 5 9", alt: true, mouse: false, cx: 5, cy: 9},
		{name: "empty", in: "", wantErr: true},
		{name: "too few fields", in: "1 1", wantErr: true},
		{name: "non-numeric", in: "x 1 2 3", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alt, mouse, cx, cy, err := parseInputFlags(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.alt, alt)
			assert.Equal(t, tt.mouse, mouse)
			assert.Equal(t, tt.cx, cx)
			assert.Equal(t, tt.cy, cy)
		})
	}
}

func TestSGRWheel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		up   bool
		x, y int
		want string
	}{
		{
			name: "wheel up at 0-based coords",
			up:   true,
			x:    10, y: 5,
			want: "\x1b[<64;11;6M",
		},
		{
			name: "wheel down at origin",
			up:   false,
			x:    0, y: 0,
			want: "\x1b[<65;1;1M",
		},
		{
			name: "negative coords clamp to one",
			up:   true,
			x:    -5, y: -3,
			want: "\x1b[<64;1;1M",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sgrWheel(tt.up, tt.x, tt.y))
		})
	}
}

func TestSplitCursorPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		content string
		cx, cy  int
	}{
		{
			name:    "full screen without blank row",
			in:      "r1\nr2\nr3\n10,14\n",
			content: "r1\nr2\nr3",
			cx:      10, cy: 14,
		},
		{
			name:    "blank last row preserved",
			in:      "r1\nr2\n\n10,14\n",
			content: "r1\nr2\n",
			cx:      10, cy: 14,
		},
		{
			name:    "no cursor line",
			in:      "r1\nr2\n",
			content: "r1\nr2",
			cx:      0, cy: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, cx, cy := splitCursorPair(tt.in)
			assert.Equal(t, tt.content, content)
			assert.Equal(t, tt.cx, cx)
			assert.Equal(t, tt.cy, cy)
		})
	}
}
