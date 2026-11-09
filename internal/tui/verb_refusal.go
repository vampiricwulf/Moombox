package tui

// RefusalError marks a verb callback's error as a refusal — the worker
// declining the verb for the job's state (the row moved to a status the verb
// is not offered on, another operation holds its staging, the row is gone) —
// rather than a fault while it ran. The feedback line shows a refusal's text
// as it is, the same text REST answers with its 409 or 404, and prefixes only
// a fault. The wiring wraps the worker's errors in it, since this package
// does not import the worker.
type RefusalError struct{ Err error }

func (r RefusalError) Error() string { return r.Err.Error() }

// Unwrap keeps the worker's sentinel reachable through errors.Is.
func (r RefusalError) Unwrap() error { return r.Err }
