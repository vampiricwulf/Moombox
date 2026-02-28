package tui

import (
	"github.com/charmbracelet/bubbletea"
)

// Key constants for key bindings.
const (
	keyTab    = "tab"
	keyUp     = "up"
	keyDown   = "down"
	keyPgUp   = "pgup"
	keyPgDown = "pgdown"
	keyHome   = "home"
	keyEnd    = "end"
	keyEnter  = "enter"
	keyEsc    = "esc"
	keyQ      = "q"
	keyCtrlC  = "ctrl+c"
)

// isQuit returns true if the key message is a quit key.
func isQuit(msg tea.KeyMsg) bool {
	return msg.String() == keyQ || msg.String() == keyCtrlC
}
