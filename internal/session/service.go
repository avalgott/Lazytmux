// Package session implements lazytmux's session operations on top of the
// tmux.Client abstraction. It is stateless: tmux itself is the source of
// truth, so sessions created outside lazytmux are discovered automatically.
// A service may carry an immutable Session Plan (see internal/plan) as
// startup configuration: listed sessions are annotated with plan metadata,
// and ApplyPlan creates missing sessions once. Nothing is persisted.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/avalgott/Lazytmux/internal/core/shell"
	"github.com/avalgott/Lazytmux/internal/core/tmux"
	"github.com/avalgott/Lazytmux/internal/plan"
)

// Info is a read-only view of a tmux session for display. ID is tmux's
// session ID, Created its creation time, and ServerPID the tmux server
// incarnation, names, IDs, and even creation seconds can be reused after
// a restart, so the full triple is the stable identity.
type Info struct {
	Name      string
	ID        string
	Created   int64
	ServerPID int64 // the tmux server incarnation (IDs recycle across restarts)
	Path      string
	Attached  bool
	Windows   int
	Plan      *plan.Session // plan entry this session matches by name; nil = ad-hoc
}

// CreateOpts configures a new tmux session.
type CreateOpts struct {
	Name    string
	Dir     string // working directory (-c); empty = tmux default
	Command string // command to run; empty = the user's normal shell
}

// Preview holds captured pane content and cursor position. PaneHeight is the
// pane's height at capture time, for a whole-history capture it separates
// real scrollback from a snapshot that contains nothing beyond the visible
// screen (alternate-screen panes have no saved history).
type Preview struct {
	Content    string
	Full       string // raw full-pane capture (untruncated, unwindowed)
	CursorX    int
	CursorY    int
	PaneID     string // the tmux pane the capture came from (%N)
	PaneHeight int
}

// Provider abstracts session operations for the GUI layer.
type Provider interface {
	List(ctx context.Context) ([]Info, error)
	Create(ctx context.Context, opts CreateOpts) error
	Kill(ctx context.Context, name string) error
	Rename(ctx context.Context, name, newName string) error
	Capture(ctx context.Context, name string, width, height int) (Preview, error)
	// CaptureScrollback captures the session's whole pane history, from
	// tmux's oldest-history sentinel to the current bottom, in one atomic
	// tmux operation, with ANSI escape codes and the pane height.
	CaptureScrollback(ctx context.Context, name string) (Preview, error)
	// PaneInputFlags reports the active pane's input mode: alternate screen
	// active, SGR (1006) mouse tracking enabled, and the 0-based cursor
	// position. The GUI uses it to decide whether the wheel should go to the
	// pane's program (which handles its own scrolling) or to lazytmux scroll
	// mode, SGR is the only wheel encoding it emits.
	PaneInputFlags(ctx context.Context, name string) (altOn, sgrMouse bool, cursorX, cursorY int, paneID string, err error)
	// ForwardMouseWheel sends a mouse wheel event to the pane's input
	// stream at the given 0-based pane-relative mouse coordinates (not the
	// pane's cursor position, SGR consumers pick the hovered widget from
	// them).
	ForwardMouseWheel(ctx context.Context, name string, up bool, cursorX, cursorY int) error
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
	plan *plan.Plan
}

// NewService creates a session service backed by a tmux client.
func NewService(tc tmux.Client) *Service {
	return &Service{tmux: tc}
}

// NewServiceWithPlan creates a session service that annotates listed
// sessions with the given plan and can reconcile it via ApplyPlan.
func NewServiceWithPlan(tc tmux.Client, p *plan.Plan) *Service {
	return &Service{tmux: tc, plan: p}
}

// List returns all sessions in the default tmux server, sorted by name.
// tmux's own list-sessions order is its internal tree order, which shifts
// when sessions are created or killed; a stable sort keeps navigation
// predictable.
//
// A missing server is mapped to an empty list rather than an error: when the
// last session is killed the tmux server exits, and "no sessions" is the
// state the dashboard should show (with the create hint, since n restarts
// the server). tmux words this two ways, "no server running" for a server
// that exited, and "error connecting to" for one that was never started.
func (s *Service) List(ctx context.Context) ([]Info, error) {
	sessions, err := s.tmux.ListSessions(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting to") {
			return nil, nil
		}
		return nil, err
	}
	// An active plan annotates matching sessions with their plan metadata;
	// everything else is ad-hoc. tmux stays the source of truth, the join
	// happens fresh on every list, nothing is remembered between calls.
	var planByName map[string]*plan.Session
	if s.plan != nil {
		planByName = make(map[string]*plan.Session, len(s.plan.Sessions))
		for i := range s.plan.Sessions {
			planByName[s.plan.Sessions[i].Name] = &s.plan.Sessions[i]
		}
	}
	infos := make([]Info, len(sessions))
	for i, sess := range sessions {
		infos[i] = Info{
			Name:      sess.Name,
			ID:        sess.ID,
			Created:   sess.Created,
			ServerPID: sess.ServerPID,
			Path:      sess.Path,
			Attached:  sess.Attached,
			Windows:   sess.Windows,
			Plan:      planByName[sess.Name],
		}
	}
	sort.Slice(infos, func(i, j int) bool {
		return strings.ToLower(infos[i].Name) < strings.ToLower(infos[j].Name)
	})
	return infos, nil
}

