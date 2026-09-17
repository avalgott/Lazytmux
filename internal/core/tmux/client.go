package tmux

import "context"

// Client abstracts tmux operations for testability.
type Client interface {
	// ListClients returns all attached tmux clients.
	ListClients(ctx context.Context) ([]ClientInfo, error)

	// FindActiveClient returns the most recently active client.
	FindActiveClient(ctx context.Context) (*ClientInfo, error)

	// ListSessions returns all tmux sessions in the server.
	ListSessions(ctx context.Context) ([]SessionInfo, error)

	// HasSession checks if a tmux session exists.
	HasSession(ctx context.Context, name string) (bool, error)

	// NewSession creates a new tmux session.
	NewSession(ctx context.Context, opts NewSessionOpts) error

	// KillSession destroys a tmux session.
	KillSession(ctx context.Context, target string) error

	// RenameSession renames a tmux session.
	RenameSession(ctx context.Context, target, newName string) error

	// ListWindows returns all windows in a session.
	ListWindows(ctx context.Context, session string) ([]WindowInfo, error)

	// NewWindow creates a new window in a session.
	NewWindow(ctx context.Context, opts NewWindowOpts) error

	// RespawnPane respawns a dead pane with a new command.
	RespawnPane(ctx context.Context, target, cmd string) error

	// KillWindow destroys a tmux window.
	KillWindow(ctx context.Context, target string) error

	// ListPanes returns all panes (optionally filtered by session).
	ListPanes(ctx context.Context, session string) ([]PaneInfo, error)

	// CapturePaneContent captures the visible content of a pane (plain text).
	CapturePaneContent(ctx context.Context, target string) (string, error)

	// CapturePaneANSI captures pane content with ANSI escape codes.
	CapturePaneANSI(ctx context.Context, target string) (string, error)

	// CapturePaneANSIWithCursor captures pane content with ANSI escape codes
	// and the pane cursor position in a single tmux invocation. The content
	// and the cursor must come from the same pane state — full-screen
	// programs that repaint constantly (e.g. Claude Code) shift their layout
	// between two separate calls, which puts the rendered cursor one row off.
	CapturePaneANSIWithCursor(ctx context.Context, target string) (content string, cursorX, cursorY int, paneID string, err error)

	// CapturePaneANSIHistory captures the whole pane history from tmux's
	// oldest-history sentinel ("-S -") to the current bottom in one
	// operation, plus the pane height from the same atomic invocation. Both
	// bounds are resolved atomically by tmux, so a pane that scrolls
	// concurrently cannot produce a partial snapshot. The height lets callers
	// detect alternate-screen panes, whose capture contains nothing beyond
	// the visible screen.
	CapturePaneANSIHistory(ctx context.Context, target string) (content string, paneHeight int, paneID string, err error)

	// PaneInputFlags reports the pane's input mode: alternate screen active,
	// SGR (1006) mouse tracking enabled, and the 0-based pane cursor
	// position.
	PaneInputFlags(ctx context.Context, target string) (altOn, mouseAny bool, cursorX, cursorY int, paneID string, err error)

	// SendMouseWheel sends a mouse wheel event to the pane's input stream
	// as SGR mouse escape sequences (0-based pane cursor coordinates).
	SendMouseWheel(ctx context.Context, target string, up bool, x, y int) error

	// SendKeys sends key sequences to a tmux target.
	// Keys are interpreted as tmux key names (e.g., "Enter", "Space").
	SendKeys(ctx context.Context, target string, keys ...string) error

	// SendKeysLiteral sends text literally to a tmux target (send-keys -l).
	// The text is NOT interpreted as key names — useful for rune characters.
	SendKeysLiteral(ctx context.Context, target string, text string) error

	// PasteToPane loads text into a tmux buffer and pastes it to the target.
	// Uses paste-buffer -p to send bracketed paste sequences.
	PasteToPane(ctx context.Context, target string, text string) error

	// ShowMessage executes display-message with a format string and returns the result.
	ShowMessage(ctx context.Context, target, format string) (string, error)

	// GetOption returns the value of a tmux option.
	GetOption(ctx context.Context, target, option string) (string, error)

	// ResizeWindow resizes a tmux window.
	ResizeWindow(ctx context.Context, target string, width, height int) error
}
