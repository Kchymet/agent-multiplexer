// Package termbg stops Bubble Tea from querying the terminal's background
// color at program start. Bubble Tea v1's package init calls
// lipgloss.HasDarkBackground, which writes OSC 11 and DSR queries to the
// terminal and blocks up to termenv's 5s OSCTimeout for the answers. Every amux
// command with a terminal on stdout paid that, including the many that draw
// nothing, and it is the full 5s wherever nothing answers (script(1), a pty
// driven by another program, some serial or remote consoles).
//
// amux renders no adaptive (background-dependent) colors, so the answer is
// never used. Settling it explicitly here, before Bubble Tea's init runs, makes
// that init's query a no-op. Go initializes ready packages in import-path
// order, and this package's path sorts before github.com/charmbracelet/..., so
// importing it anywhere in the binary is enough.
package termbg

import "github.com/charmbracelet/lipgloss"

func init() { lipgloss.SetHasDarkBackground(true) }
