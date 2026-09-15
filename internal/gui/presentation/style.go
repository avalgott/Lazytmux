// Package presentation provides ANSI styling primitives for the TUI.
// Adapted from lazyclaude's presentation package (Claude-specific styles removed).
package presentation

// ANSI style constants.
const (
	Reset = "\x1b[0m"
	Bold  = "\x1b[1m"
	Dim   = "\x1b[2m"
)

// ANSI foreground colors.
const (
	FgRed     = "\x1b[31m"
	FgGreen   = "\x1b[32m"
	FgYellow  = "\x1b[33m"
	FgCyan    = "\x1b[36m"
	FgDimGray = "\x1b[38;5;242m" // dim gray for muted text
)

// Icons used in the UI.
const (
	IconSep      = "│"
	IconAttached = "●"
)

// StyledKey renders a keybinding hint: key in bold, description in dim.
// Example: StyledKey("n", "new") => "\x1b[1mn\x1b[0m\x1b[2m:new\x1b[0m"
func StyledKey(key, desc string) string {
	return Bold + key + Reset + Dim + ":" + desc + Reset
}
