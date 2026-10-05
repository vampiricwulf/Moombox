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
	keyEsc      = "esc"
	keyCtrlC    = "ctrl+c"
	keyCtrlO    = "ctrl+o"
)
