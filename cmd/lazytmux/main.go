// Command lazytmux is a lazygit-style TUI for managing tmux sessions.
// It lists all sessions in the default tmux server, shows a live preview of
// the selected session, and attaches on Enter. After the user detaches from
// the attached session, the dashboard returns.
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/avalgott/Lazytmux/internal/core/tmux"
	"github.com/avalgott/Lazytmux/internal/gui"
	"github.com/avalgott/Lazytmux/internal/session"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lazytmux:", err)
		os.Exit(1)
	}
}

// run drives the dashboard/attach loop: each Run() invocation shows the
// dashboard; when the user presses Enter, Run returns the target session,
// the terminal is restored, and we attach to it. When the user detaches,
// the loop shows the dashboard again.
//
// When lazytmux runs inside a tmux session, the attach takes over the
// terminal as a new tmux client (the original client is displaced), so after
// detaching the user returns to the dashboard on a raw terminal — their
// original session is still alive detached and reachable with plain
// `tmux attach`. The pty size is captured before the attach and restored
// afterwards in case the client left it corrupt.
func run() error {
	svc := session.NewService(tmux.NewExecClient())

	for {
		app, err := gui.NewApp(svc)
		if err != nil {
			return fmt.Errorf("init TUI: %w", err)
		}
		if err := app.Run(); err != nil {
			return err
		}

		target := app.AttachTarget()
		if target == "" {
			return nil
		}

		// Terminal is restored (gocui closed); attach normally. $TMUX is
		// cleared inside Attach so this also works when lazytmux itself
		// runs inside a tmux session.
		width, height := terminalSize()
		if err := svc.Attach(target); err != nil {
			fmt.Fprintf(os.Stderr, "attach %q: %v\n", target, err)
			continue
		}

		// Detached from the target. Restore the pty size in case the client
		// left it corrupt, then loop and show the dashboard again.
		if width > 0 {
			setTerminalSize(width, height)
		}
	}
}

// terminalSize returns the size of the controlling terminal, or 0,0 if the
// size cannot be determined (e.g. stdin is not a tty).
func terminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return 0, 0
	}
	return width, height
}

// setTerminalSize resets the size of the controlling terminal. Used to undo
// the size corruption a detached tmux client leaves behind.
func setTerminalSize(width, height int) {
	ws := &unix.Winsize{Col: uint16(width), Row: uint16(height)}
	_ = unix.IoctlSetWinsize(int(os.Stdin.Fd()), unix.TIOCSWINSZ, ws)
}
