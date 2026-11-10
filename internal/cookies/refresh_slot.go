package cookies

import "os"

// refreshHolder is who holds the refresh slot. The setup slot is separate
// and not modelled here (its own fields on AutoCookieService).
type refreshHolder uint8

const (
	// refreshHolderNone: the slot is free.
	refreshHolderNone refreshHolder = iota
	// refreshHolderRefresh: a RefreshCookies pass, from its claim to its
	// outer defer — the launch, the browser's run and the critical tail
	// (merge → atomic write → jar reload → auth verify → meta save).
	refreshHolderRefresh
	// refreshHolderImport: an ImportCookies call (owner decision O-D). It
	// launches no browser, so the slot never publishes a process for it.
	refreshHolderImport
)

// String is the holder as the declining pass's Info line names it.
func (h refreshHolder) String() string {
	switch h {
	case refreshHolderRefresh:
		return "refresh pass"
	case refreshHolderImport:
		return "cookie import"
	case refreshHolderNone:
		return "none"
	}
	return "unknown"
}

// refreshSlot is the single-flight between a refresh pass, an import and a
// setup start (StartSetup refuses while it is held), guarded by
// AutoCookieService.mu. It replaced a *exec.Cmd whose identity carried the
// holder (a package-level import sentinel against a fresh &exec.Cmd{}) and
// whose .Process was the browser to kill.
//
// proc is the launched refresh browser while — and only while — it may still
// be running: published once its process exists and withdrawn once it is
// reaped, because a reaped PID can be recycled onto an unrelated process and
// Stop's killRefreshProcess must never taskkill /F /T that. The one exit
// that deliberately leaves it published is runWithTimeout's kill that never
// observed a reap (see its note).
type refreshSlot struct {
	holder refreshHolder
	proc   *os.Process
}

// held reports whether anyone holds the slot.
func (r *refreshSlot) held() bool { return r.holder != refreshHolderNone }

// claim takes the free slot for h. The caller checked held() under the same
// lock.
func (r *refreshSlot) claim(h refreshHolder) { *r = refreshSlot{holder: h} }

// publish names the refresh browser process Stop may kill.
func (r *refreshSlot) publish(p *os.Process) { r.proc = p }

// unpublish withdraws the process once it is reaped. The holder keeps the
// slot: the refresh's critical tail still has to run.
func (r *refreshSlot) unpublish() { r.proc = nil }

// release frees the slot (the holder's outer defer).
func (r *refreshSlot) release() { *r = refreshSlot{} }

// inProgressError is the refusal for a held slot: ErrImportInProgress when an
// import holds it, else ErrRefreshInProgress. Either reads "please try again
// shortly", and both match ErrRefreshInProgress, which the routes map to 409.
func (r *refreshSlot) inProgressError() error {
	if r.holder == refreshHolderImport {
		return ErrImportInProgress
	}
	return ErrRefreshInProgress
}
