// Package session implements lazytmux's session operations on top of the
// tmux.Client abstraction. It is stateless: tmux itself is the source of
// truth, so sessions created outside lazytmux are discovered automatically.
package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/avalgott/Lazytmux/internal/core/shell"
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
	// CaptureScrollback captures a range of the session's pane history
	// (including the visible screen) with ANSI escape codes. start/end are
	// tmux capture-pane line offsets: 0 is the top of the visible screen,
	// negative values count back into the scrollback history.
	CaptureScrollback(ctx context.Context, name string, start, end int) (Preview, error)
	// HistorySize returns the number of lines in the pane's scrollback
	// history (the visible screen excluded).
	HistorySize(ctx context.Context, name string) (int, error)
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
//
// A non-empty command is wrapped in an interactive shell (the user's $SHELL
// sources a temp script holding the command, then execs a fresh shell). This
// keeps the shell between the command and the pane, so Ctrl+C interrupts the
// command without killing the pane — which would otherwise close the window
// and delete the whole session, since tmux runs the command as the pane's
// process. The script deletes itself when the shell reads it.
func (s *Service) Create(ctx context.Context, opts CreateOpts) error {
	command := opts.Command
	script := ""
	if command != "" {
		var err error
		script, err = writeCommandScript(command)
		if err != nil {
			return fmt.Errorf("write command script: %w", err)
		}
		// The path is always created directly under /tmp (never os.TempDir,
		// which honors TMPDIR and could introduce spaces or metacharacters),
		// so it is safe inside the single-quoted wrapper — same trick as
		// lazyclaude's launcher scripts. The keyword to run the script is
		// chosen per shell: fish uses `source`, POSIX shells use `.`.
		command = fmt.Sprintf(`exec "$SHELL" -lic '%s %s; exec "$SHELL"'`, shellSourceKeyword(), script)
	}

	err := s.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:     opts.Name,
		StartDir: opts.Dir,
		Command:  command,
		Detached: true,
	})
	if err != nil {
		// Clean up only on failure; on success the script self-deletes when
		// the shell sources it.
		if script != "" {
			_ = os.Remove(script)
		}
		return err
	}
	return nil
}

// shellSourceKeyword returns the keyword the user's shell uses to run a
// script in the current process: fish uses `source`, POSIX shells use `.`.
// The wrapper itself is executed by "$SHELL", so both must agree.
func shellSourceKeyword() string {
	if filepath.Base(os.Getenv("SHELL")) == "fish" {
		return "source"
	}
	return "."
}

// writeCommandScript writes the user's command to a temp file whose first
// line removes the file itself. The file is created directly under /tmp (not
// os.TempDir) so the path is always safe to interpolate into the
// single-quoted shell wrapper, regardless of TMPDIR. Returns the script path.
func writeCommandScript(command string) (string, error) {
	f, err := os.CreateTemp("/tmp", "lazytmux-cmd-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o700); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if _, err := fmt.Fprintf(f, "rm -f %s\n%s\n", shell.Quote(f.Name()), command); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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
// preview reproduces colors as closely as possible. The content and the pane
// cursor are fetched atomically so the rendered cursor never disagrees with
// the rendered content.
func (s *Service) Capture(ctx context.Context, name string, width, height int) (Preview, error) {
	content, cursorX, cursorY, err := s.tmux.CapturePaneANSIWithCursor(ctx, name)
	if err != nil {
		return Preview{}, err
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > width {
			lines[i] = ansi.Truncate(line, width, "")
		}
	}
	// Anchor the vertical window to the cursor, like a real terminal does:
	// when the cursor sits below the visible window (e.g. after a full-screen
	// program like top exits without restoring the screen, leaving the shell
	// prompt on the last row), show the window that contains it instead of
	// cutting the prompt off.
	if height > 0 && len(lines) > height {
		start := 0
		if cursorY >= height {
			start = cursorY - height + 1
			if start > len(lines)-height {
				start = len(lines) - height
			}
		}
		lines = lines[start : start+height]
		cursorY -= start
	}

	return Preview{
		Content: strings.Join(lines, "\n"),
		CursorX: cursorX,
		CursorY: cursorY,
	}, nil
}

// CaptureScrollback captures a range of the session's pane history.
func (s *Service) CaptureScrollback(ctx context.Context, name string, start, end int) (Preview, error) {
	content, err := s.tmux.CapturePaneANSIRange(ctx, name, start, end)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Content: content}, nil
}

// HistorySize returns the number of scrollback lines in the session's pane.
func (s *Service) HistorySize(ctx context.Context, name string) (int, error) {
	out, err := s.tmux.ShowMessage(ctx, name, "#{history_size}")
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n, nil
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
