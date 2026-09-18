package chat

import (
	"net/url"
	"testing"
)

// chatPage renders a watch page whose ytInitialData carries a chat
// continuation, in the given assignment form. It mirrors watchPageHTML in
// api_cookie_test.go, which every chat cookie test routes through.
func chatPage(assignment, token string) string {
	return `<html><body><script>` + assignment +
		`{"contents":{"twoColumnWatchNextResults":{"conversationBar":{"liveChatRenderer":` +
		`{"isReplay":false,"continuations":[{"reloadContinuationData":{"continuation":"` + token + `"}}]}}}}};` +
		`</script></body></html>`
}

// TestExtractChatContinuationSkipsAForgedCandidate is the chat twin of
// internal/youtube's TestPlayerResponseSkipsAForgedCandidate. A page-authored
// `var ytInitialData = {}` ahead of the real assignment matches the anchor and
// scans cleanly, but is empty — it must not end the search.
//
// Mutant this kills: returning on the first match (the pre-change
// `ytInitialDataRegex.FindStringSubmatch`, and any iterator that stops at the
// first candidate). The forged `{}` then wins and the function reports "no
// liveChatRenderer found" — chat capture silently off for that stream.
func TestExtractChatContinuationSkipsAForgedCandidate(t *testing.T) {
	page := `<p>var ytInitialData = {} </p>` + chatPage(`var ytInitialData = `, "REALTOK")

	tok, replay, err := ExtractChatContinuation([]byte(page))
	if err != nil {
		t.Fatalf("ExtractChatContinuation: %v — a forged candidate ahead of the real assignment ended the search", err)
	}
	if tok != "REALTOK" {
		t.Errorf("token = %q, want %q", tok, "REALTOK")
	}
	if replay {
		t.Errorf("isReplay = true, want false")
	}
}

// TestExtractChatContinuationSkipsAMalformedCandidate pins the acceptance test
// after the decode became it (report #60 / YOUTUBE-15, the chat twin of
// internal/youtube's extractChatContinuation). The old predicate was
// utils.IsNonEmptyJSONObject (since deleted — it had no production caller
// left) — a json.Valid scan over a multi-megabyte literal FOLLOWED by a full
// map decode of the same bytes, two passes answering one question. The map
// decode alone answers it, so only the emptiness half
// (utils.IsNonEmptyJSONBody) runs ahead of it.
//
// A page-authored candidate that scans balanced but is not valid JSON is
// exactly what json.Valid used to reject, so it is what the decode must reject
// now — otherwise the search ends on it and the real blob behind it is lost.
//
// Mutant this kills: accepting on utils.IsNonEmptyJSONBody alone (the decode's
// error ignored, or moved back after the search). The forged candidate then
// wins and the function reports "no liveChatRenderer found" instead of
// returning MALFORMEDOK — chat capture silently off for that stream.
func TestExtractChatContinuationSkipsAMalformedCandidate(t *testing.T) {
	// Balanced braces, a non-empty body, and a trailing comma json.Unmarshal
	// rejects as a syntax error.
	page := `<p>var ytInitialData = {"contents":{"twoColumnWatchNextResults":{},},}</p>` +
		chatPage(`var ytInitialData = `, "MALFORMEDOK")

	tok, _, err := ExtractChatContinuation([]byte(page))
	if err != nil {
		t.Fatalf("ExtractChatContinuation: %v — a malformed candidate ahead of the real assignment ended the search", err)
	}
	if tok != "MALFORMEDOK" {
		t.Errorf("token = %q, want %q", tok, "MALFORMEDOK")
	}
}

// TestExtractChatContinuationReadsTheWindowAssignmentForm pins the second
// anchor. The lazy regex this replaces matched only `var ytInitialData = `
// with exactly one space and required a `;</script>` terminator, so the
// window-property form YouTube has historically served read as "ytInitialData
// not found" — the same spelling internal/youtube's locator has always
// accepted.
//
// Mutant this kills: dropping the window anchor from ytInitialDataAnchors.
func TestExtractChatContinuationReadsTheWindowAssignmentForm(t *testing.T) {
	page := chatPage(`window["ytInitialData"]   =   `, "WTOK")

	tok, _, err := ExtractChatContinuation([]byte(page))
	if err != nil {
		t.Fatalf("ExtractChatContinuation: %v", err)
	}
	if tok != "WTOK" {
		t.Errorf("token = %q, want %q", tok, "WTOK")
	}
}

// TestExtractChatContinuationReportsAMissingBlob keeps the not-found error
// text, which is the only signal a caller gets when a page carries no
// ytInitialData at all (a consent wall body, an error page).
//
// Mutant this kills: returning a nil error with an empty token when the
// locator finds nothing — orchestrator_chat.go and the downloader both treat
// an empty token as "no chat", so the reason would vanish from the log.
func TestExtractChatContinuationReportsAMissingBlob(t *testing.T) {
	if _, _, err := ExtractChatContinuation([]byte(`<html><body>nothing here</body></html>`)); err == nil ||
		err.Error() != "ytInitialData not found" {
		t.Errorf("err = %v, want %q", err, "ytInitialData not found")
	}
}

// TestFreshContinuationWatchURLCarriesTheAgeGateBypass is row #59's twin in
// this package. FetchFreshContinuation is the chat path's own watch-page
// fetch, and without yt-dlp's bpctr / has_verified pair (_video.py:3809) an
// age-restricted stream answers with the age-gate shell — whose ytInitialData
// carries no liveChatRenderer, so the continuation is read as "no chat"
// instead of being read at all.
//
// The whole query string is pinned: the two parameters are APPENDED to the URL
// that already worked, so a rewrite that drops or reorders `v=` fails here too.
//
// Mutant this kills: the two parameters dropped → the query check fails.
func TestFreshContinuationWatchURLCarriesTheAgeGateBypass(t *testing.T) {
	got := freshContinuationWatchURL("dQw4w9WgXcQ")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("freshContinuationWatchURL produced an unparseable URL %q: %v", got, err)
	}
	if want := "https://www.youtube.com/watch"; u.Scheme+"://"+u.Host+u.Path != want {
		t.Errorf("endpoint = %q, want %q", u.Scheme+"://"+u.Host+u.Path, want)
	}
	if want := "v=dQw4w9WgXcQ&bpctr=9999999999&has_verified=1"; u.RawQuery != want {
		t.Errorf("query = %q, want exactly %q", u.RawQuery, want)
	}
}
