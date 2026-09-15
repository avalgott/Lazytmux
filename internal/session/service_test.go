package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"lazytmux/internal/core/tmux"
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
	assert.Equal(t, "ssh devbox", opts.Command)
	assert.True(t, opts.Detached)
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
