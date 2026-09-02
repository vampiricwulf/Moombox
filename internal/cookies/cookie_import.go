package cookies

import (
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
// an unrelated-looking reason. mergeCookieFiles is the merge — name+domain
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
// by name+domain over a working credential on disk.
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
