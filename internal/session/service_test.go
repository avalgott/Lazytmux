package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/avalgott/Lazytmux/internal/core/tmux"
)

func TestServiceList(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Infos["devbox"] = tmux.SessionInfo{Name: "devbox", Path: "/home/u", Attached: true, Windows: 2}
	mock.Infos["logs"] = tmux.SessionInfo{Name: "logs", Path: "/var/log", Attached: false, Windows: 1}
	mock.Infos["Bugs"] = tmux.SessionInfo{Name: "Bugs", Path: "/tmp", Attached: false, Windows: 1}

	svc := NewService(mock)
	infos, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, infos, 3)

	// Sorted by name, case-insensitively, so indexes stay stable.
	names := []string{infos[0].Name, infos[1].Name, infos[2].Name}
	assert.Equal(t, []string{"Bugs", "devbox", "logs"}, names)

	assert.Equal(t, Info{Name: "devbox", Path: "/home/u", Attached: true, Windows: 2}, infos[1])
	assert.Equal(t, Info{Name: "logs", Path: "/var/log", Windows: 1}, infos[2])
}

func TestServiceListError(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.ErrListSessions = assert.AnError

	svc := NewService(mock)
	_, err := svc.List(context.Background())
	assert.ErrorIs(t, err, assert.AnError)
}

func TestServiceListNoServerIsEmpty(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.ErrListSessions = errors.New("tmux -u list-sessions: exit status 1 (stderr: no server running on /tmp/tmux-1000/default)")

	svc := NewService(mock)
	infos, err := svc.List(context.Background())
	require.NoError(t, err, "a missing server means zero sessions, not an error")
	assert.Empty(t, infos)
}

func TestServiceCreate(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	err := svc.Create(context.Background(), CreateOpts{
		Name:    "devbox",
		Dir:     "/home/u/projects",
		Command: "ssh devbox",
	})
	require.NoError(t, err)

	opts := mock.LastNewSessionOpts
	assert.Equal(t, "devbox", opts.Name)
	assert.Equal(t, "/home/u/projects", opts.StartDir)
	assert.True(t, opts.Detached)

	// The command runs inside an interactive shell wrapper so Ctrl+C cannot
	// kill the pane (and with it the session). The user's command lives in a
	// self-deleting temp script under /tmp.
	assert.Contains(t, opts.Command, `exec "$SHELL" -lic '. /tmp/lazytmux-cmd-`)
	assert.Contains(t, opts.Command, `; exec "$SHELL"'`)
	assert.Equal(t, "/bin/bash", opts.Env["SHELL"], "the resolved shell is pinned into the session environment")

	script := strings.TrimSuffix(strings.TrimPrefix(opts.Command, `exec "$SHELL" -lic '. `), `; exec "$SHELL"'`)
	assert.True(t, strings.HasPrefix(script, "/tmp/lazytmux-cmd-"), "script must live directly under /tmp, not TMPDIR")

	// The mock never runs the session, so the script's self-delete line never
	// fires — remove it at test end.
	t.Cleanup(func() { _ = os.Remove(script) })

	data, err := os.ReadFile(script)
	require.NoError(t, err, "temp script should exist")
	assert.Contains(t, string(data), "rm -f '"+script+"'", "script should self-delete")
	assert.Contains(t, string(data), "\nssh devbox\n")
}

func TestServiceCreateUsesFishSourceKeyword(t *testing.T) {
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	t.Setenv("SHELL", "/usr/bin/fish")
	err := svc.Create(context.Background(), CreateOpts{Name: "x", Command: "top"})
	require.NoError(t, err)

	opts := mock.LastNewSessionOpts
	assert.Contains(t, opts.Command, `-lic 'source /tmp/lazytmux-cmd-`, "fish uses `source`, not `.`")
	assert.Equal(t, "/usr/bin/fish", opts.Env["SHELL"], "the resolved fish path is pinned into the session environment")
	t.Cleanup(func() {
		script := strings.TrimSuffix(strings.TrimPrefix(opts.Command, `exec "$SHELL" -lic 'source `), `; exec "$SHELL"'`)
		_ = os.Remove(script)
	})
}

