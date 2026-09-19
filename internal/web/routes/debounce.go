package routes

import (
	"net/http"
	"sync/atomic"
	"time"
)

// callDebouncer bounds how often an endpoint that spends a third-party budget
// actually acts. Three routes need exactly this — /api/monitors/check-now and
// /api/backfill/rescan (a jumpy operator must not spam the platforms) and
// /api/update/check (every call spends one of GitHub's 60/h unauthenticated
// requests) — and they carried two byte-identical copies plus one gap. This is
// the one copy.
//
// Deliberately CAS-free: the worst a race can do is let two callers through
// together, and every caller here is idempotent (the monitors coalesce a kick,
// the backfill sweep dedupes in-flight scans, a double update check costs one
// extra GitHub request). A CAS would buy nothing and read as if it did.
type callDebouncer struct {
	window time.Duration
	// last is the UnixNano of the last ALLOWED call, or 0 before the first.
	// Refused calls never stamp it: a stamping refusal would let a held key
	// hold the window open forever.
	last atomic.Int64
}

func newCallDebouncer(window time.Duration) *callDebouncer {
	return &callDebouncer{window: window}
}

// allow reports whether a call at `now` may proceed, and — when it may not —
// how long the caller should wait. An allowed call stamps the clock.
func (d *callDebouncer) allow(now time.Time) (ok bool, wait time.Duration) {
	n := now.UnixNano()
	prev := d.last.Load()
	if prev != 0 && n-prev < int64(d.window) {
		return false, d.window - time.Duration(n-prev)
	}
	d.last.Store(n)
	return true, 0
}

// writeDebounced writes the answer the three debounced endpoints share:
// 200 with {"success":false,"debounced":true,"retryAfterMs":N}, not a 429 —
// the call is not an error, it is one the server chose not to repeat yet.
// No Retry-After header: the wait is in the body, where the three callers
// (app.js, settings.js twice) already read it.
func writeDebounced(w http.ResponseWriter, wait time.Duration) {
	jsonResponse(w, map[string]any{
		"success":      false,
		"debounced":    true,
		"retryAfterMs": wait.Milliseconds(),
	})
}