// ApplyPlan creates every planned session that does not already exist in
// tmux, using the same create machinery as the GUI dialogs (the shell
// wrapper included). Existing sessions, planned or ad-hoc, are never
// touched: a session whose name matches a plan entry counts as that planned
// session and is not recreated or verified. Creation is best-effort per
// session: every missing session is attempted and all failures are collected
// and returned together, so one broken entry does not hide failures of the
// others, and successfully created sessions are never rolled back. A missing
// tmux server is treated as an empty session list, so the first run with a
// plan starts the server.
func (s *Service) ApplyPlan(ctx context.Context) error {
	if s.plan == nil {
		return nil
	}
	existing, err := s.List(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for plan: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, info := range existing {
		have[info.Name] = struct{}{}
	}
	var errs []error
	for _, sess := range s.plan.Sessions {
		if _, ok := have[sess.Name]; ok {
			continue
		}
		if err := s.Create(ctx, CreateOpts{Name: sess.Name, Dir: sess.Cwd, Command: sess.Command}); err != nil {
			errs = append(errs, fmt.Errorf("create planned session %q: %w", sess.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Create starts a new detached tmux session. When opts.Command is empty the
// session runs the user's normal shell.
//
// A non-empty command is wrapped in an interactive shell (the user's $SHELL
// sources a temp script holding the command, then execs a fresh shell). This
// keeps the shell between the command and the pane, so Ctrl+C interrupts the
// command without killing the pane, which would otherwise close the window
// and delete the whole session, since tmux runs the command as the pane's
// process. The script deletes itself when the shell reads it.
func (s *Service) Create(ctx context.Context, opts CreateOpts) error {
	command := opts.Command
	script := ""
	var sessionEnv map[string]string
	if command != "" {
		var err error
		script, err = writeCommandScript(command)
		if err != nil {
			return fmt.Errorf("write command script: %w", err)
		}
		command, sessionEnv, err = buildShellWrapper(script)
		if err != nil {
			_ = os.Remove(script)
			return err
		}
	}

	err := s.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:     opts.Name,
		StartDir: opts.Dir,
		Command:  command,
		Detached: true,
		Env:      sessionEnv,
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

// buildShellWrapper returns the tmux command that runs the user's command
// inside their interactive shell, the session environment that pins that
// shell, or an error for shells we cannot wrap correctly.
//
// The shell is resolved here and pinned into the new session's environment,
// so the template selection can never diverge from the shell the pane
// actually executes (a long-lived tmux server may carry a stale SHELL).
// Every supported shell gets a template that works in it; the rest are
// rejected explicitly rather than running a silently broken command. The
// script path is always created directly under /tmp (never os.TempDir, which
// honors TMPDIR and could introduce spaces or metacharacters), so it is safe
// inside the single-quoted wrapper, same trick as lazyclaude's launcher
// scripts.
func buildShellWrapper(script string) (string, map[string]string, error) {
	shellPath := os.Getenv("SHELL")
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	name := filepath.Base(shellPath)

	// The templates are per shell family, fish does not understand POSIX
	// ${var:-default} expansion, so each family gets its own syntax. SHELL is
	// pinned via the session env, so the plain "$SHELL" reference is exact.
	var relaunch string
	switch name {
	case "sh", "bash", "dash", "ksh", "zsh":
		relaunch = `exec "$SHELL" -lic '. ` + script + `; exec "$SHELL"'`
	case "fish":
		relaunch = `exec "$SHELL" -lic 'source ` + script + `; exec "$SHELL"'`
	case "csh", "tcsh":
		return "", nil, fmt.Errorf("shell %q is not supported for command sessions; use an empty command or a POSIX shell", name)
	default:
		return "", nil, fmt.Errorf("unknown shell %q; command sessions support sh, bash, dash, ksh, zsh, and fish", name)
	}
	return relaunch, map[string]string{"SHELL": shellPath}, nil
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
	content, cursorX, cursorY, paneID, err := s.tmux.CapturePaneANSIWithCursor(ctx, name)
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
		Full:    content,
		CursorX: cursorX,
		CursorY: cursorY,
		PaneID:  paneID,
	}, nil
}

// CaptureScrollback captures the session's whole pane history in one
// atomic tmux operation.
func (s *Service) CaptureScrollback(ctx context.Context, name string) (Preview, error) {
	content, paneH, paneID, err := s.tmux.CapturePaneANSIHistory(ctx, name)
	if err != nil {
		return Preview{}, err
	}
	return Preview{Content: content, PaneHeight: paneH, PaneID: paneID}, nil
}

// PaneInputFlags reports the active pane's input mode.
func (s *Service) PaneInputFlags(ctx context.Context, name string) (bool, bool, int, int, string, error) {
	return s.tmux.PaneInputFlags(ctx, name)
}

// ForwardMouseWheel sends a mouse wheel event to the pane's input stream.
func (s *Service) ForwardMouseWheel(ctx context.Context, name string, up bool, cursorX, cursorY int) error {
	return s.tmux.SendMouseWheel(ctx, name, up, cursorX, cursorY)
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

// ValidateName reports whether a session name is acceptable for the
// create/rename dialogs. The name is trimmed first (dialogs tolerate stray
// whitespace), then checked against tmux.ValidateSessionName, the canonical
// rule set, so names validate identically whether they come from a dialog
// or a session plan.
func ValidateName(name string) error {
	return tmux.ValidateSessionName(strings.TrimSpace(name))
}