func TestServiceCreateRejectsUnsupportedShell(t *testing.T) {
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	// Snapshot the matching file set before the call, so unrelated
	// concurrently-running instances cannot make the cleanup check flaky.
	before, globErr := filepath.Glob("/tmp/lazytmux-cmd-*")
	require.NoError(t, globErr)

	t.Setenv("SHELL", "/bin/csh")
	err := svc.Create(context.Background(), CreateOpts{Name: "x", Command: "top"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")

	// No session was created and the temp script was cleaned up.
	assert.Empty(t, mock.Infos)
	after, globErr := filepath.Glob("/tmp/lazytmux-cmd-*")
	require.NoError(t, globErr)
	assert.Equal(t, before, after, "the rejected shell must not leave scripts behind")
}

func TestServiceCreateIgnoresTMPDIR(t *testing.T) {
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	// A hostile TMPDIR must not affect the wrapper (the script always goes
	// to /tmp, and the path is interpolated unquoted into single quotes).
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("TMPDIR", "/tmp with spaces; rm -rf")
	err := svc.Create(context.Background(), CreateOpts{Name: "x", Command: "top"})
	require.NoError(t, err)

	opts := mock.LastNewSessionOpts
	assert.Contains(t, opts.Command, ". /tmp/lazytmux-cmd-")
	assert.NotContains(t, opts.Command, "with spaces")
	t.Cleanup(func() {
		script := strings.TrimSuffix(strings.TrimPrefix(opts.Command, `exec "$SHELL" -lic '. `), `; exec "$SHELL"'`)
		_ = os.Remove(script)
	})
}

func TestServiceCreateCleansUpScriptOnFailure(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	mock := tmux.NewMockClient()
	mock.ErrNewSession = assert.AnError
	svc := NewService(mock)

	// Snapshot the matching file set before the call, so unrelated
	// concurrently-running instances cannot make the cleanup check flaky.
	before, globErr := filepath.Glob("/tmp/lazytmux-cmd-*")
	require.NoError(t, globErr)

	err := svc.Create(context.Background(), CreateOpts{Name: "x", Command: "top"})
	assert.ErrorIs(t, err, assert.AnError)

	// The temp script must be removed when the session could not be created.
	after, globErr := filepath.Glob("/tmp/lazytmux-cmd-*")
	require.NoError(t, globErr)
	assert.Equal(t, before, after, "no leftover command scripts")
}

func TestServiceCreateShellSession(t *testing.T) {
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	// Empty command and directory: the session runs the user's normal shell
	// in tmux's default directory.
	err := svc.Create(context.Background(), CreateOpts{Name: "shell"})
	require.NoError(t, err)

	opts := mock.LastNewSessionOpts
	assert.Equal(t, "", opts.Command)
	assert.Equal(t, "", opts.StartDir)
}

func TestServiceKill(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Infos["devbox"] = tmux.SessionInfo{Name: "devbox"}
	svc := NewService(mock)

	require.NoError(t, svc.Kill(context.Background(), "devbox"))
	_, ok := mock.Infos["devbox"]
	assert.False(t, ok)
}

func TestServiceRename(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Infos["devbox"] = tmux.SessionInfo{Name: "devbox"}
	svc := NewService(mock)

	require.NoError(t, svc.Rename(context.Background(), "devbox", "devbox2"))
	_, ok := mock.Infos["devbox"]
	assert.False(t, ok)
	renamed, ok := mock.Infos["devbox2"]
	assert.True(t, ok)
	assert.Equal(t, "devbox2", renamed.Name)
}

func TestServiceCapture(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Captured["devbox"] = strings.Join([]string{
		"$ ssh devbox",
		"Last login: Fri Sep 12 09:00:00 2026 from 10.0.0.1",
		"$ ",
	}, "\n")
	mock.Messages["devbox"] = "2,7"
	svc := NewService(mock)

	preview, err := svc.Capture(context.Background(), "devbox", 100, 30)
	require.NoError(t, err)
	assert.Equal(t, "$ ssh devbox\nLast login: Fri Sep 12 09:00:00 2026 from 10.0.0.1\n$ ", preview.Content)
	assert.Equal(t, 2, preview.CursorX)
	assert.Equal(t, 7, preview.CursorY)
}

func TestServiceCaptureCrops(t *testing.T) {
	mock := tmux.NewMockClient()
	longLine := strings.Repeat("x", 200)
	mock.Captured["logs"] = longLine + "\n" + "line2\n" + "line3"
	svc := NewService(mock)

	preview, err := svc.Capture(context.Background(), "logs", 80, 2)
	require.NoError(t, err)
	lines := strings.Split(preview.Content, "\n")
	assert.Len(t, lines, 2, "capture should be cropped to 2 lines")
	assert.Len(t, lines[0], 80, "first line should be truncated to 80 columns")
	assert.Equal(t, "line2", lines[1])
}

func TestServiceCaptureAnchorsToCursor(t *testing.T) {
	mock := tmux.NewMockClient()
	// 40 rows of screen; the shell prompt sits on the last row, below a
	// full-screen program's remains (rows 0-38) — like after `top` exits
	// without restoring the screen.
	rows := make([]string, 40)
	for i := range rows {
		rows[i] = fmt.Sprintf("row-%02d", i)
	}
	mock.Captured["devbox"] = strings.Join(rows, "\n")
	mock.Messages["devbox"] = "0,39"
	svc := NewService(mock)

	preview, err := svc.Capture(context.Background(), "devbox", 80, 36)
	require.NoError(t, err)
	lines := strings.Split(preview.Content, "\n")
	require.Len(t, lines, 36)
	// The window is anchored so the cursor row is visible at the bottom.
	assert.Equal(t, "row-04", lines[0])
	assert.Equal(t, "row-39", lines[35])
	assert.Equal(t, 35, preview.CursorY, "cursor must be remapped into the cropped window")
}

func TestServiceCaptureTopAnchoredWhenCursorFits(t *testing.T) {
	mock := tmux.NewMockClient()
	rows := make([]string, 40)
	for i := range rows {
		rows[i] = fmt.Sprintf("row-%02d", i)
	}
	mock.Captured["devbox"] = strings.Join(rows, "\n")
	mock.Messages["devbox"] = "0,2" // fresh shell prompt near the top
	svc := NewService(mock)

	preview, err := svc.Capture(context.Background(), "devbox", 80, 36)
	require.NoError(t, err)
	lines := strings.Split(preview.Content, "\n")
	assert.Equal(t, "row-00", lines[0])
	assert.Equal(t, 2, preview.CursorY)
}

func TestServiceCaptureMissingCursor(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Captured["devbox"] = "hello"
	// No Messages entry: ShowMessage returns empty string -> cursor stays 0,0.
	svc := NewService(mock)

	preview, err := svc.Capture(context.Background(), "devbox", 40, 10)
	require.NoError(t, err)
	assert.Equal(t, "hello", preview.Content)
	assert.Equal(t, 0, preview.CursorX)
	assert.Equal(t, 0, preview.CursorY)
}

func TestServiceSendKeys(t *testing.T) {
	mock := tmux.NewMockClient()
	svc := NewService(mock)

	require.NoError(t, svc.SendKeys(context.Background(), "devbox", "Enter", "Up"))
	require.NoError(t, svc.SendLiteral(context.Background(), "devbox", "hello"))
	require.NoError(t, svc.Paste(context.Background(), "devbox", "line1\nline2"))

	assert.Equal(t, []string{"Enter", "Up", "hello", "line1\nline2"}, mock.SentKeys["devbox"])
}

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"devbox", false},
		{"my session", false},
		{"", true},
		{"   ", true},
		{"a:b", true},
		{"a.b", true},
		{"a;rm", true},
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		if c.wantErr {
			assert.Error(t, err, "name %q should be rejected", c.name)
		} else {
			assert.NoError(t, err, "name %q should be accepted", c.name)
		}
	}
}

