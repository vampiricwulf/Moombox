// Package keepawake keeps the host from going to sleep on its idle timer while
// Moombox has work that sleep would end. A capture produces no keyboard or
// mouse input, and network and disk activity do not reset Windows' idle timer,
// so a PC whose power plan allows sleep suspended Moombox mid-recording: the
// capture stopped, and nothing failed that either UI could report.
//
// Only Windows is covered (SetThreadExecutionState). Elsewhere the Holder does
// nothing: a Linux host that runs Moombox is a server or a container, and its
// sleep policy belongs to systemd.
package keepawake

// Holder asks the operating system to stay awake while held. The screen may
// still turn off: only system sleep is held back. Its methods are safe for
// concurrent use.
type Holder struct {
	impl holderImpl
}

// New returns a Holder that holds nothing yet.
func New() *Holder {
	return &Holder{impl: newHolderImpl()}
}

// Supported reports whether holding does anything on this operating system.
func Supported() bool { return supported }

// Hold asks for the host to stay awake (true) or lets it sleep again (false).
// Repeating the current state is harmless. It reports whether the operating
// system took the request; always false where Supported is false, and after
// Close.
func (h *Holder) Hold(on bool) bool {
	return h.impl.hold(on)
}

// Close lets the host sleep again and ends the Holder. The request also ends
// with the process, however it exits.
func (h *Holder) Close() {
	h.impl.close()
}
