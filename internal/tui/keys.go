package tui

// Key constants for key bindings.
const (
	keyTab = "tab"
	// keyShiftTab cycles panel focus backwards; the spec has always listed it
	// beside Tab, and until it was handled it was a silent no-op.
	keyShiftTab = "shift+tab"
	keyUp       = "up"
	keyDown     = "down"
	keyLeft     = "left"
	keyRight    = "right"
	keyPgUp     = "pgup"
	keyPgDown   = "pgdown"
	keyHome     = "home"
	keyEnd      = "end"
	keyEnter    = "enter"
	// keySpace is what a Space keypress stringifies to in bubbletea v2
	// (KeyPressMsg{Code: KeySpace, Text: " "}.String()) — never " ". The two
	// keys that bind Space matched " ", which no terminal sends, so ticking a
	// task for a batch and every "Space to toggle" in the notification
	// editor did nothing.
	keySpace = "space"
	keyEsc   = "esc"
	keyCtrlC = "ctrl+c"
	keyCtrlO = "ctrl+o"
)
