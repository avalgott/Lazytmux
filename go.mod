module github.com/avalgott/Lazytmux

go 1.25.0

replace github.com/jesseduffield/gocui => ./third_party/gocui

replace github.com/gdamore/tcell/v2 => ./third_party/tcell

require (
	github.com/charmbracelet/x/ansi v0.11.6
	github.com/jesseduffield/gocui v0.3.1-0.20260308162933-5e45e57b5564
	github.com/stretchr/testify v1.11.1
	golang.org/x/sys v0.44.0
	golang.org/x/term v0.38.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/gdamore/encoding v1.0.1 // indirect
	github.com/gdamore/tcell/v2 v2.13.5 // indirect
	github.com/go-errors/errors v1.0.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	golang.org/x/text v0.39.0 // indirect
)
