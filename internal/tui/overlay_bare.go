package tui

import "fmt"

// Below the normal layout an overlay with a bare form is drawn bare: no
// border, no centring margin, a one-row header, only the content and keys the
// job needs (owner rulings R-C10 to R-C13). One without a bare form, or whose
// current state needs more room than the terminal has, is kept under the
// notice (heldOverlayView), and an action that would open it is refused; the
// notice and the refusal name the size that overlay needs (owner rulings
// R-C5 and R-C6 as amended).
//
// The answer is overlayDrawFor for an overlay that is open and
// overlayOpensHere for one that is not. Both are derived from the terminal
// size and the overlay's state on every call and never stored. View,
// handleKey, handleMouse, screenHeld, the refusals, the chord hint and Help
// all ask them; nothing else compares a size against an overlay.
//
// A bare view is drawn by the overlay's own model, which branches on
// layoutModeFor of its own size in the first statement of View and SetSize.
// It must be exactly width x height cells at every compact size at or above
// its minimum, wide-short (200x8) and narrow-tall (30x50) included.

// overlayBare is an overlay's bare form: the smallest terminal it draws in.
type overlayBare struct {
	// openW and openH are the size needed to open it (its first screen).
	openW, openH int
	// now is the size its current state needs. nil: every state fits
	// openW x openH.
	now func(*App) (w, h int)
}

// overlayDraw is how an open overlay is drawn at the current size.
type overlayDraw int

const (
	// drawNormal: the normal layout, the overlay's bordered view.
	drawNormal overlayDraw = iota
	// drawBare: the compact layout, the overlay's bare view.
	drawBare
	// drawHeld: the overlay is kept under the notice.
	drawHeld
)

// openNeed is the smallest terminal o opens in below the normal layout: its
// bare form's, or the normal layout's when it has none.
func (o *overlayEntry) openNeed() (w, h int) {
	if o.bare == nil {
		return minTermWidth, minTermHeight
	}
	return o.bare.openW, o.bare.openH
}

// overlayNeed is the smallest terminal o's current state is drawn in below
// the normal layout.
func (a *App) overlayNeed(o *overlayEntry) (w, h int) {
	if o.bare != nil && o.bare.now != nil {
		return o.bare.now(a)
	}
	return o.openNeed()
}

// overlayDrawFor is how o, an open overlay, is drawn right now.
func (a *App) overlayDrawFor(o *overlayEntry) overlayDraw {
	switch a.layoutMode() {
	case layoutNormal:
		return drawNormal
	case layoutCompact:
		if w, h := a.overlayNeed(o); o.bare != nil && a.width >= w && a.height >= h {
			return drawBare
		}
	}
	return drawHeld
}

// overlayEntryNamed is the registry entry called name, nil when there is none.
func overlayEntryNamed(name string) *overlayEntry {
	for i := range overlayRegistry {
		if overlayRegistry[i].name == name {
			return &overlayRegistry[i]
		}
	}
	return nil
}

// overlayOpensAt reports whether the overlay called name, not yet open, can
// be opened in a terminal of width x height, and the size it needs when it
// cannot. A name that is no registry entry needs the normal layout.
func overlayOpensAt(name string, width, height int) (ok bool, needW, needH int) {
	needW, needH = minTermWidth, minTermHeight
	if o := overlayEntryNamed(name); o != nil {
		needW, needH = o.openNeed()
	}
	switch layoutModeFor(width, height) {
	case layoutNormal:
		return true, needW, needH
	case layoutCompact:
		return width >= needW && height >= needH, needW, needH
	}
	return false, needW, needH
}

// overlayOpensHere is overlayOpensAt for the terminal's current size.
func (a *App) overlayOpensHere(name string) (ok bool, needW, needH int) {
	return overlayOpensAt(name, a.width, a.height)
}

// compactRefusal is the feedback line an action gets when the overlay it
// opens needs a terminal of w x h and this one is smaller (owner ruling
// R-C5).
func compactRefusal(w, h int) string {
	return fmt.Sprintf("Enlarge the terminal to %dx%d for this", w, h)
}

// heldOverlayNotice is the first line of the notice over an overlay kept
// open in a terminal smaller than the w x h it needs (owner ruling R-C6).
func heldOverlayNotice(w, h int) string {
	return fmt.Sprintf("Enlarge to %dx%d to continue", w, h)
}
