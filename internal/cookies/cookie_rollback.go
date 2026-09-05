package cookies

import (
	"fmt"
	"strings"
)

// loadCookieJar reloads the jar from path. It is CookieJar.Load behind a
// package-level seam — the same shape, and for the same reason, as
// applyUserOnlyDACL: a test can make it fail and drive the one exit where
// cookies.txt is correct and the running process is not.
//
// Only the reload that follows a RESTORE goes through it. The ordinary
// post-write reloads on both paths stay direct calls, so a test that breaks
// this one still reaches the rollback it wants to observe.
//
// It lives HERE rather than with cookie_files.go's seams because that is what
// it is for: those are thin stand-ins for utils file operations, this one
// exists for the rollback exit below and its doc is that exit's argument.
var loadCookieJar = func(s *AutoCookieService, path string) error { return s.jar.Load(path) }

// The half of each sentence that is a fact about the INSTALL rather than about
// the caller: which generation of credentials the next download will use. Both
// paths reached the same two states and said the same two things about them,
// so they are said once, here.
const (
	rollbackWriteFailedTail  = " — cookies.txt still holds the rejected new credentials"
	rollbackReloadFailedTail = " — this process is still using the rejected credentials until the next refresh"

	// The log line for a failed reload is identical on both paths; the one for
	// a failed write is not (the import names the rejected import), so that one
	// arrives in rollbackMessages.
	rollbackReloadFailedLog = "could not reload cookie jar after restoring the previous cookies.txt"
)

// rollbackStage names which half of a restore failed. The two are different
// states of the install and callers word them differently: after
// rollbackWriteFailed the FILE holds the rejected credentials, after
// rollbackReloadFailed the file is right and the jar is stale.
type rollbackStage int

const (
	rollbackWriteFailed rollbackStage = iota + 1
	rollbackReloadFailed
)

// rollbackMessages is the wording each caller owns. Everything else about the
// two rollback blocks was identical; these four fields are the whole of the
// difference, and they are passed in rather than folded together because every
// one of them is a truthful phrase about a different mechanism — a paste the
// operator supplied, or a browser profile that was read.
//
// The heads are sentence FRAGMENTS: restorePreviousCookies appends the cause in
// parentheses and then the shared tail.
type rollbackMessages struct {
	// sentinel, when non-nil, is wrapped into the failure error and prefixed to
	// its text, so a caller whose route matches on a sentinel (the import path's
	// ErrImportRollbackIncomplete) keeps both properties. nil leaves the failure
	// error wrapping the cause alone, which is what the browser path built.
	sentinel error

	writeHead  string
	reloadHead string

	// writeLog is the logger.Error message for a failed write-back. The reload's
	// is rollbackReloadFailedLog on both paths.
	writeLog string
}

// rollbackFailure reports a restore that did not land. The sentence has already
// been recorded via setError and logged with the platforms by the time a caller
// sees this; what is left is the value the caller returns, which the two paths
// word differently — the import returns err (the sentinel-wrapped sentence, so
// the route and the dialog render the same truth), the browser refresh returns
// its own short wrapper around cause.
type rollbackFailure struct {
	stage rollbackStage
	err   error
	cause error
}

// restorePreviousCookies writes restored over cookies.txt and reloads the jar,
// and reports either half failing.
//
// A rollback that does not land must not be reported as one: both failures
// leave the process describing credentials that are not the ones in force, and
// a caller that carried on would tell the operator their previous cookies were
// kept while the rejected ones are what the next download uses. So both set
// lastError — unlike a rejected paste, this IS a state of the install — and
// both end the caller's pass.
//
// platforms are the lowercase platform keys for the log fields; the
// operator-facing names are already inside msgs, because the two callers
// capitalise them differently.
func (s *AutoCookieService) restorePreviousCookies(restored string, platforms []string, msgs rollbackMessages) *rollbackFailure {
	fail := func(stage rollbackStage, head, tail, logMsg string, cause error) *rollbackFailure {
		var failure error
		if msgs.sentinel != nil {
			failure = fmt.Errorf("%w: %s (%w)%s", msgs.sentinel, head, cause, tail)
		} else {
			failure = fmt.Errorf("%s (%w)%s", head, cause, tail)
		}
		s.setError(failure.Error())
		s.logger.Error(logMsg, "err", cause, "platforms", strings.Join(platforms, ","))
		return &rollbackFailure{stage: stage, err: failure, cause: cause}
	}

	if restoreErr := writeCookieFile(s.cookiePath, []byte(restored), 0o600); restoreErr != nil {
		return fail(rollbackWriteFailed, msgs.writeHead, rollbackWriteFailedTail, msgs.writeLog, restoreErr)
	}
	if loadErr := loadCookieJar(s, s.cookiePath); loadErr != nil {
		return fail(rollbackReloadFailed, msgs.reloadHead, rollbackReloadFailedTail, rollbackReloadFailedLog, loadErr)
	}
	return nil
}
