package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultTimeout = 5 * time.Second

// validateShellSafe rejects strings containing shell metacharacters.
func validateShellSafe(s, field string) error {
	for _, c := range s {
		switch c {
		case ';', '&', '|', '`', '$', '(', ')', '{', '}', '<', '>', '\n', '\r', '\x00':
			return fmt.Errorf("%s contains unsafe character %q", field, c)
		}
	}
	return nil
}

// ValidateSessionName reports whether a session name is safe to pass to tmux
// and acceptable across lazytmux. It is the single source of truth for
// session-name rules: empty names, shell metacharacters (the set
// validateShellSafe rejects), and tmux's ':' and '.' separators are all
// rejected.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name is required")
	}
	if err := validateShellSafe(name, "session name"); err != nil {
		return err
	}
	if strings.ContainsAny(name, ":.") {
		return fmt.Errorf("session name %q contains an invalid character", name)
	}
	return nil
}

// envKeyPattern matches valid POSIX environment variable names.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvKey rejects env keys that are not valid POSIX identifiers.
func validateEnvKey(k string) error {
	if !envKeyPattern.MatchString(k) {
		return fmt.Errorf("env key %q is not a valid identifier", k)
	}
	return nil
}

// ExecClient implements Client by executing tmux commands.
type ExecClient struct {
	tmuxBin  string
	socket   string   // tmux -L socket name (empty = default server)
	debugLog *os.File // optional debug log file
}

// NewExecClient creates an ExecClient using the default tmux server.
func NewExecClient() *ExecClient {
	return &ExecClient{tmuxBin: "tmux"}
}

// NewExecClientWithSocket creates an ExecClient using a dedicated tmux socket.
func NewExecClientWithSocket(socket string) *ExecClient {
	return &ExecClient{tmuxBin: "tmux", socket: socket}
}

// SetDebugLog enables command logging to a file.
func (c *ExecClient) SetDebugLog(f *os.File) {
	c.debugLog = f
}

func (c *ExecClient) logCmd(prefix string, args []string, output string, err error) {
	if c.debugLog == nil {
		return
	}
	if err != nil {
		fmt.Fprintf(c.debugLog, "%s: tmux %s → ERR: %v (out: %s)\n", prefix, strings.Join(args, " "), err, strings.TrimSpace(output))
	} else {
		fmt.Fprintf(c.debugLog, "%s: tmux %s → OK (out: %s)\n", prefix, strings.Join(args, " "), strings.TrimSpace(output))
	}
}

// Socket returns the configured socket name (empty = default).
func (c *ExecClient) Socket() string {
	return c.socket
}

func (c *ExecClient) prependSocket(args []string) []string {
	prefix := []string{"-u"} // force UTF-8
	if c.socket != "" {
		// Use -S for absolute paths, -L for socket names
		if strings.HasPrefix(c.socket, "/") {
			prefix = append(prefix, "-S", c.socket)
		} else {
			prefix = append(prefix, "-L", c.socket)
		}
	}
	return append(prefix, args...)
}

// run executes a tmux command and returns the trimmed output. Most commands
// are parsed field-wise, so surrounding whitespace is noise.
func (c *ExecClient) run(ctx context.Context, args ...string) (string, error) {
	out, err := c.runRaw(ctx, args...)
	return strings.TrimSpace(out), err
}

// runRaw executes a tmux command and returns the output untouched. Needed
// for capture-pane -S/-E ranges, where leading/trailing blank lines carry
// meaning (they shift the content relative to the requested offsets).
func (c *ExecClient) runRaw(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	fullArgs := c.prependSocket(args)
	cmd := exec.CommandContext(ctx, c.tmuxBin, fullArgs...)

	// Use Output() (stdout only), CombinedOutput() mixes stderr into stdout
	// which corrupts parseWindows/parsePanes parsing.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()

	c.logCmd("run", fullArgs, string(out), err)
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w (stderr: %s)", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (c *ExecClient) ListClients(ctx context.Context) ([]ClientInfo, error) {
	out, err := c.run(ctx, "list-clients", "-F",
		"#{client_name}\t#{client_session}\t#{client_width}\t#{client_height}\t#{client_activity}")
	if err != nil {
		return nil, err
	}
	return parseClients(out), nil
}

func (c *ExecClient) FindActiveClient(ctx context.Context) (*ClientInfo, error) {
	clients, err := c.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	best := clients[0]
	for _, cl := range clients[1:] {
		if cl.Activity > best.Activity {
			best = cl
		}
	}
	return &best, nil
}

