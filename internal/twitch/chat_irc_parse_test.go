package twitch

import (
	"strings"
	"testing"
)

// TestParseIRCTagsReturnsNilForATaglessLine is TWITCH-6 (report row #61).
// Every inbound line without an "@tags" prefix — JOIN, PART, PONG, CAP,
// ROOMSTATE — allocated a 16-slot map (1,240 B / 4 allocs, measured) that no
// caller ever reads. Nil-map reads are legal in Go, and every consumer in
// parseLine/parsePrivmsg/parseUsernotice only ever reads.
//
// Mutant: restoring `tags := make(map[string]string, 16)` before the empty
// check — got is then non-nil and the first assertion fails.
func TestParseIRCTagsReturnsNilForATaglessLine(t *testing.T) {
	got := parseIRCTags("")
	if got != nil {
		t.Errorf("parseIRCTags(\"\") = %v (non-nil), want nil — a tagless line must allocate nothing", got)
	}
	// The nil map has to be SAFE for every read shape the parsers use.
	if v := got["id"]; v != "" {
		t.Errorf("reading a missing key from the nil map returned %q, want \"\"", v)
	}
	if _, ok := got["display-name"]; ok {
		t.Error("the nil map reported a key present")
	}
	if n := len(got); n != 0 {
		t.Errorf("len(nil map) = %d, want 0", n)
	}
}

// TestParseEmoteTagsSkipsTheIndexTableForBMPText is the second half of
// TWITCH-6. The code-point -> UTF-16 index table is the IDENTITY mapping when
// no rune needs a surrogate pair, which is almost every chat message; building
// it cost 1,320 B / 8 allocs per emote message.
//
// The assertion is RELATIVE — BMP strictly cheaper than non-BMP for the same
// tag — so it says nothing about absolute allocator behaviour and cannot rot
// against a Go release.
//
// Mutant: building cpToUnit unconditionally — the two counts then match and
// `bmp < nonBMP` is false.
func TestParseEmoteTagsSkipsTheIndexTableForBMPText(t *testing.T) {
	bmp := testing.AllocsPerRun(100, func() {
		parseEmoteTags("25:0-4", "Kappa hello world")
	})
	nonBMP := testing.AllocsPerRun(100, func() {
		parseEmoteTags("25:0-4", "Kappa hello \U0001F918world")
	})
	if !(bmp < nonBMP) {
		t.Errorf("parseEmoteTags allocated %v for BMP-only text and %v for text with a "+
			"surrogate pair; the BMP case must be strictly cheaper (it needs no index table)",
			bmp, nonBMP)
	}
}

// TestParseEmoteTagsBMPFastPathKeepsTheOffsets proves the fast path is not a
// behaviour change: for BMP-only text the wire code-point offsets and the
// emitted UTF-16 offsets coincide, which is exactly why the table can be
// skipped.
//
// Mutant: a fast path that forgets to slice Name out of the runes, or that
// zeroes Start/End when cpToUnit is nil.
func TestParseEmoteTagsBMPFastPathKeepsTheOffsets(t *testing.T) {
	refs := parseEmoteTags("25:6-10", "hello Kappa world")
	if len(refs) != 1 {
		t.Fatalf("parseEmoteTags returned %d refs, want 1", len(refs))
	}
	if refs[0].Name != "Kappa" || refs[0].Start != 6 || refs[0].End != 10 {
		t.Errorf("ref = %+v, want Name \"Kappa\" Start 6 End 10", refs[0])
	}
}

// TestIRCCapRequestDropsMembership is owner decision O-S. Twitch's
// twitch.tv/membership capability delivers JOIN and PART bursts — every one of
// them dropped by parseLine's default arm — so the session was paying for
// traffic nothing consumes. chatterino requests it only because it renders a
// user list (TwitchIrcServer.cpp); Moombox does not.
//
// Mutant: putting twitch.tv/membership back into ircCapRequest.
func TestIRCCapRequestDropsMembership(t *testing.T) {
	if strings.Contains(ircCapRequest, "twitch.tv/membership") {
		t.Error("the CAP request still asks for twitch.tv/membership — nothing consumes JOIN/PART")
	}
	if !strings.Contains(ircCapRequest, "twitch.tv/tags") {
		t.Error("the CAP request no longer asks for twitch.tv/tags — every emote, badge and " +
			"id Moombox archives rides on that capability")
	}
	if !strings.Contains(ircCapRequest, "twitch.tv/commands") {
		t.Error("the CAP request no longer asks for twitch.tv/commands — USERNOTICE, RECONNECT " +
			"and NOTICE all ride on it")
	}
	if ircCapRequest != "CAP REQ :twitch.tv/tags twitch.tv/commands" {
		t.Errorf("ircCapRequest = %q, want exactly %q", ircCapRequest,
			"CAP REQ :twitch.tv/tags twitch.tv/commands")
	}
}

// TestIRCSessionSendsTheCapRequestVerbatim proves the constant is what actually
// goes on the wire, not a value the session ignores. startIRCReplier records
// the four handshake messages of each connection.
//
// Mutant: leaving the literal in runIRCSession while the constant changes.
func TestIRCSessionSendsTheCapRequestVerbatim(t *testing.T) {
	rep := startIRCReplier(t, []string{welcomeLine})
	cd := newDowngradeTestChatDownloader(t,
		staticCredentials("token-one", "archiveraccount"), &testLogger{}, func(string) {})

	runLiveIRCSession(t, cd)

	lines := rep.nextSession(t)
	var cap string
	for _, l := range lines {
		if strings.HasPrefix(l, "CAP REQ") {
			cap = l
			break
		}
	}
	if cap == "" {
		t.Fatalf("no CAP REQ in the recorded handshake %q", lines)
	}
	if cap != ircCapRequest {
		t.Errorf("the session wrote %q, want ircCapRequest (%q)", cap, ircCapRequest)
	}
}