// TestShellWrapperExecutesUnderPOSIX runs the generated wrapper in a real
// /bin/sh: the user command must run and a fresh shell must take over.
func TestShellWrapperExecutesUnderPOSIX(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")

	dir := t.TempDir()
	script := filepath.Join(dir, "cmd.sh")
	require.NoError(t, os.WriteFile(script, []byte("echo WRAPPER-RAN\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sink"), []byte(""), 0o600))

	wrapper, sessionEnv, err := buildShellWrapper(script)
	require.NoError(t, err)
	assert.Equal(t, "/bin/sh", sessionEnv["SHELL"])

	// Run exactly as tmux would: sh -c '<wrapper>' with stdin from a file
	// and the pinned SHELL in the environment.
	cmd := exec.Command("sh", "-c", wrapper)
	cmd.Env = append(os.Environ(), "SHELL="+sessionEnv["SHELL"])
	cmd.Stdin = strings.NewReader("echo SHELL-ALIVE; exit\n")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "wrapper output: %s", out)
	assert.Contains(t, string(out), "WRAPPER-RAN")
	assert.Contains(t, string(out), "SHELL-ALIVE", "the relaunched shell must run")
}

// TestShellWrapperExecutesUnderFish runs the generated wrapper in a real
// fish shell when one is installed (the wrapper template is fish-specific).
func TestShellWrapperExecutesUnderFish(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}
	t.Setenv("SHELL", fish)

	dir := t.TempDir()
	script := filepath.Join(dir, "cmd.sh")
	require.NoError(t, os.WriteFile(script, []byte("echo WRAPPER-RAN\n"), 0o700))

	wrapper, sessionEnv, err := buildShellWrapper(script)
	require.NoError(t, err)
	assert.Equal(t, fish, sessionEnv["SHELL"])

	cmd := exec.Command("sh", "-c", wrapper)
	cmd.Env = append(os.Environ(), "SHELL="+sessionEnv["SHELL"])
	cmd.Stdin = strings.NewReader("echo SHELL-ALIVE; exit\n")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "wrapper output: %s", out)
	assert.Contains(t, string(out), "WRAPPER-RAN")
	assert.Contains(t, string(out), "SHELL-ALIVE", "the relaunched fish shell must run")
}

