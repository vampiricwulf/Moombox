package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// prepareCookieImport validates an operator-supplied Netscape cookie file and
// returns the exact text that must be written to cookies.txt.
//
// PURE: no I/O, no clock of its own, no service state. Its caller
// (AutoCookieService.ImportCookies) owns the read, the write and the jar
// reload; everything decidable from the two strings is decided here, so the
// rules can be table-tested without a filesystem.
//
// MERGE, NEVER REPLACE (spec R3). A YouTube-only paste must leave every Twitch
// row exactly as it was: blind replacement would destroy the sibling platform's
// credentials silently, and the operator's next Twitch capture would fail for
// an unrelated-looking reason. mergeCookieFiles is the merge — name+domain+path
// keyed, verified tab-safe, and already the merge every other writer uses.
//
// The empty-value filter runs TWICE and both runs are load-bearing. See
// stripEmptyValuedRows for what an empty-valued row does to the jar.
func prepareCookieImport(existing, incoming string) (string, error) {
	cleaned, kept, dropped, unparseable := cleanNetscapeRows(incoming)
	switch {
	case kept == 0 && dropped == 0 && unparseable > 0:
		// Not one line looked like a cookie row.
		return "", ErrImportNotNetscape
	case kept == 0 && dropped == 0:
		// Comments and blanks only. An empty body never reaches here — the
		// route refuses that as a request-shape problem, which keeps this
		// message honest about what it describes.
		return "", ErrImportNoRows
	}

	// Rows that ARE structurally cookie rows but carry no login. Asked of the
	// throwaway jar rather than by matching names against the text, so the
	// domain routing, the name admission and the #HttpOnly_ handling have ONE
	// reading in this package. A paste that was all dropped rows lands here too
	// and gets this answer, correctly: it was a cookie file and none of it was
	// usable.
	if !netscapeCookiesHoldACredential(cleaned) {
		return "", ErrImportNoCredential
	}

	return stripEmptyValuedRows(mergeCookieFiles(existing, cleaned)), nil
}

// cleanNetscapeRows splits the submitted text into the data rows worth merging
// and counts what it threw away.
//
// The three counters are three because the caller's answer differs. `kept` and
// `dropped` both prove the text IS a cookie file; only `unparseable` on its own
// means it is not. Folding dropped into either neighbour would report a
// well-formed export whose values are blank as "not a Netscape file".
//
// CRLF is normalised here and nowhere else. Every browser extension on Windows
// exports CRLF, and mergeCookieFiles carries a row VERBATIM — so a stray \r
// would ride into cookies.txt on the end of a value, where CookieJar.Load's
// TrimSpace hides it from this process and the next writer propagates it.
func cleanNetscapeRows(incoming string) (cleaned string, kept, dropped, unparseable int) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(incoming, "\r\n", "\n"), "\r", "\n")
	var rows []string
	for line := range strings.SplitSeq(normalized, "\n") {
		if !isNetscapeDataRow(line) {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
		if len(fields) < 7 {
			unparseable++
			continue
		}
		if strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[5]) == "" || netscapeRowValue(line) == "" {
			dropped++
			continue
		}
		rows = append(rows, line)
		kept++
	}
	return strings.Join(rows, "\n"), kept, dropped, unparseable
}

// isNetscapeDataRow reports whether a line carries cookie data. `#HttpOnly_`
// lines are DATA despite the leading '#'; every other comment and every blank
// line is not. Same rule as CookieJar.loadFrom, countNetscapeCookieRows and
// mergeCookieFiles' own parser — a fourth reading of it is how they drift.
func isNetscapeDataRow(line string) bool {
	if strings.HasPrefix(line, "#HttpOnly_") {
		return true
	}
	if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
		return false
	}
	return true
}

// netscapeRowValue returns a data row's VALUE, or "" for a row too short to
// have one.
//
// Fields 6.. are re-JOINED, exactly as CookieJar.loadFrom does, so a value that
// legitimately contains a tab reads as itself rather than as a truncation.
func netscapeRowValue(line string) string {
	fields := strings.Split(strings.TrimPrefix(line, "#HttpOnly_"), "\t")
	if len(fields) < 7 {
		return ""
	}
	return strings.TrimSpace(strings.Join(fields[6:], "\t"))
}

