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

// platformsInNetscape reports which platforms a Netscape blob carries rows for.
//
// Asked of the CLEANED rows by its one caller, never of the submitted text: a
// row the empty-value filter threw away changes nothing on disk, and counting
// it as "the operator supplied credentials for this platform" would report an
// import of a credential that was never installed.
//
// Built out of netscapeDataRows and cookieRowPlatform — the same pair
// restorePlatformRows swaps rows with — so "which platform is this row" has one
// reading across the write and the rollback. A second reading is how a rollback
// gives back rows an outcome says were never touched.
func platformsInNetscape(netscape string) map[string]bool {
	touched := map[string]bool{}
	for _, row := range netscapeDataRows(netscape) {
		if platform := cookieRowPlatform(row); platform != "" {
			touched[platform] = true
		}
	}
	return touched
}

// ImportOutcome is what one import DID to one platform's rows on disk, which is
// a different question from what verifying them CONCLUDED.
//
// The pair (RefreshOK, ImportRolledBack) is the state this type exists to
// carry, and the one the dashboard had no way to say: the platform
// authenticates, and the rows doing it are not the ones the operator just
// pasted. Read through the verdict alone that import is a success, and the
// operator walks away believing a paste that was thrown out — which is finding
// F6's second half, the first being that the paste used to destroy those rows
// instead.
type ImportOutcome int

const (
	// ImportUnknown means this import reached no conclusion about the
	// platform's rows. It is the ZERO VALUE on purpose, the same guard
	// RefreshUnknown is: the exits that fail with cookies.txt already replaced
	// — a rollback that could not be written, a jar that could not read the
	// file back — must not claim a platform was imported or left alone.
	ImportUnknown ImportOutcome = iota
	// ImportUnchanged means the paste carried no row for this platform, so
	// whatever was on disk for it is still there. mergeCookieFiles carries the
	// sibling platform verbatim; this is that guarantee, reported.
	ImportUnchanged
	// ImportInstalled means the paste's rows for this platform are what is on
	// disk now. It says nothing about the verdict — an accepted-but-unverified
	// import lands here, because the rows ARE installed and in use.
	ImportInstalled
	// ImportRolledBack means the paste's rows were written, conclusively
	// rejected, and the previous rows put back. The credentials in force are
	// the previous ones, and the verdict beside this describes THOSE.
	ImportRolledBack
	// ImportRejected means the paste's rows were written and conclusively
	// rejected with nothing established to give back — no previous file, no
	// snapshot, or a platform that was already dead. The rejected rows are on
	// disk, deliberately: replacing dead cookies with other dead cookies costs
	// nothing, and the fresher set is the better guess for the next attempt.
	ImportRejected
)