func TestServiceCaptureScrollbackSetsPaneHeight(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Captured["devbox"] = "line1\nline2\n"
	mock.PaneHeight = 42

	svc := NewService(mock)
	preview, err := svc.CaptureScrollback(context.Background(), "devbox")
	require.NoError(t, err)
	assert.Equal(t, "line1\nline2\n", preview.Content)
	assert.Equal(t, 42, preview.PaneHeight, "the pane height rides along with the snapshot")
}

func TestServicePaneInputFlags(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Messages["devbox#flags"] = "1 1 1 12 34"

	svc := NewService(mock)
	alt, mouse, cx, cy, err := svc.PaneInputFlags(context.Background(), "devbox")
	require.NoError(t, err)
	assert.True(t, alt)
	assert.True(t, mouse)
	assert.Equal(t, 12, cx)
	assert.Equal(t, 34, cy)
}

func TestServiceForwardMouseWheel(t *testing.T) {
	mock := tmux.NewMockClient()

	svc := NewService(mock)
	require.NoError(t, svc.ForwardMouseWheel(context.Background(), "devbox", true, 10, 5))
	require.NoError(t, svc.ForwardMouseWheel(context.Background(), "devbox", false, 0, 0))

	require.Len(t, mock.WheelEvents, 2)
	assert.Equal(t, tmux.WheelEvent{Target: "devbox", Up: true, X: 10, Y: 5}, mock.WheelEvents[0])
	assert.Equal(t, tmux.WheelEvent{Target: "devbox", Up: false, X: 0, Y: 0}, mock.WheelEvents[1])
}

func TestServiceCaptureKeepsFullContent(t *testing.T) {
	mock := tmux.NewMockClient()
	mock.Captured["devbox"] = strings.Join([]string{
		"row0", "row1", strings.Repeat("w", 120),
	}, "\n")

	svc := NewService(mock)
	preview, err := svc.Capture(context.Background(), "devbox", 20, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, len(strings.Split(preview.Full, "\n")), "Full keeps every pane row untruncated")
	assert.Contains(t, preview.Full, strings.Repeat("w", 120), "Full keeps full-width rows")
	// The windowed Content remains as before (truncated to the preview size).
	assert.NotContains(t, preview.Content, strings.Repeat("w", 120))
}
