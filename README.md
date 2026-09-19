# lazytmux

A [lazygit](https://github.com/jesseduffield/lazygit)-style TUI for managing [tmux](https://github.com/tmux/tmux) sessions: browse every session, watch a live preview of it, and attach, all from one screen.

> **Status:** experimental MVP. It does one thing (list / preview / attach / create / rename / kill) and deliberately nothing else. Not a production framework.

```
┌ Sessions (3) ──────┐┌ devbox ───────────────────────────────┐
│  devbox      ●    ││ $ make test                           │
│  logs             ││ ok  lazytmux/internal/session         │
│  pg               ││ ok  lazytmux/internal/core/tmux       │
│                   ││ $ █                                   │
│                   ││                                       │
└───────────────────┘└───────────────────────────────────────┘
 j/k move  n new  d delete  r rename  Enter open  a attach  q quit
```

## Features

- **All sessions, automatically:** every session on the default tmux server is listed, including ones you created with plain `tmux new -s name`.
- **Live preview with scrolling:** the right panel mirrors the selected session's active pane, ANSI colors preserved and cropped to fit, refreshed about twice a second; press `Tab` to focus the panel and browse a frozen snapshot of its history with `j`/`k`, `PgUp`/`PgDn`, `g`/`G`, or the mouse wheel.
- **Fullscreen passthrough:** `Enter` expands the selected session to the full terminal and forwards every keystroke to it; `Ctrl+D` returns to the dashboard instantly.
- **Real attach too:** `a` attaches with `tmux attach-session`, the right tool for vim, htop, and other full-screen TUI apps; detaching returns you to the dashboard.
- **Session lifecycle:** create, rename, and kill sessions from the TUI.
- **Zero configuration:** no config files, no separate tmux socket, no state on disk.
- **Works with anything:** shells, SSH, database CLIs, log tails, dev servers, vim, htop. There is no editor-, agent-, or tool-specific behavior.

## Requirements

- tmux (any modern 3.x), installed and on `PATH`
- Go 1.25+ (build only)

## Installation

### Quick install (standalone binary)

```bash
curl -fsSL https://raw.githubusercontent.com/avalgott/Lazytmux/main/install.sh | sh
```

Downloads the pre-built binary from the latest GitHub release to `~/.local/bin/`, no Go required. If there is no release yet (or none matches your platform), the script falls back to cloning and building from source (requires git and Go 1.25+). The result is the same: `lazytmux` on your PATH.

One note: plain `go install ...@latest` won't work, because the vendored TUI forks (gocui/tcell) use relative `replace` directives, which the Go module proxy rejects. Same tradeoff as lazyclaude.

### Build from source

```bash
git clone git@github.com:avalgott/Lazytmux.git
cd Lazytmux
make build                     # -> bin/lazytmux
make install PREFIX=~/.local   # -> ~/.local/bin/lazytmux
make test                      # go test -race -cover ./...
```

Releases are built automatically from `v*` tags via GoReleaser (see `.github/workflows/release.yml`).

### Updating

```bash
lazytmux update    # self-update to the latest release (binary installs)
make update        # git pull + make install (source clones)
```

`lazytmux update` compares the embedded version against the latest GitHub release, downloads the matching prebuilt binary, verifies it against the release's published SHA-256 checksums, and atomically replaces the running one. No repository and no Go needed. Re-running the install one-liner also upgrades in place: it downloads the latest release and verifies the archive against the published checksums before installing.

## Usage

Run `lazytmux` from any shell, inside tmux or outside it:

```bash
tmux new -s devbox     # or create sessions from within lazytmux with `n`
tmux new -s logs
lazytmux               # browse with j/k, Enter opens fullscreen, Ctrl+D returns
```

`Enter` opens the session in fullscreen passthrough mode: the dashboard stays alive and forwards your keystrokes to the session's pane, so `Ctrl+D` returns to the dashboard instantly, no tmux detach needed. `a` performs a real `tmux attach-session` instead, and detaching from it returns you to the dashboard.

### Dashboard keybindings

| Key | Action |
|-----|--------|
| `j` / `k`, `↓` / `↑` | Move the selection (or scroll the preview when it has focus) |
| `Tab` / `Shift+Tab` | Cycle focus between the session list and the preview panel |
| `n` | New session (create dialog) |
| `d` | Kill the selected session (asks for confirmation) |
| `r` | Rename the selected session |
| `Enter` | Open the selected session in fullscreen (passthrough) |
| `a` | Attach for real (`tmux attach-session`) |
| `q` / `Ctrl+C` | Quit |

### Preview panel

Press `Tab` to focus the preview panel (its frame turns cyan). While it has focus, `j`/`k`/`↑`/`↓` scroll line by line, `PgUp`/`PgDn` page by half a screen, `g` jumps to the oldest line, and `G` returns to the live view; the mouse wheel scrolls the preview the same way, anywhere on the dashboard. The history is a frozen snapshot taken on the first scroll, so output produced while browsing does not shift the view. Moving to a different session, resizing the terminal, or entering fullscreen returns the preview to its live capture. Session actions (`n`, `d`, `r`, `Enter`, `a`, `q`) still work while the preview has focus.

### Fullscreen mode

All keystrokes are forwarded to the session's active pane. The screen is a capture refreshed after every key, so line-based programs (shells, REPLs, ssh commands) feel native; full-screen TUI apps (vim, htop) redraw with slight lag. Use `a` for those.

| Key | Action |
|-----|--------|
| `Ctrl+D` | Back to the dashboard |
| `Ctrl+\` | Back to the dashboard |
| `Ctrl+O` | Send a literal Ctrl+D (EOF) to the pane, for exiting shells and REPLs |
| `Ctrl+C` | Forwarded to the pane (interrupts the running program) |
| `Ctrl+V` | Enter scroll mode (browse the pane's history) |
| paste | Forwarded as a bracketed paste |
| everything else | Forwarded to the pane |

### Scroll mode

Inside fullscreen, `Ctrl+V` (or the mouse wheel) switches to scrollback browsing, and the pane's history replaces the live view. Keys are no longer forwarded while browsing. The history is snapshotted once when the mode is entered and browsed in memory, so output produced while browsing does not shift the view; re-enter scroll mode to pick up new output.

| Key | Action |
|-----|--------|
| `j` / `k`, `↓` / `↑` | Scroll line by line |
| `PgUp` / `PgDn` | Scroll half a page |
| `g` / `G` | Jump to the top / the live view |
| mouse wheel | Scroll (enters scroll mode if not active) |
| `Esc` / `q` / `Ctrl+V` | Back to the live view |

Full-screen programs that use the alternate screen (Claude Code, vim, less, htop, and the like) keep no tmux scrollback history, so tmux has nothing for lazytmux to browse. While such a session is selected on the dashboard (or open in fullscreen), lazytmux accumulates its own captures of the pane, up to 400 lines per session, and scroll mode browses that synthetic scrollback instead. Content that appears and disappears between captures (or while the session is not being observed) is not retained. Scrolling an idle session shows a hint instead: if the program tracks mouse input with SGR encoding, the hint invites you to press `Enter` and scroll inside it, where the wheel is forwarded to the program and scrolls its own history; otherwise the panel just says no scrollback is available.

If the session dies while you are in it (for example, the shell exits after `Ctrl+O`), lazytmux returns to the dashboard automatically.

Entering fullscreen resizes the target session's window to fill your terminal, exactly like `tmux attach` does, and keeps it sized while you resize the terminal. Note this also affects any other client attached to that session.

### Dialogs

| Key | Action |
|-----|--------|
| `Enter` | Confirm |
| `Esc` | Cancel |
| `Tab` | Cycle fields (create dialog only) |
| `y` | Confirm the kill (delete dialog only) |

The create dialog has three fields: **Name** (required), **Directory** (optional, pre-filled with the current working directory), and **Command** (optional; leave it empty to start your normal shell). A non-empty command runs inside your shell, so interrupting it with Ctrl+C leaves the session alive with a shell prompt, and when it finishes normally the shell takes over.

## How it works

```
cmd/lazytmux   entry point + dashboard/attach loop
   └── internal/gui       gocui TUI: layout, keybindings, render, preview
        └── internal/session   stateless service (model of session operations)
             └── internal/core/tmux   tmux command abstraction
```

All tmux interaction goes through the tmux CLI: `list-sessions`, `capture-pane`, `send-keys`, `new-session`, `attach-session`, `rename-session`, `kill-session`, `display-message`. tmux is the source of truth, which is why the service holds no state and sessions created anywhere are picked up on the next refresh. Terminal resizes are handled, and the preview cache is invalidated on resize.

The gocui and tcell forks under `third_party/` are vendored (inherited from lazyclaude) to keep lazygit-style rendering.

## Limitations

- The preview shows the active pane of the active window of the selected session, one pane per session.
- Fullscreen passthrough is capture-based: full-screen TUI apps (vim, htop) redraw with noticeable lag and some special key sequences can be lossy. Use `a` (real attach) for those.
- With `a`, attaching from inside tmux takes over the terminal as a new tmux client (tmux has one client per tty), so your original session becomes detached. Detaching from the target returns you to the dashboard on the raw terminal; run `tmux attach` after quitting to get back into your original session.
- Mouse support is limited to wheel scrolling (the preview panel and fullscreen); no config files, no persistence, deliberately out of scope for the MVP.

## License

MIT, see [LICENSE](LICENSE).

### Acknowledgements

lazytmux is adapted from [lazyclaude](https://github.com/any-context/lazyclaude) (MIT, © 2026 KEMSHlM) with all Claude Code-specific behavior stripped out. Thanks to the lazyclaude authors for the TUI structure, the vendored gocui/tcell forks, and the lazygit-inspired rendering.

### Support

If lazytmux makes your terminal workflow a little smoother and you'd like to support ongoing maintenance and updates for this fork, feel free to buy me a coffee!

<a href="https://buymeacoffee.com/avalgott">
  <img src="https://cdn.buymeacoffee.com/buttons/v2/default-yellow.png"
       height="50"
       alt="Buy Me A Coffee">
</a>

Bug reports, feature suggestions, and contributions are always appreciated.
