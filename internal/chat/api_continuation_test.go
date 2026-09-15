package chat

import "testing"

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
