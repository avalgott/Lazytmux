# lazytmux

A [lazygit](https://github.com/jesseduffield/lazygit)-style TUI for managing [tmux](https://github.com/tmux/tmux) sessions: browse every session, watch a live preview of it, and attach — all from one screen.

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

- **All sessions, automatically** — every session on the default tmux server is listed, including ones you created with plain `tmux new -s name`.
- **Live preview** — the right panel mirrors the selected session's active pane, ANSI colors preserved and cropped to fit, refreshed about twice a second.
- **Fullscreen passthrough** — `Enter` expands the selected session to the full terminal and forwards every keystroke to it; `Ctrl+D` returns to the dashboard instantly.
- **Real attach too** — `a` attaches with `tmux attach-session` (the right tool for vim, htop, and other full-screen TUI apps); detaching returns you to the dashboard.
- **Session lifecycle** — create, rename, and kill sessions from the TUI.
- **Zero configuration** — no config files, no separate tmux socket, no state on disk.
- **Works with anything** — shells, SSH, database CLIs, log tails, dev servers, vim, htop. There is no editor-, agent- or tool-specific behavior.

## Requirements

- tmux (any modern 3.x), installed and on `PATH`
- Go 1.25+ (build only)

## Installation

### Quick install (standalone binary)

```bash
curl -fsSL https://raw.githubusercontent.com/avalgott/Lazytmux/main/install.sh | sh
```

Downloads the pre-built binary from the latest GitHub release to `~/.local/bin/` — no Go required. Before the first release has been cut, the script falls back to cloning and building from source (requires git and Go 1.25+). The result is the same: `lazytmux` on your PATH.

> Note: plain `go install ...@latest` is not supported — the vendored TUI forks (gocui/tcell) use relative `replace` directives, which the Go module proxy rejects. Same tradeoff as lazyclaude.

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

`lazytmux update` compares the embedded version against the latest GitHub release, downloads the matching prebuilt binary, and atomically replaces the running one — no repository or Go needed. Re-running the install one-liner does the same thing.

## Usage

Run `lazytmux` from any shell — inside tmux or outside it:

```bash
tmux new -s devbox     # or create sessions from within lazytmux with `n`
tmux new -s logs
lazytmux               # browse with j/k, Enter opens fullscreen, Ctrl+D returns
```

`Enter` opens the session in fullscreen passthrough mode: the dashboard stays alive and forwards your keystrokes to the session's pane, so `Ctrl+D` returns to the dashboard instantly — no tmux detach needed. `a` performs a real `tmux attach-session` instead; detaching from it returns you to the dashboard.

### Dashboard keybindings

| Key | Action |
|-----|--------|
| `j` / `k`, `↓` / `↑` | Move the selection |
| `n` | New session (create dialog) |
| `d` | Kill the selected session (asks for confirmation) |
| `r` | Rename the selected session |
| `Enter` | Open the selected session in fullscreen (passthrough) |
| `a` | Attach for real (`tmux attach-session`) |
| `q` / `Ctrl+C` | Quit |

### Fullscreen mode

All keystrokes are forwarded to the session's active pane. The screen is a capture refreshed after every key, so line-based programs (shells, REPLs, ssh commands) feel native; full-screen TUI apps (vim, htop) redraw with slight lag — use `a` for those.

| Key | Action |
|-----|--------|
| `Ctrl+D` | Back to the dashboard |
| `Ctrl+\` | Back to the dashboard |
| `Ctrl+O` | Send a literal Ctrl+D (EOF) to the pane — for exiting shells and REPLs |
| `Ctrl+C` | Forwarded to the pane (interrupts the running program) |
| `Ctrl+V` | Enter scroll mode (browse the pane's history) |
| paste | Forwarded as a bracketed paste |
| everything else | Forwarded to the pane |

### Scroll mode

Inside fullscreen, `Ctrl+V` (or the mouse wheel) switches to scrollback browsing — the pane's history replaces the live view. Keys are no longer forwarded while browsing.

| Key | Action |
|-----|--------|
| `j` / `k`, `↓` / `↑` | Scroll line by line |
| `PgUp` / `PgDn` | Scroll half a page |
| `g` / `G` | Jump to the top / the live view |
| mouse wheel | Scroll (enters scroll mode if not active) |
| `Esc` / `q` / `Ctrl+V` | Back to the live view |

If the session dies while you are in it (e.g. the shell exits after `Ctrl+O`), lazytmux returns to the dashboard automatically.

Entering fullscreen resizes the target session's window to fill your terminal (exactly like `tmux attach` does), and keeps it sized while you resize the terminal. Note this also affects any other client attached to that session.

### Dialogs

| Key | Action |
|-----|--------|
| `Enter` | Confirm |
| `Esc` | Cancel |
| `Tab` | Cycle fields (create dialog only) |
| `y` | Confirm the kill (delete dialog only) |

The create dialog has three fields: **Name** (required), **Directory** (optional, pre-filled with the current working directory), and **Command** (optional — leave it empty to start your normal shell). A non-empty command runs inside your shell, so interrupting it with Ctrl+C leaves the session alive with a shell prompt, and when it finishes normally the shell takes over.

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

- The preview shows the active pane of the active window of the selected session — one pane per session.
- Fullscreen passthrough is capture-based: full-screen TUI apps (vim, htop) redraw with noticeable lag and some special key sequences can be lossy. Use `a` (real attach) for those.
- With `a`, attaching from inside tmux takes over the terminal as a new tmux client (tmux has one client per tty), so your original session becomes detached. Detaching from the target returns you to the dashboard on the raw terminal — run `tmux attach` after quitting to get back into your original session.
- Mouse support is limited to wheel scrolling in fullscreen; no config files, no persistence — deliberately out of scope for the MVP.

## License

MIT — see [LICENSE](LICENSE).

### Acknowledgements

lazytmux is adapted from [lazyclaude](https://github.com/any-context/lazyclaude) (MIT, © 2026 KEMSHlM) with all Claude Code-specific behavior stripped out. Thanks to the lazyclaude authors for the TUI structure, the vendored gocui/tcell forks, and the lazygit-inspired rendering.
