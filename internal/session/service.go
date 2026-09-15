// Package session implements lazytmux's session operations on top of the
// tmux.Client abstraction. It is stateless: tmux itself is the source of
// truth, so sessions created outside lazytmux are discovered automatically.
package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/avalgott/Lazytmux/internal/core/tmux"
)

// Info is a read-only view of a tmux session for display.
type Info struct {
	Name     string
	Path     string
	Attached bool
	Windows  int
}

// CreateOpts configures a new tmux session.
type CreateOpts struct {
	Name    string
	Dir     string // working directory (-c); empty = tmux default
	Command string // command to run; empty = the user's normal shell
}

// Preview holds captured pane content and cursor position.
type Preview struct {
	Content string
	CursorX int
	CursorY int
}

// Provider abstracts session operations for the GUI layer.
type Provider interface {
	List(ctx context.Context) ([]Info, error)
	Create(ctx context.Context, opts CreateOpts) error
	Kill(ctx context.Context, name string) error
	Rename(ctx context.Context, name, newName string) error
	Capture(ctx context.Context, name string, width, height int) (Preview, error)
	// SendKeys sends tmux key names (e.g. "Enter", "Up", "C-c") to the
	// session's active pane. Used by fullscreen passthrough mode.
	SendKeys(ctx context.Context, name string, keys ...string) error
	// SendLiteral sends text literally (send-keys -l) to the session's
	// active pane.
	SendLiteral(ctx context.Context, name, text string) error
	// Paste sends text to the pane as a bracketed paste.
	Paste(ctx context.Context, name, text string) error
	// ResizeWindow resizes the session's current window (tmux resize-window).
	// Used by fullscreen mode so the target pane fills the terminal.
	ResizeWindow(ctx context.Context, name string, width, height int) error
	// Attach attaches the calling terminal to a session and blocks until the
	// user detaches. $TMUX is cleared so attaching from inside a tmux session
	// is allowed (the attach runs as a nested client).
	Attach(name string) error
}

// Service implements Provider using the default tmux server.
type Service struct {
	tmux tmux.Client
}

// NewService creates a session service backed by a tmux client.
func NewService(tc tmux.Client) *Service {
	return &Service{tmux: tc}
}

// List returns all sessions in the default tmux server, sorted by name.
// tmux's own list-sessions order is its internal tree order, which shifts
// when sessions are created or killed; a stable sort keeps navigation
// predictable.
//
// A missing server is mapped to an empty list rather than an error: when the
// last session is killed the tmux server exits, and "no sessions" is the
// state the dashboard should show (with the create hint, since n restarts
// the server).
func (s *Service) List(ctx context.Context) ([]Info, error) {
	sessions, err := s.tmux.ListSessions(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") {
			return nil, nil
		}
		return nil, err
	}
	infos := make([]Info, len(sessions))
	for i, sess := range sessions {
		infos[i] = Info{
			Name:     sess.Name,
			Path:     sess.Path,
			Attached: sess.Attached,
			Windows:  sess.Windows,
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		return strings.ToLower(infos[i].Name) < strings.ToLower(infos[j].Name)
	})
	return infos, nil
}

// Create starts a new detached tmux session. When opts.Command is empty the
// session runs the user's normal shell.
func (s *Service) Create(ctx context.Context, opts CreateOpts) error {
	return s.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:     opts.Name,
		StartDir: opts.Dir,
		Command:  opts.Command,
		Detached: true,
	})
}

// Kill destroys a tmux session.
func (s *Service) Kill(ctx context.Context, name string) error {
	return s.tmux.KillSession(ctx, name)
}

// Rename renames a tmux session.
func (s *Service) Rename(ctx context.Context, name, newName string) error {
	return s.tmux.RenameSession(ctx, name, newName)
}

// Capture returns the visible content of the session's active pane, cropped
// to width x height columns/rows. ANSI escape sequences are preserved so the
// preview reproduces colors as closely as possible.
func (s *Service) Capture(ctx context.Context, name string, width, height int) (Preview, error) {
	content, err := s.tmux.CapturePaneANSI(ctx, name)
	if err != nil {
		return Preview{}, err
	}

	var cursorX, cursorY int
	if pos, posErr := s.tmux.ShowMessage(ctx, name, "#{cursor_x},#{cursor_y}"); posErr == nil {
		parts := strings.SplitN(strings.TrimSpace(pos), ",", 2)
		if len(parts) == 2 {
			cursorX, _ = strconv.Atoi(parts[0])
			cursorY, _ = strconv.Atoi(parts[1])
		}
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	if len(lines) > height {
		lines = lines[:height]
	}

	return Preview{
		Content: strings.Join(lines, "\n"),
		CursorX: cursorX,
		CursorY: cursorY,
	}, nil
}

// SendKeys sends tmux key names to the session's active pane.
func (s *Service) SendKeys(ctx context.Context, name string, keys ...string) error {
	return s.tmux.SendKeys(ctx, name, keys...)
}

// SendLiteral sends text literally to the session's active pane.
func (s *Service) SendLiteral(ctx context.Context, name, text string) error {
	return s.tmux.SendKeysLiteral(ctx, name, text)
}

// Paste sends text to the session's active pane as a bracketed paste.
func (s *Service) Paste(ctx context.Context, name, text string) error {
	return s.tmux.PasteToPane(ctx, name, text)
}

// ResizeWindow resizes the session's current window.
func (s *Service) ResizeWindow(ctx context.Context, name string, width, height int) error {
	return s.tmux.ResizeWindow(ctx, name, width, height)
}

// Attach attaches the calling terminal to the session and blocks until the
// user detaches. $TMUX and $TMUX_PANE are cleared so a lazytmux instance
// running inside tmux can attach without the "sessions should be nested with
// care" refusal.
func (s *Service) Attach(name string) error {
	cmd := exec.Command("tmux", "attach-session", "-t", name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withoutTmuxEnv(os.Environ())
	return cmd.Run()
}

// withoutTmuxEnv returns env with TMUX* variables removed.
func withoutTmuxEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ValidateName reports whether a session name is acceptable. tmux rejects
// names containing ':' or '.', and empty names; other errors (duplicates,
// invalid options) surface from tmux itself on create/rename.
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("session name is required")
	}
	if strings.ContainsAny(name, ":.&|;") {
		return fmt.Errorf("session name %q contains an invalid character", name)
	}
	return nil
}
