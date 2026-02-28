package tui

import tea "github.com/charmbracelet/bubbletea"

// PanelRegion stores the screen coordinates of a panel for mouse hit-testing.
type PanelRegion struct {
	X, Y          int
	Width, Height int
}

// Contains returns true if the coordinates are within this region.
func (r PanelRegion) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.Width &&
		y >= r.Y && y < r.Y+r.Height
}

// ContentY returns the y offset within the panel content area (inside borders).
func (r PanelRegion) ContentY(mouseY int) int {
	return mouseY - r.Y - 1 // -1 for top border
}

// isScrollUp returns true if the mouse event is a scroll up.
func isScrollUp(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonWheelUp
}

// isScrollDown returns true if the mouse event is a scroll down.
func isScrollDown(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonWheelDown
}

// isLeftClick returns true if the mouse event is a left click.
func isLeftClick(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress
}