func (o ImportOutcome) String() string {
	switch o {
	case ImportUnchanged:
		return "unchanged"
	case ImportInstalled:
		return "imported"
	case ImportRolledBack:
		return "rolled-back"
	case ImportRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// importOutcomeFor answers ImportOutcome for one platform.
//
// `check` is the check that JUDGED the paste, never a re-verification after a
// rollback: the question "what happened to these rows" can only be answered by
// the check that rejected them. Same rule, same reason, as
// rollbackWasInconclusive on the refresh path.
func importOutcomeFor(platform string, touched, restored map[string]bool, check map[string]platformAuth) ImportOutcome {
	switch {
	case !touched[platform]:
		return ImportUnchanged
	case restored[platform]:
		return ImportRolledBack
	case check[platform].state == verifyFailed:
		return ImportRejected
	default:
		return ImportInstalled
	}
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

	// What this import DID to each platform's rows. ADDED, never replacing the
	// two facts above: those still answer "can we do authenticated work" and
	// "did we accept what the operator supplied", and after a rollback both are
	// about the RESTORED credentials. This third fact is the one that says the
	// paste itself was thrown out, and without it a rolled-back import is
	// indistinguishable on the wire from a successful one.
	YouTubeOutcome ImportOutcome
	TwitchOutcome  ImportOutcome

	// RollbackProtected reports whether this import could have given a platform
	// its previous rows back: a pre-write snapshot was taken AND it describes
	// the file that was about to be replaced.
	//
	// False in two situations that must not be confused with each other in the
	// wording, which is why the log line and this field are separate: there was
	// no cookies.txt at all (a first acquisition — nothing to protect, nothing
	// to warn about), or the pre-write load failed (protection is off, and the
	// log says so in the refresh path's own sentence). In both, no platform was
	// restored and none could have been.
	//
	// Deliberately NOT on the wire. It is a fact about the FILE and about what
	// this import risked, not a verdict about a platform, and the operator's
	// next move does not change with it — the same split SetupResult.Wrote
	// makes. cookieImportOutcome carries the per-platform outcomes instead.
	RollbackProtected bool

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
// cookies.txt, reloads the jar, verifies what it just installed and gives a
// platform its previous rows back if the paste killed it.
//
// VERIFIED AND REVERSIBLE, per platform, on the same machinery both refresh
// paths use: snapshotPlatformAuth before the write, platformsToRestoreOnRegression
// over the check after it, restorePlatformRows to swap the rows back. Until
// this, a paste whose rows for one platform were dead REPLACED that platform's
// working rows — mergeCookieFiles lets the pasted value win by
// name+domain+path — and the operator was told the credentials failed, over a
// session that had been alive until they pressed the button. The sibling
// platform was safe (the merge carries it verbatim) and, verifying, made the
// whole import look partly successful.
//
// The result says which of the four things happened to each platform; see
// ImportOutcome, and RollbackProtected for what "could not have been undone"
// looks like.
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

	// Which platforms this paste actually carries. cleanNetscapeRows is pure
	// and runs a second time for it, which is cheaper than widening
	// prepareCookieImport's contract and the table tests that pin it — and the
	// answer has to come from the cleaned rows; see platformsInNetscape.
	cleaned, _, _, _ := cleanNetscapeRows(netscape)
	touched := platformsInNetscape(cleaned)

	// Verify BEFORE overwriting, exactly as both refresh paths do and through
	// the same helper. Rolling a regression back is impossible without knowing
	// what worked beforehand, and "the file had cookies" is not the same as
	// "those cookies worked" — which is precisely how a paste whose rows for
	// one platform are dead used to replace that platform's working rows and be
	// reported as a failure of the credentials rather than of the paste.
	//
	// Skipped when there is nothing to protect, so a first acquisition — every
	// fresh container — costs no extra round trips and produces no warning
	// about protection being off. On an install that already has credentials
	// the cost is two verification round trips: the price of not silently
	// destroying them.
	pre := map[string]platformAuth{}
	rollbackProtected := false
	if existing != "" {
		pre, rollbackProtected = s.snapshotPlatformAuth(ctx, "import")
		if !rollbackProtected {
			// The snapshot describes whatever the jar last held rather than the
			// file about to be replaced, so it establishes nothing about what
			// this import would be destroying and must not license handing rows
			// back. The import still PROCEEDS: refusing it would throw away
			// credentials the operator supplied by hand for the sake of a read
			// that failed, and this endpoint is the only re-authentication a
			// container has. snapshotPlatformAuth has already logged it.
			pre = map[string]platformAuth{}
		}
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

	// importCheck is kept under its own name because the rollback below
	// REPLACES yt/tw with a re-verification of what was restored. The question
	// "why were these rows rejected" can only be answered by the check that
	// rejected them; after the re-verify that check is no longer in scope. Same
	// name, same reason, as the refresh path's.
	importCheck := map[string]platformAuth{"youtube": yt, "twitch": tw}

	// Roll back, per platform, a paste that made that platform worse.
	//
	// platformsToRestoreOnRegression is the REGRESSION ARM ONLY, shared with
	// the browser refresh: "it verified before the write and is conclusively
	// rejected after it". An inconclusive check is not a failure and must not
	// roll anything back — see that function for the full argument, and note
	// that on this path the opposite would contradict credentialAccepted, which
	// ACCEPTS an inconclusive check over a credential a human just supplied.
	//
	// Filtered to the platforms the paste TOUCHED. A platform this paste never
	// mentioned cannot have been damaged by it — mergeCookieFiles carries its
	// rows verbatim — so a restore there would write its own rows back over
	// themselves and make the log and the outcome claim something happened. The
	// one way an untouched platform can still lose rows is the merge's expiry
	// prune, and restorePlatformRows applies that same prune to the previous
	// rows, so there is nothing a rollback could give it either.
	restore := map[string]bool{}
	for platform, wanted := range platformsToRestoreOnRegression(pre, importCheck) {
		if wanted && touched[platform] {
			restore[platform] = true
		}
	}
	if len(restore) > 0 {
		restoredPlatforms := make([]string, 0, len(restore))
		verdicts := make([]string, 0, len(restore))
		for _, platform := range []string{"youtube", "twitch"} {
			if restore[platform] {
				restoredPlatforms = append(restoredPlatforms, platform)
				verdicts = append(verdicts, platform+"="+verdictOf(importCheck[platform]).String())
			}
		}
		s.logger.Warn("the pasted cookies did not hold up — restoring the previous credentials for those platforms",
			"platforms", strings.Join(restoredPlatforms, ","),
			"verdicts", strings.Join(verdicts, " "),
			"rows", countNetscapeCookieRows(cleaned))

		restored := restorePlatformRows(merged, existing, restore)

		// A rollback that does not land must not be reported as one. Both
		// failures below leave the process describing a file that is not on
		// disk, and a result saying "kept the previous cookies for X" while the
		// rejected paste is what the next download actually uses. So they end
		// the import instead, with a message describing the state that really
		// exists — and they set lastError, because unlike a rejected paste this
		// IS a state of the install: cookies.txt holds credentials that do not
		// work. The per-platform outcomes stay ImportUnknown, which is what that
		// zero value is for.
		//
		// The SAME message is returned and recorded, wrapped around
		// ErrImportRollbackIncomplete. Returning a short "restore previous
		// cookies: %w" instead left the only truthful sentence in lastError,
		// where the dialog that caused this never looks — and the route then
		// fell through to its `result.Wrote` arm and told the operator the
		// cookies had been imported and written, which is false on both exits.
		if restoreErr := writeCookieFile(s.cookiePath, []byte(restored), 0o600); restoreErr != nil {
			failure := fmt.Errorf("%w: the pasted cookies did not verify for %s, and Moombox could not "+
				"restore the previous ones (%w) — cookies.txt still holds the rejected new credentials",
				ErrImportRollbackIncomplete, strings.Join(restoredPlatforms, " + "), restoreErr)
			s.setError(failure.Error())
			s.logger.Error("could not restore the previous cookies.txt after a rejected import",
				"err", restoreErr, "platforms", strings.Join(restoredPlatforms, ","))
			return result, failure
		}
		if loadErr := s.jar.Load(s.cookiePath); loadErr != nil {
			// The FILE is correct here; the running process is not.
			failure := fmt.Errorf("%w: the previous cookies for %s were restored after the pasted ones "+
				"did not verify, but reloading them failed (%w) — this process is still using the "+
				"rejected credentials until the next refresh",
				ErrImportRollbackIncomplete, strings.Join(restoredPlatforms, " + "), loadErr)
			s.setError(failure.Error())
			s.logger.Error("could not reload cookie jar after restoring the previous cookies.txt",
				"err", loadErr, "platforms", strings.Join(restoredPlatforms, ","))
			return result, failure
		}

		// Re-verify the file we actually KEPT. Without this the result would
		// describe the discarded paste and flag a re-login over credentials
		// that were restored and never re-checked — an instruction a container
		// operator cannot act on. No setError on the way out: the credentials
		// in force are the previous, working ones, so "your recordings will
		// fail" would be alarming an operator whose recordings are fine.
		yt, tw = s.checkPlatformAuth(ctx)
	}

	result.YouTube = verdictOf(yt)
	result.Twitch = verdictOf(tw)
	result.YouTubeAccepted = credentialAccepted(yt)
	result.TwitchAccepted = credentialAccepted(tw)
	result.RollbackProtected = rollbackProtected
	result.YouTubeOutcome = importOutcomeFor("youtube", touched, restore, importCheck)
	result.TwitchOutcome = importOutcomeFor("twitch", touched, restore, importCheck)

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

	// The completion line, and on a container it is the only view an operator
	// has of what their paste did. Platform, outcome, verdict and a row count —
	// never a cookie value, never the paste; see TestImportFailurePathsCarryNoValue
	// for the rule this line lives under.
	s.logger.Info("cookie import completed",
		"youtube", result.YouTubeOutcome.String()+"/"+result.YouTube.String(),
		"twitch", result.TwitchOutcome.String()+"/"+result.Twitch.String(),
		"rows", countNetscapeCookieRows(cleaned),
		"rollback_protected", rollbackProtected)

	return result, nil
}
