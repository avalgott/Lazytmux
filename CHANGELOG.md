# Changelog
---

## [v0.4.0] - 2026-09-19

### Summary

The headline feature is Session Plans: describe your whole development setup in
one YAML file and run `lazytmux -p myapp` to have every session ready. Planned
sessions are marked with `●` and protected from accidental renaming or deletion,
and fullscreen mode shows what each planned session is supposed to be running.

### Added

- **Session Plans**: YAML plans under `lazytmux/plans/` in the user config directory declare the named
  sessions of a development workflow. Plans are strictly validated before anything
  is created (schema, session names, paths must exist, plan `name` must match the
  requested one).
- `-p <name>` / `--plan <name>` loads a plan at startup and creates the missing
  tmux sessions. Idempotent: existing sessions are never touched, creation failures
  are all reported (successes are kept), and tmux remains the source of truth.
- Planned sessions are marked with a dim `●` in the session list and cannot be
  renamed or deleted while the plan is active (a log message explains why).
- Fullscreen mode shows a `Command` panel with the plan-configured command (wrapped
  to at most three rows) for planned sessions that define one.
- Session names with shell metacharacters are now rejected by the create/rename
  dialogs up front instead of failing later.

### Changed

- A never-started tmux server is treated as an empty session list, like a stopped
  one, so the first `-p` run on a cold machine works.

---
## [v0.3.0] - 2026-09-19

### Summary

The dashboard preview is now scrollable: focus it with `Tab` and browse a
session's history without leaving the dashboard.

### Added

- Scrollable dashboard preview: `Tab` focuses the preview panel and browses a
  frozen snapshot of the session history with `j`/`k`, `PgUp`/`PgDn`, `g`/`G`,
  and the mouse wheel.
---
## [v0.2.0] - 2026-09-16

### Summary

lazytmux can now update itself (`lazytmux update`), and fullscreen mode lets you
scroll through a session's history. Several installer and updater issues were
also fixed.

### Added

- `lazytmux update`: self-update from the latest GitHub release with checksum
  verification of the downloaded binary.
- Fullscreen scroll mode (`Ctrl+V`): browse the session's history right in
  fullscreen; the mouse wheel scrolls it too.
- Installer verifies release tarballs against the published checksums before
  installing.

### Fixed

- Installer fallback now clones and builds from source instead of failing on
  `go install`.
- Fullscreen interaction bugs and session leaks during scrollback capture.
- Updater build-metadata parsing and prerelease comparison.

### Changed

- Command sessions reliably use your current shell, even with a long-running
  tmux server.

---
## [v0.1.0] - 2026-09-15

### Summary

First release: a dashboard for all your tmux sessions, browse them, watch what
they're doing live, create and remove them, and jump into any of them.

### Added

- Initial release: dashboard TUI listing every tmux session with a live ANSI
  preview, session create/rename/delete dialogs, fullscreen passthrough mode,
  and real `tmux attach-session` support.
- `install.sh` one-line installer and GoReleaser setup for `v*` tags.

[v0.4.0]: https://github.com/avalgott/Lazytmux/compare/v0.3.0...HEAD
[v0.3.0]: https://github.com/avalgott/Lazytmux/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/avalgott/Lazytmux/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/avalgott/Lazytmux/releases/tag/v0.1.0