func (c *ExecClient) HasSession(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	fullArgs := c.prependSocket([]string{"has-session", "-t", name})
	cmd := exec.CommandContext(ctx, c.tmuxBin, fullArgs...)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	err := cmd.Run()
	c.logCmd("hasSession", fullArgs, "", err)
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// Non-exit error (e.g., binary not found, context cancelled).
			return false, fmt.Errorf("tmux has-session: %w", err)
		}
		// Exit code 1: distinguish "session not found" from transient errors
		// by checking stderr. tmux writes "can't find session" when the
		// session genuinely does not exist.
		stderrStr := stderr.String()
		if strings.Contains(stderrStr, "can't find session") ||
			strings.Contains(stderrStr, "no session") ||
			strings.Contains(stderrStr, "no server running") ||
			strings.Contains(stderrStr, "no current target") ||
			strings.Contains(stderrStr, "error connecting") {
			return false, nil
		}
		// Any other stderr (e.g., "error connecting")
		// is a transient error, propagate it.
		return false, fmt.Errorf("tmux has-session transient error: %s", strings.TrimSpace(stderrStr))
	}
	return true, nil
}

func (c *ExecClient) NewSession(ctx context.Context, opts NewSessionOpts) error {
	if err := validateShellSafe(opts.Name, "session name"); err != nil {
		return err
	}
	// opts.Command is not validated, it is user input and may contain shell
	// constructs like `ssh devbox` or `tail -f /var/log/app.log`.
	for k := range opts.Env {
		if err := validateEnvKey(k); err != nil {
			return err
		}
	}

	args := []string{"new-session", "-s", opts.Name}
	if opts.WindowName != "" {
		args = append(args, "-n", opts.WindowName)
	}
	if opts.StartDir != "" {
		args = append(args, "-c", opts.StartDir)
	}
	if opts.Detached {
		args = append(args, "-d")
	}
	if opts.Width > 0 {
		args = append(args, "-x", fmt.Sprintf("%d", opts.Width))
	}
	if opts.Height > 0 {
		args = append(args, "-y", fmt.Sprintf("%d", opts.Height))
	}
	// Pass environment variables via tmux -e flag (reaches the shell inside tmux)
	for k, v := range opts.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	if opts.Command != "" {
		args = append(args, opts.Command)
	}

	// Chain post-creation commands (e.g. set-option, unbind-key)
	for _, postCmd := range opts.PostCommands {
		args = append(args, ";")
		args = append(args, postCmd...)
	}

	ctx2, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	fullArgs := c.prependSocket(args)
	cmd := exec.CommandContext(ctx2, c.tmuxBin, fullArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	c.logCmd("NewSession", fullArgs, string(out), err)
	if err != nil {
		return fmt.Errorf("new-session: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *ExecClient) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	out, err := c.run(ctx, "list-sessions", "-F",
		"#{session_name}\t#{session_id}\t#{session_path}\t#{session_attached}\t#{session_windows}\t#{session_created}\t#{pid}")
	if err != nil {
		return nil, err
	}
	return parseSessions(out), nil
}

func (c *ExecClient) KillSession(ctx context.Context, target string) error {
	_, err := c.run(ctx, "kill-session", "-t", target)
	return err
}

func (c *ExecClient) RenameSession(ctx context.Context, target, newName string) error {
	if err := validateShellSafe(newName, "session name"); err != nil {
		return err
	}
	_, err := c.run(ctx, "rename-session", "-t", target, newName)
	return err
}

func (c *ExecClient) ListWindows(ctx context.Context, session string) ([]WindowInfo, error) {
	out, err := c.run(ctx, "list-windows", "-t", session, "-F",
		"#{window_id}\t#{window_index}\t#{window_name}\t#{session_name}\t#{window_active}")
	if err != nil {
		return nil, err
	}
	return parseWindows(out), nil
}

func (c *ExecClient) NewWindow(ctx context.Context, opts NewWindowOpts) error {
	for k := range opts.Env {
		if err := validateEnvKey(k); err != nil {
			return err
		}
	}

	args := []string{"new-window", "-t", opts.Session}
	if opts.Name != "" {
		args = append(args, "-n", opts.Name)
	}
	if opts.StartDir != "" {
		args = append(args, "-c", opts.StartDir)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	if opts.Command != "" {
		args = append(args, opts.Command)
	}

	ctx2, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	fullArgs := c.prependSocket(args)
	cmd := exec.CommandContext(ctx2, c.tmuxBin, fullArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	c.logCmd("NewWindow", fullArgs, string(out), err)
	if err != nil {
		return fmt.Errorf("new-window: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *ExecClient) RespawnPane(ctx context.Context, target, command string) error {
	_, err := c.run(ctx, "respawn-pane", "-t", target, "-k", command)
	return err
}

func (c *ExecClient) KillWindow(ctx context.Context, target string) error {
	_, err := c.run(ctx, "kill-window", "-t", target)
	return err
}

func (c *ExecClient) ResizeWindow(ctx context.Context, target string, width, height int) error {
	_, err := c.run(ctx, "resize-window", "-t", target,
		"-x", fmt.Sprintf("%d", width), "-y", fmt.Sprintf("%d", height))
	return err
}

func (c *ExecClient) ListPanes(ctx context.Context, session string) ([]PaneInfo, error) {
	args := []string{"list-panes", "-F", "#{pane_id}\t#{window_id}\t#{pane_pid}\t#{pane_dead}"}
	if session != "" {
		// -s lists panes across ALL windows in the session.
		// Without -s, -t targets only the active window.
		args = append(args, "-s", "-t", session)
	} else {
		args = append(args, "-a")
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parsePanes(out), nil
}

func (c *ExecClient) CapturePaneContent(ctx context.Context, target string) (string, error) {
	return c.run(ctx, "capture-pane", "-t", target, "-p")
}

// CapturePaneANSI captures pane content with ANSI color escape codes preserved.
func (c *ExecClient) CapturePaneANSI(ctx context.Context, target string) (string, error) {
	return c.run(ctx, "capture-pane", "-t", target, "-ep")
}

// CapturePaneANSIWithCursor captures the pane content and the cursor position
// in a single tmux invocation (capture-pane followed by display-message in
// the same command batch). The last line of the output is the cursor pair.
func (c *ExecClient) CapturePaneANSIWithCursor(ctx context.Context, target string) (string, int, int, string, error) {
	ctx2, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	args := []string{"capture-pane", "-t", target, "-ep", ";",
		"display-message", "-t", target, "-p", "#{cursor_x},#{cursor_y} #{pane_id}"}
	fullArgs := c.prependSocket(args)
	cmd := exec.CommandContext(ctx2, c.tmuxBin, fullArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	c.logCmd("CapturePaneANSIWithCursor", fullArgs, string(out), err)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("tmux %s: %w (stderr: %s)", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}

	content, cursorX, cursorY, paneID := splitCursorPair(string(out))
	return content, cursorX, cursorY, paneID, nil
}

// splitCursorPair splits the combined output of
// "capture-pane -ep ; display-message -p #{cursor_x},#{cursor_y}" into the
// capture content and the cursor position. The cursor pair is the final
// line when it parses as two integers. Splitting by line structure (rather
// than trimming trailing newlines) keeps blank trailing rows, an
// alt-screen app's cursor row, which TrimRight would destroy, wobbling the
// row count between captures.
func splitCursorPair(out string) (content string, cursorX, cursorY int, paneID string) {
	lines := strings.Split(out, "\n")
	// The output ends with the cursor line's newline; drop that phantom.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if n := len(lines); n > 0 {
		// The cursor line is "x,y %N": the cursor pair plus the pane ID, so
		// the capture and the pane it came from are one atomic observation.
		if parts := strings.SplitN(strings.TrimSpace(lines[n-1]), ",", 2); len(parts) == 2 {
			x, errX := strconv.Atoi(parts[0])
			rest := strings.Fields(parts[1])
			if len(rest) >= 1 {
				if y, errY := strconv.Atoi(rest[0]); errX == nil && errY == nil {
					cursorX, cursorY = x, y
					if len(rest) >= 2 {
						paneID = rest[1]
					}
					lines = lines[:n-1]
				}
			}
		}
	}
	return strings.Join(lines, "\n"), cursorX, cursorY, paneID
}

// CapturePaneANSIHistory captures from the oldest history line to the
// current bottom in one operation ("-" is tmux's start-of-history sentinel),
// plus the pane's height from the same atomic invocation. The height
// distinguishes real scrollback from a snapshot that contains nothing but
// the visible screen (alternate-screen panes such as Claude Code have no
// saved history at all).
func (c *ExecClient) CapturePaneANSIHistory(ctx context.Context, target string) (string, int, string, error) {
	ctx2, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	args := []string{"capture-pane", "-t", target, "-ep", "-S", "-", ";",
		"display-message", "-t", target, "-p", "#{pane_height} #{pane_id}"}
	fullArgs := c.prependSocket(args)
	cmd := exec.CommandContext(ctx2, c.tmuxBin, fullArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	c.logCmd("CapturePaneANSIHistory", fullArgs, string(out), err)
	if err != nil {
		return "", 0, "", fmt.Errorf("tmux %s: %w (stderr: %s)", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return splitPaneHeightLine(string(out))
}

// PaneInputFlags reports the pane's input mode in one display-message call:
// whether the alternate screen is active, whether the program has mouse
// tracking enabled with the SGR (1006) encoding, the only encoding
// SendMouseWheel emits, and the pane cursor position (0-based). The format
// is the positional argument, matching ShowMessage, display-message expands
// format variables there on every tmux version.
//
// mouse_any_flag is the aggregate over every tracking mode (1000 standard,
// 1002 button-event, 1003 any-event), NOT the 1003-only flag, that is
// mouse_all_flag, which would exclude vim-style 1000/1002 tracking. Verified
// against a live tmux: 1002+1006 reports any=1 sgr=1 (forward), 1006 alone
// reports any=0 sgr=1 (do not forward).
func (c *ExecClient) PaneInputFlags(ctx context.Context, target string) (bool, bool, int, int, string, error) {
	out, err := c.run(ctx, "display-message", "-t", target, "-p",
		"#{alternate_on} #{mouse_any_flag} #{mouse_sgr_flag} #{cursor_x} #{cursor_y} #{pane_id}")
	if err != nil {
		return false, false, 0, 0, "", err
	}
	return parseInputFlags(out)
}

// SendMouseWheel sends a mouse wheel event to the target pane's input stream
// as a single SGR mouse escape sequence (wheel motion is an impulse, there
// is no release event). A program with SGR mouse tracking enabled reads it
// as a real wheel event.
func (c *ExecClient) SendMouseWheel(ctx context.Context, target string, up bool, x, y int) error {
	_, err := c.run(ctx, "send-keys", "-l", "-t", target, "--", sgrWheel(up, x, y))
	return err
}

// splitPaneHeightLine splits the combined output of
// "capture-pane -ep -S - ; display-message -p #{pane_height}" into the raw
// capture content (its trailing newline preserved, capture output terminates
// with one) and the pane height.
func splitPaneHeightLine(out string) (string, int, string, error) {
	s := strings.TrimRight(out, "\n")
	idx := strings.LastIndex(s, "\n")
	if idx < 0 {
		return "", 0, "", fmt.Errorf("capture output missing pane-height line")
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 1 {
		return "", 0, "", fmt.Errorf("capture output missing pane-height line")
	}
	h, err := strconv.Atoi(fields[0])
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid pane height %q: %w", fields[0], err)
	}
	paneID := ""
	if len(fields) >= 2 {
		paneID = fields[1]
	}
	return s[:idx+1], h, paneID, nil
}

// parseInputFlags parses the display-message output of
// "#{alternate_on} #{mouse_any_flag} #{mouse_sgr_flag} #{cursor_x} #{cursor_y}".
// The mouse result is true only when a tracking mode is enabled AND the SGR
// (1006) encoding is selected, SGR alone leaves a program that is not
// listening for mouse events, and any other encoding would not understand
// the SGR sequences SendMouseWheel emits. mouse_any_flag covers every
// tracking mode (1000/1002/1003); mouse_all_flag would restrict the check
// to 1003 and stop forwarding to vim-style 1002-tracking panes.
func parseInputFlags(s string) (altOn, mouseSGR bool, cx, cy int, paneID string, err error) {
	fields := strings.Fields(s)
	if len(fields) != 6 {
		return false, false, 0, 0, "", fmt.Errorf("unexpected input flags %q", s)
	}
	var nums [5]int
	for i, f := range fields[:5] {
		n, e := strconv.Atoi(f)
		if e != nil {
			return false, false, 0, 0, "", fmt.Errorf("invalid input flags %q: %w", s, e)
		}
		nums[i] = n
	}
	return nums[0] == 1, nums[1] == 1 && nums[2] == 1, nums[3], nums[4], fields[5], nil
}

// sgrWheel builds the SGR mouse escape sequence for one wheel step. Wheel
// motion is reported as single impulses, unlike button presses there is no
// release event, so appending one would inject a spurious second event.
// Coordinates are converted from 0-based pane-relative mouse coordinates
// to SGR's 1-based scheme, clamped to at least 1.
func sgrWheel(up bool, x, y int) string {
	b := 64
	if !up {
		b = 65
	}
	cx, cy := x+1, y+1
	if cx < 1 {
		cx = 1
	}
	if cy < 1 {
		cy = 1
	}
	return fmt.Sprintf("\x1b[<%d;%d;%dM", b, cx, cy)
}

func (c *ExecClient) SendKeys(ctx context.Context, target string, keys ...string) error {
	args := append([]string{"send-keys", "-t", target}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

func (c *ExecClient) SendKeysLiteral(ctx context.Context, target string, text string) error {
	// tmux's global argument parser treats a standalone ";" argument as a
	// command separator, even in exec mode. This happens before individual
	// command parsing, so "--" does not prevent it. Multi-character strings
	// like "hello;" are safe because ";" must be the entire argument.
	// Escape the bare ";" → "\;" so tmux passes it through to send-keys.
	escaped := text
	if escaped == ";" {
		escaped = `\;`
	}
	args := []string{"send-keys", "-l", "-t", target, "--", escaped}
	_, err := c.run(ctx, args...)
	return err
}

func (c *ExecClient) PasteToPane(ctx context.Context, target string, text string) error {
	ctx2, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	loadArgs := c.prependSocket([]string{"load-buffer", "-"})
	loadCmd := exec.CommandContext(ctx2, c.tmuxBin, loadArgs...)
	loadCmd.Stdin = strings.NewReader(text)
	if out, err := loadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("load-buffer: %w (out: %s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := c.run(ctx, "paste-buffer", "-t", target, "-d", "-p"); err != nil {
		return fmt.Errorf("paste-buffer: %w", err)
	}
	return nil
}

func (c *ExecClient) ShowMessage(ctx context.Context, target, format string) (string, error) {
	args := []string{"display-message", "-t", target, "-p", format}
	return c.run(ctx, args...)
}

func (c *ExecClient) GetOption(ctx context.Context, target, option string) (string, error) {
	args := []string{"show-option", "-gqv"}
	if target != "" {
		args = []string{"show-option", "-t", target, "-qv"}
	}
	args = append(args, option)
	return c.run(ctx, args...)
}

// --- Parsers ---

func parseClients(out string) []ClientInfo {
	if out == "" {
		return nil
	}
	var clients []ClientInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 5 {
			continue
		}
		w, _ := strconv.Atoi(parts[2])
		h, _ := strconv.Atoi(parts[3])
		a, _ := strconv.ParseInt(parts[4], 10, 64)
		clients = append(clients, ClientInfo{
			Name:     parts[0],
			Session:  parts[1],
			Width:    w,
			Height:   h,
			Activity: a,
		})
	}
	return clients
}

func parseSessions(out string) []SessionInfo {
	if out == "" {
		return nil
	}
	var sessions []SessionInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 7)
		if len(parts) < 7 {
			continue
		}
		attached := parts[3] != "0"
		windows, _ := strconv.Atoi(parts[4])
		created, _ := strconv.ParseInt(parts[5], 10, 64)
		serverPID, _ := strconv.ParseInt(parts[6], 10, 64)
		sessions = append(sessions, SessionInfo{
			Name:      parts[0],
			ServerPID: serverPID,
			ID:        parts[1],
			Path:      parts[2],
			Attached:  attached,
			Windows:   windows,
			Created:   created,
		})
	}
	return sessions
}

func parseWindows(out string) []WindowInfo {
	if out == "" {
		return nil
	}
	var windows []WindowInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 5 {
			continue
		}
		idx, _ := strconv.Atoi(parts[1])
		windows = append(windows, WindowInfo{
			ID:      parts[0],
			Index:   idx,
			Name:    parts[2],
			Session: parts[3],
			Active:  parts[4] == "1",
		})
	}
	return windows
}

func parsePanes(out string) []PaneInfo {
	if out == "" {
		return nil
	}
	var panes []PaneInfo
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(parts[2])
		panes = append(panes, PaneInfo{
			ID:     parts[0],
			Window: parts[1],
			PID:    pid,
			Dead:   parts[3] == "1",
		})
	}
	return panes
}