// stripEmptyValuedRows removes every data row whose value is empty, preserving
// comments, blanks and row order.
//
// THE TRAP, in full, because it is invisible from the file: CookieJar.Load
// TrimSpaces the line, so the trailing tab of `domain … NAME \t` disappears,
// the row reads as SIX fields and Load skips it. The credential is therefore
// absent from the jar while the row sits in cookies.txt forever — and no writer
// prunes it, because mergeCookieFiles prunes on EXPIRY and this row has a
// perfectly good expiry.
//
// Run on the OUTPUT, so an import repairs rows an older writer already left
// behind. cleanNetscapeRows runs the same rule on the INPUT, and that one is
// the guard that matters most: without it an empty-valued row in a paste wins
// by name+domain+path over a working credential on disk.
func stripEmptyValuedRows(netscape string) string {
	lines := strings.Split(netscape, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if isNetscapeDataRow(line) && netscapeRowValue(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// ImportResult reports what ONE operator-supplied cookie import concluded, per
// platform.
//
// Field for field this is SetupResult's shape, and deliberately so: an import
// is an acquisition with a per-platform verdict, exactly like a wizard finish,
// and cookieImportOutcome renders the same key set cookieSetupOutcome does so
// the dashboard reuses the copy helpers rather than inventing a fourth phrasing
// of the same three states. It is a distinct TYPE because SetupResult's doc
// says what one INTERACTIVE SETUP concluded, and an import is not a setup —
// there is no browser, no slot and no cancel.
//
// The two facts per platform can disagree, which is the whole reason both are
// here: see credentialAccepted for why an inconclusive check still accepts a
// credential the operator just supplied.
type ImportResult struct {
	// What the auth check CONCLUDED, in the three-way vocabulary a refresh pass
	// uses. RefreshUnknown is the zero value on purpose, so any exit that
	// returns without checking cannot accidentally assert health or failure.
	YouTube RefreshVerdict
	Twitch  RefreshVerdict

	YouTubeAccepted bool
	TwitchAccepted  bool

	// Wrote reports that this import REPLACED cookies.txt, and it is true on
	// one error path as well as on success — the jar reload after a successful
	// write can fail, and that exit returns an error over a file that has
	// already been replaced. Its caller is the route's deferred re-check, which
	// must fire on exactly that case: it is where a re-check is worth most,
	// because refresh's own jar.Reload repairs the stale in-memory jar the
	// error left behind. Same contract, same caller, as SetupResult.Wrote.
	Wrote bool
}

// ImportCookies merges an operator-supplied Netscape cookie file into
// cookies.txt, reloads the jar and verifies what it just installed.
//
// THE FIFTH WRITER of cookies.txt, and it inherits Arc 2's catalogue whole: the
// read goes through the readCookieFile seam and distinguishes "does not exist"
// from every other error, the merge goes through mergeCookieFiles, the write
// goes through writeCookieFile, and no empty-valued row is ever produced (see
// prepareCookieImport).
//
// NOT gated on the `stopped` latch, unlike StartSetup and
// RefreshCookiesDetailed. Both of those refusals are about launching or
// steering a browser PROCESS; this launches nothing, and refusing an import
// during a drain would throw away credentials the operator supplied by hand,
// for the sake of a shutdown that is about to read the file back on the next
// start anyway.
//
// The caller runs the auth re-check. Every gesture that can write cookies.txt
// must end in one (Arc 10 R4) and this one's caller lives in
// internal/web/routes, which — like the two setup-wizard finishes — runs it
// itself rather than through the OnPassCompleted seam. Firing that seam here
// would double every external site; see its doc comment.
func (s *AutoCookieService) ImportCookies(ctx context.Context, netscape string) (ImportResult, error) {
	if s.cookiePath == "" {
		// Unreachable in production — cmd/moombox always constructs the service
		// with cookies.cookie_file, which config defaults to ./cookies.txt —
		// but writing to "" would otherwise produce a temp file in the process
		// working directory and a rename onto nothing.
		return ImportResult{}, errors.New("no cookie file is configured — set cookies.cookie_file")
	}

	var existing string
	data, readErr := readCookieFile(s.cookiePath)
	switch {
	case readErr == nil:
		existing = string(data)
	case errors.Is(readErr, fs.ErrNotExist):
		// No cookies.txt yet — first acquisition. Nothing to merge, nothing to
		// protect.
	default:
		// Abort BEFORE anything is written. A transient read failure is NOT
		// "no existing file": the unreadable file may hold working credentials
		// for a platform this paste never mentions, and proceeding as if it
		// were absent would replace it with the paste alone.
		//
		// Wrapped so the route can tell this apart from every other import
		// failure — the operator must be told to fix the permission or the
		// mount, and NEVER to replace the file Moombox just went out of its way
		// not to destroy.
		mergeErr := fmt.Errorf("%w — refusing to merge or overwrite an existing cookies.txt that could not be read (%w)",
			ErrCookieFileUnreadable, readErr)
		s.setError(mergeErr.Error())
		s.logger.Error("cookie import: aborting rather than overwrite cookies.txt after a read failure",
			"path", s.cookiePath, "err", readErr)
		return ImportResult{}, mergeErr
	}

	// No setError on this exit, deliberately, and it is the same rule that
	// keeps FinishSetupDetailed's two guard clauses from setting: a rejected
	// paste is not a state of the INSTALL. Nothing ran, nothing was written,
	// and the caller renders the refusal synchronously in the dialog that
	// produced it — while lastError renders in Settings as a standing "your
	// recordings will fail" that no later success clears.
	merged, err := prepareCookieImport(existing, netscape)
	if err != nil {
		return ImportResult{}, err
	}

	if err := os.MkdirAll(filepath.Dir(s.cookiePath), 0o755); err != nil {
		s.setError("could not create the directory for cookies.txt: " + err.Error())
		return ImportResult{}, err
	}
	if err := writeCookieFile(s.cookiePath, []byte(merged), 0o600); err != nil {
		// The hint names the ONE deployment mistake that produces this, and is
		// kept SHORT because it goes to a status line both dashboards render —
		// the same split FinishSetupDetailed's failed write already makes
		// against refresh.go's log-bound paragraph.
		wrErr := fmt.Errorf("%w (%w) — if this is Docker, mount the data directory rather than cookies.txt itself",
			ErrCookieFileUnwritable, err)
		s.setError(wrErr.Error())
		return ImportResult{}, wrErr
	}

	// Wrote is set from here down: the file on disk has been replaced, so every
	// exit past this point leaves a credential pair the running process may not
	// have compared yet.
	result := ImportResult{Wrote: true}

	// Load(path), not Reload(): Reload re-reads the jar's OWN filePath and is a
	// silent no-op on a jar that was never loaded from one, which would leave
	// the process serving the old credentials while the file on disk is
	// correct. Load is what the other two merge-writers use.
	if err := s.jar.Load(s.cookiePath); err != nil {
		s.setError("the imported cookies were written but could not be loaded: " + err.Error())
		return result, err
	}

	yt, tw := s.checkPlatformAuth(ctx)
	result.YouTube = verdictOf(yt)
	result.Twitch = verdictOf(tw)
	result.YouTubeAccepted = credentialAccepted(yt)
	result.TwitchAccepted = credentialAccepted(tw)

	// Clear the re-login flag for every platform this import ACCEPTED, not just
	// the ones it verified — the operator has just done the thing the flag asks
	// for, and leaving it raised because the confirming request hit a rate
	// limit would nag them about work already done. Process-local; the next
	// conclusive check re-raises it. Same rule, same wording, as
	// FinishSetupDetailed's clear.
	s.mu.Lock()
	if result.YouTubeAccepted {
		s.needsRelogin["youtube"] = false
	}
	if result.TwitchAccepted {
		s.needsRelogin["twitch"] = false
	}
	s.mu.Unlock()

	return result, nil
}
