// Package jobstate derives the sub-states a job's persisted row implies but
// does not name: the automatic action the Twitch monitor will take for an
// Error row (PendingAutoAction), the progress line of a job that is waiting
// rather than delivering (WaitLine, IsWaitLine), why a Queued backlog row is
// not being admitted (AdmissionHolds.For), and how many automatic cookie
// retries a job parked in COOKIES? has used (AdmissionHolds.CookieRetryFor). Each is derived, never
// persisted (audit rule 5: "a sub-state that changes what the operator should
// do is derived, never persisted, and shown in both UIs").
//
// A leaf package (it imports only internal/database), like internal/jobverbs,
// so the worker that produces these states, the REST layer that sends them
// and internal/tui, which does not import internal/worker, all call the same
// functions. The dashboard mirrors them in web/public/modules/utils.js, and
// cmd/moombox's job-state parity test runs both against one shared table
// (web/tests/fixtures/job-state-parity.json).
package jobstate
