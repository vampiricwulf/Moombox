package twitch

import (
	"reflect"
	"strconv"
	"testing"
	"unicode/utf16"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

func TestParseIRCTags(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect map[string]string
	}{
		{
			name:  "key value pairs",
			input: "color=#FF0000;display-name=TestUser;emotes=;subscriber=1",
			expect: map[string]string{
				"color":        "#FF0000",
				"display-name": "TestUser",
				"emotes":       "",
				"subscriber":   "1",
			},
		},
		{
			name:   "empty string",
			input:  "",
			expect: map[string]string{},
		},
		{
			name:  "single pair",
			input: "id=abc-123-def",
			expect: map[string]string{
				"id": "abc-123-def",
			},
		},
		{
			name:  "key with no value (no equals sign)",
			input: "somekey",
			expect: map[string]string{
				"somekey": "",
			},
		},
		{
			name:  "key with empty value",
			input: "emotes=",
			expect: map[string]string{
				"emotes": "",
			},
		},
		{
			name:  "escaped characters in value",
			input: `system-msg=User\ssubscribed\sat\sTier\s1`,
			expect: map[string]string{
				"system-msg": `User\ssubscribed\sat\sTier\s1`,
			},
		},
		{
			name:  "mixed keys with and without values",
			input: "turbo;color=#1E90FF;user-id=12345",
			expect: map[string]string{
				"turbo":   "",
				"color":   "#1E90FF",
				"user-id": "12345",
			},
		},
		{
			name:  "tmi-sent-ts timestamp",
			input: "tmi-sent-ts=1678900000000;id=msg-123",
			expect: map[string]string{
				"tmi-sent-ts": "1678900000000",
				"id":          "msg-123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIRCTags(tt.input)
			if len(got) != len(tt.expect) {
				t.Errorf("parseIRCTags(%q) returned %d entries, expected %d", tt.input, len(got), len(tt.expect))
				t.Errorf("  got:    %v", got)
				t.Errorf("  expect: %v", tt.expect)
				return
			}
			for k, v := range tt.expect {
				if got[k] != v {
					t.Errorf("parseIRCTags(%q)[%q] = %q, expected %q", tt.input, k, got[k], v)
				}
			}
		})
	}
}

func TestParseBadges(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{
			name:   "multiple badges",
			input:  "subscriber/12,moderator/1",
			expect: []string{"subscriber/12", "moderator/1"},
		},
		{
			name:   "empty string returns nil",
			input:  "",
			expect: nil,
		},
		{
			name:   "single badge",
			input:  "broadcaster/1",
			expect: []string{"broadcaster/1"},
		},
		{
			name:   "three badges",
			input:  "subscriber/24,moderator/1,turbo/1",
			expect: []string{"subscriber/24", "moderator/1", "turbo/1"},
		},
		{
			name:   "vip badge",
			input:  "vip/1",
			expect: []string{"vip/1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBadges(tt.input)
			if tt.expect == nil {
				if got != nil {
					t.Errorf("parseBadges(%q) = %v, expected nil", tt.input, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.expect) {
				t.Errorf("parseBadges(%q) = %v, expected %v", tt.input, got, tt.expect)
			}
		})
	}
}

func TestParseEmoteTags(t *testing.T) {
	tests := []struct {
		name      string
		emotesStr string
		message   string
		expect    []TwitchEmoteRef
	}{
		{
			name:      "empty emotes string returns nil",
			emotesStr: "",
			message:   "hello world",
			expect:    nil,
		},
		{
			name:      "single emote",
			emotesStr: "25:0-4",
			message:   "Kappa hello",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
			},
		},
		{
			name:      "single emote multiple positions",
			emotesStr: "25:0-4,12-16",
			message:   "Kappa hello Kappa",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
				{ID: "25", Name: "Kappa", Start: 12, End: 16},
			},
		},
		{
			name:      "multiple different emotes",
			emotesStr: "25:0-4/1902:6-10",
			message:   "Kappa Keepo",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 0, End: 4},
				{ID: "1902", Name: "Keepo", Start: 6, End: 10},
			},
		},
		{
			name:      "invalid position format skipped",
			emotesStr: "25:invalid",
			message:   "Kappa",
			expect:    nil,
		},
		{
			name:      "no colon separator skipped",
			emotesStr: "25",
			message:   "Kappa",
			expect:    nil,
		},
		{
			name:      "emote at end of message",
			emotesStr: "25:6-10",
			message:   "hello Kappa",
			expect: []TwitchEmoteRef{
				{ID: "25", Name: "Kappa", Start: 6, End: 10},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEmoteTags(tt.emotesStr, tt.message)
			if tt.expect == nil {
				if got != nil {
					t.Errorf("parseEmoteTags(%q, %q) = %v, expected nil", tt.emotesStr, tt.message, got)
				}
				return
			}
			if len(got) != len(tt.expect) {
				t.Errorf("parseEmoteTags(%q, %q) returned %d refs, expected %d", tt.emotesStr, tt.message, len(got), len(tt.expect))
				return
			}
			for i, ref := range got {
				exp := tt.expect[i]
				if ref.ID != exp.ID || ref.Name != exp.Name || ref.Start != exp.Start || ref.End != exp.End {
					t.Errorf("parseEmoteTags(%q, %q)[%d] = {ID:%q Name:%q Start:%d End:%d}, expected {ID:%q Name:%q Start:%d End:%d}",
						tt.emotesStr, tt.message, i,
						ref.ID, ref.Name, ref.Start, ref.End,
						exp.ID, exp.Name, exp.Start, exp.End)
				}
			}
		})
	}
}

func TestParseEmoteTagsOutOfBounds(t *testing.T) {
	// Emote position extends beyond message length — name should be empty and
	// the raw wire offsets survive untouched.
	refs := parseEmoteTags("25:0-99", "short")
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Name != "" {
		t.Errorf("expected empty name for out-of-bounds emote, got %q", refs[0].Name)
	}
	if refs[0].Start != 0 || refs[0].End != 99 {
		t.Errorf("out-of-bounds ref = (%d,%d), want the raw wire offsets (0,99)",
			refs[0].Start, refs[0].End)
	}
}

// TestParseEmoteTagsInvertedRange pins a range whose end precedes its start.
//
// Mutant: dropping the `start <= end` half of the bounds guard. The old code
// guarded only `start >= 0 && end < len(msgUnits)`, so "25:4-2" on "Kappa"
// evaluated msgUnits[4:3] and PANICKED — inside the IRC read loop, which would
// take the whole chat session down on one malformed tag.
func TestParseEmoteTagsInvertedRange(t *testing.T) {
	refs := parseEmoteTags("25:4-2", "Kappa")
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Name != "" || refs[0].Start != 4 || refs[0].End != 2 {
		t.Errorf("inverted ref = {Name:%q Start:%d End:%d}, want {\"\" 4 2}",
			refs[0].Name, refs[0].Start, refs[0].End)
	}
}

// TestParseEmoteTagsNonBMP pins THE wire fact: Twitch's emote-tag offsets count
// Unicode CODE POINTS, and the Start/End we emit are UTF-16 code units because
// player.js slices with String.prototype.substring and the VOD path (api.go,
// utf16Len) already emits UTF-16.
//
// Verified 2026-09-15 over the raw IRC lines of 18 real Twitch chat archives:
// of 120 emote ranges preceded by a non-BMP character, 120 are whole-word
// tokens under code-point slicing and 0 under UTF-16 slicing. chatterino7
// (codepointToUtf16Idx), gempir/go-twitch-irc ([]rune slicing) and
// robotty/twitch-irc-rs (chars().skip().take()) all index by code point.
//
// Mutants this kills:
//   - raw pass-through ("Twitch sends UTF-16 already"): Start would be 2, not 3.
//   - UTF-16-indexed slicing (the behaviour before this arc, commit 5031cd2b):
//     Name would be " Kapp" and Start 2.
//   - End = cpToUnit[end] instead of cpToUnit[end+1]-1. The two expressions
//     are EQUAL whenever the span's last code point is BMP, so the first three
//     fixtures cannot see this one at all; it clips only a span that ends on a
//     non-BMP code point, which is why "non-BMP emote at end" exists. There it
//     reports End 6 instead of 7 and the utf16.Decode check below then decodes
//     a lone high surrogate.
//   - End derived from the sentinel (cpToUnit[len(runes)]-1) rather than from
//     cpToUnit[end+1]. Every other fixture puts the emote last, where those are
//     the same value; "non-BMP emote mid-message" is the only span here that
//     does not end the message, and it reports End 5 instead of 3.
//
// The sentinel entry itself needs no particular fixture. The store
// `cpToUnit[len(runes)] = units` is UNCONDITIONAL — it runs after the fill
// loop, before any range is read — so every call with a non-empty emote tag
// executes it, and every fixture in this file pins it, the ASCII table in
// TestParseEmoteTags included. Dropping the +1 from make([]int, len(runes)+1)
// therefore makes that store an out-of-range panic on the first such call,
// rather than a wrong answer some fixture has to be shaped to catch.
func TestParseEmoteTagsNonBMP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		emotesStr string
		message   string
		want      TwitchEmoteRef
	}{
		{
			// "🎉 Kappa": 🎉 is code point 0 (UTF-16 [0..1]), space code point 1
			// (UTF-16 [2]), "Kappa" code points 2..6 (UTF-16 [3..7]).
			name:      "emoji then emote",
			emotesStr: "25:2-6",
			message:   "🎉 Kappa",
			want:      TwitchEmoteRef{ID: "25", Name: "Kappa", Start: 3, End: 7},
		},
		{
			// A real captured line: wire text "🤘 shachiOrcaWail", range 2-15.
			name:      "real wire range",
			emotesStr: "301428:2-15",
			message:   "🤘 shachiOrcaWail",
			want:      TwitchEmoteRef{ID: "301428", Name: "shachiOrcaWail", Start: 3, End: 16},
		},
		{
			// Two non-BMP characters before the emote: the drift is per code
			// point, so a single-character fudge factor cannot pass this.
			name:      "two emoji then emote",
			emotesStr: "25:4-8",
			message:   "🤘🤝 xKappa",
			want:      TwitchEmoteRef{ID: "25", Name: "Kappa", Start: 6, End: 10},
		},
		{
			// The emote IS the non-BMP character, and it ends the message:
			// "Kappa 🎉" is code points 0..6 with 🎉 at code point 6, UTF-16
			// [6..7]. End must be 7 — the SECOND unit of the surrogate pair.
			name:      "non-BMP emote at end",
			emotesStr: "25:6-6",
			message:   "Kappa 🎉",
			want:      TwitchEmoteRef{ID: "25", Name: "🎉", Start: 6, End: 7},
		},
		{
			// A non-BMP emote with text on BOTH sides: "a 🎉 b" is code points
			// 0..4 with 🎉 at code point 2, UTF-16 [2..3]. The only span in this
			// table that does not run to the end of the message.
			name:      "non-BMP emote mid-message",
			emotesStr: "25:2-2",
			message:   "a 🎉 b",
			want:      TwitchEmoteRef{ID: "25", Name: "🎉", Start: 2, End: 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := parseEmoteTags(tc.emotesStr, tc.message)
			if len(refs) != 1 {
				t.Fatalf("expected 1 ref, got %d", len(refs))
			}
			if refs[0] != tc.want {
				t.Errorf("parseEmoteTags(%q, %q) = %+v, want %+v",
					tc.emotesStr, tc.message, refs[0], tc.want)
			}
			// The emitted span must be sliceable by JavaScript's own
			// substring(start, end+1) — which is what player.js does.
			units := utf16.Encode([]rune(tc.message))
			if got := string(utf16.Decode(units[refs[0].Start : refs[0].End+1])); got != tc.want.Name {
				t.Errorf("UTF-16 slice [%d:%d] = %q, want %q — the emitted span does not "+
					"select the emote in player.js's index space",
					refs[0].Start, refs[0].End+1, got, tc.want.Name)
			}
		})
	}
}

func TestChatDedupAddMessage(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "test",
		StreamID:     "123",
	}, &testLogger{})

	// Add unique messages
	for i := range 100 {
		cd.addMessage(&TwitchChatMessage{
			ID:          "msg_" + strconv.Itoa(i),
			TimestampMs: int64(i * 1000),
		})
	}

	if cd.MessageCount() != 100 {
		t.Errorf("expected 100 messages, got %d", cd.MessageCount())
	}

	// Try adding duplicate
	cd.addMessage(&TwitchChatMessage{
		ID:          "msg_50",
		TimestampMs: 50000,
	})

	if cd.MessageCount() != 100 {
		t.Errorf("expected 100 messages after duplicate, got %d", cd.MessageCount())
	}
}

func TestChatDedupPruning(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "test",
		StreamID:     "123",
	}, &testLogger{})

	// addMessage prunes at chatDedupMax*2, keeping chatDedupMax entries.
	// After pruning, new messages continue to arrive. So add exactly
	// chatDedupMax*2+1 to trigger one pruning pass, then verify.
	totalMsgs := chatDedupMax*2 + 1
	for i := range totalMsgs {
		cd.addMessage(&TwitchChatMessage{
			ID:          "msg_" + strconv.Itoa(i),
			TimestampMs: int64(i * 1000),
		})
	}

	cd.mu.Lock()
	dedupLen := cd.dedup.Len()
	cd.mu.Unlock()

	// After inserting chatDedupMax*2+1 messages:
	// On the 10001st add, dedup.Len() becomes 10001 > 10000,
	// pruning fires and keeps the last chatDedupMax (5000) entries.
	expectedLen := chatDedupMax
	if dedupLen != expectedLen {
		t.Errorf("expected dedup len=%d after pruning, got %d", expectedLen, dedupLen)
	}

	cd.mu.Lock()
	lastID := "msg_" + strconv.Itoa(totalMsgs-1)
	hasLast := cd.dedup.Seen(lastID)
	firstID := "msg_0"
	hasFirst := cd.dedup.Seen(firstID)
	cd.mu.Unlock()

	if !hasLast {
		t.Error("expected most recent message to be in dedup set")
	}
	if hasFirst {
		t.Error("expected oldest message to be pruned from dedup set")
	}
}

// TestChatDownloaderStartDoubleCallRejected verifies that a second Start call
// against a running downloader returns an error rather than racing on the
// dedup/resume state. Regression test for audit reports/twitch.md issue #4.
func TestChatDownloaderStartDoubleCallRejected(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "test",
		StreamID:     "123",
	}, &testLogger{})

	// Simulate an already-running downloader.
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()

	ctx := t.Context()

	err := cd.Start(ctx)
	if err == nil {
		t.Fatal("expected Start to fail when downloader is already running")
	}
}

func TestChatDedupLastTimestamp(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "test",
		StreamID:     "123",
	}, &testLogger{})

	cd.addMessage(&TwitchChatMessage{
		ID:          "msg_1",
		TimestampMs: 1000,
	})
	cd.addMessage(&TwitchChatMessage{
		ID:          "msg_2",
		TimestampMs: 5000,
	})
	cd.addMessage(&TwitchChatMessage{
		ID:          "msg_3",
		TimestampMs: 3000, // Out of order
	})

	cd.mu.Lock()
	lastTs := cd.lastTimestampMs
	cd.mu.Unlock()

	if lastTs != 5000 {
		t.Errorf("expected lastTimestampMs to be 5000, got %d", lastTs)
	}
}

func TestParseLinePrivmsg(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	line := "@badges=subscriber/12;color=#FF0000;display-name=TestUser;emotes=;id=msg-abc123;tmi-sent-ts=1678900000000;user-id=12345 :testuser!testuser@testuser.tmi.twitch.tv PRIVMSG #testchannel :Hello World!"

	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("expected non-nil message from PRIVMSG")
	}
	if msg.ID != "msg-abc123" {
		t.Errorf("ID = %q, want %q", msg.ID, "msg-abc123")
	}
	if msg.AuthorName != "TestUser" {
		t.Errorf("AuthorName = %q, want %q", msg.AuthorName, "TestUser")
	}
	if msg.Message != "Hello World!" {
		t.Errorf("Message = %q, want %q", msg.Message, "Hello World!")
	}
	if msg.MessageType != "chat" {
		t.Errorf("MessageType = %q, want %q", msg.MessageType, "chat")
	}
	if msg.Raw != line {
		t.Error("Raw should contain the original line")
	}
}

func TestParseLineUsernotice(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	line := `@badges=subscriber/0;display-name=GiftGiver;id=gift-123;msg-id=subgift;msg-param-recipient-display-name=LuckyUser;msg-param-sub-plan=1000;system-msg=GiftGiver\sgifted\sa\sTier\s1\ssub;tmi-sent-ts=1678900000000;user-id=99999 :giftgiver!giftgiver@giftgiver.tmi.twitch.tv USERNOTICE #testchannel`

	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("expected non-nil message from USERNOTICE")
	}
	if msg.MessageType != "subgift" {
		t.Errorf("MessageType = %q, want %q", msg.MessageType, "subgift")
	}
	if msg.GiftRecipient != "LuckyUser" {
		t.Errorf("GiftRecipient = %q, want %q", msg.GiftRecipient, "LuckyUser")
	}
	if msg.SubPlan != "1000" {
		t.Errorf("SubPlan = %q, want %q", msg.SubPlan, "1000")
	}
	if msg.SystemMsg != "GiftGiver gifted a Tier 1 sub" {
		t.Errorf("SystemMsg = %q", msg.SystemMsg)
	}
}

func TestParseLineUsernoticeAnnouncement(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	line := `@badges=broadcaster/1;color=#1E90FF;display-name=StreamerName;id=ann-456;msg-id=announcement;msg-param-color=PURPLE;system-msg=;tmi-sent-ts=1678900000000;user-id=12345 :streamername!streamername@streamername.tmi.twitch.tv USERNOTICE #testchannel :Tournament starts in 10 minutes!`

	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("expected non-nil message from announcement USERNOTICE")
	}
	if msg.MessageType != "announcement" {
		t.Errorf("MessageType = %q, want %q", msg.MessageType, "announcement")
	}
	if msg.AnnouncementColor != "purple" {
		t.Errorf("AnnouncementColor = %q, want %q", msg.AnnouncementColor, "purple")
	}
	if msg.Message != "Tournament starts in 10 minutes!" {
		t.Errorf("Message = %q, want %q", msg.Message, "Tournament starts in 10 minutes!")
	}
}

func TestParseLineUsernoticeAnnouncementNoColor(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	// msg-param-color absent → AnnouncementColor empty (semantically "primary")
	line := `@badges=broadcaster/1;display-name=StreamerName;id=ann-789;msg-id=announcement;system-msg=;tmi-sent-ts=1678900000000;user-id=12345 :streamername!streamername@streamername.tmi.twitch.tv USERNOTICE #testchannel :be right back`

	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("expected non-nil message from announcement USERNOTICE")
	}
	if msg.MessageType != "announcement" {
		t.Errorf("MessageType = %q, want %q", msg.MessageType, "announcement")
	}
	if msg.AnnouncementColor != "" {
		t.Errorf("AnnouncementColor = %q, want empty", msg.AnnouncementColor)
	}
}

func TestParseLineNoID(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	// PRIVMSG without id tag should return nil
	line := "@badges=;display-name=User;tmi-sent-ts=1000 :user!user@user.tmi.twitch.tv PRIVMSG #testchannel :hello"
	msg := cd.parseLine(line)
	if msg != nil {
		t.Error("expected nil for PRIVMSG without id")
	}
}

func TestParseLinePing(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	// PING should not produce a message (parseLine doesn't handle it; handled in runIRCSession)
	msg := cd.parseLine("PING :tmi.twitch.tv")
	if msg != nil {
		t.Error("expected nil for PING")
	}
}

func TestParseLineUnknownCommand(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		dedup:        utils.NewOrderedDedup[string](),
		logger:       &testLogger{},
	}

	msg := cd.parseLine(":tmi.twitch.tv 001 justinfan12345 :Welcome, GLHF!")
	if msg != nil {
		t.Error("expected nil for unknown command")
	}
}

// TestParsePrivmsgStripsActionWrapper pins the /me shape end to end.
//
// A "/me" chat line reaches us as the CTCP form \x01ACTION <text>\x01, and the
// emote offsets index the STRIPPED text — verified 2026-09-15 against the two
// real ACTION ranges in the archives: both slice to a whole word after the
// wrapper is removed, neither does against the wrapped text.
//
// Mutants this kills:
//   - no strip at all (today): Message keeps "\x01ACTION Kappa\x01" and the
//     emote lands on "ACTION" — offsets shift by the wrapper's 8 code points.
//   - stripping AFTER parseEmoteTags: Message is right and the emote span is
//     still 8 units too far along.
//   - stripping only the prefix: the trailing \x01 renders as a stray glyph and
//     the message length is one too long.
//   - stripping on any message that merely CONTAINS the marker: the third case
//     below sends "\x01ACTION" mid-text and must come through untouched.
//
// The last three rows are the wrapper's EDGES. stripActionWrapper is a
// CutPrefix plus a TrimSuffix, and each of the three is a place a tidier
// rewrite would quietly change behaviour:
//   - an unclosed wrapper (the prefix arrived, the trailing SOH did not) is
//     still an action carrying its text. Mutant: an implementation that
//     REQUIRES the closing SOH — a truncated line would then render with a
//     literal "ACTION " head instead of as a /me.
//   - an empty body (prefix immediately followed by the closing SOH) is still
//     an action, with empty text. Mutant: one that requires a non-empty body,
//     or that decides IsAction from the TrimSuffix result — the archive would
//     record a bare "" chat line rather than an empty action.
//   - ACTION with NO space after it is not a wrapper at all. Mutant: matching
//     the prefix without its trailing space — the head of any message that
//     happens to start with those bytes would be eaten.
func TestParsePrivmsgStripsActionWrapper(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
	}, &testLogger{})

	const soh = "\x01"
	for _, tc := range []struct {
		name       string
		tags       string
		body       string
		wantText   string
		wantAction bool
		wantEmotes []TwitchEmoteRef
	}{
		{
			name:       "action with an emote at the head",
			tags:       "emotes=25:0-4;id=m1;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION Kappa" + soh,
			wantText:   "Kappa",
			wantAction: true,
			wantEmotes: []TwitchEmoteRef{{ID: "25", Name: "Kappa", Start: 0, End: 4}},
		},
		{
			name:       "real wire action range",
			tags:       "emotes=301428:20-33;id=m2;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION take care everychat shachiOrcaLove" + soh,
			wantText:   "take care everychat shachiOrcaLove",
			wantAction: true,
			wantEmotes: []TwitchEmoteRef{{ID: "301428", Name: "shachiOrcaLove", Start: 20, End: 33}},
		},
		{
			name:       "ordinary message is untouched",
			tags:       "emotes=25:0-4;id=m3;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       "Kappa hello",
			wantText:   "Kappa hello",
			wantAction: false,
			wantEmotes: []TwitchEmoteRef{{ID: "25", Name: "Kappa", Start: 0, End: 4}},
		},
		{
			name:       "the marker mid-text is not a wrapper",
			tags:       "id=m4;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       "look: " + soh + "ACTION is the CTCP form",
			wantText:   "look: " + soh + "ACTION is the CTCP form",
			wantAction: false,
		},
		{
			// A truncated wire line: the prefix identifies the form, so the
			// missing closing SOH does not un-make the action.
			name:       "an unclosed wrapper is still an action",
			tags:       "id=m5;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION Kappa",
			wantText:   "Kappa",
			wantAction: true,
		},
		{
			// "/me " with nothing after it: empty text AND an action. The two
			// facts are independent.
			name:       "an empty action body keeps IsAction",
			tags:       "id=m6;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTION " + soh,
			wantText:   "",
			wantAction: true,
		},
		{
			// No space after ACTION, so the prefix does not match and the
			// whole value — both SOH bytes included — is the message.
			name:       "ACTION without the space is not a wrapper",
			tags:       "id=m7;tmi-sent-ts=1700000000000;user-id=u1;display-name=Viewer",
			body:       soh + "ACTIONKappa" + soh,
			wantText:   soh + "ACTIONKappa" + soh,
			wantAction: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := "@" + tc.tags + " :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #testchan :" + tc.body
			msg := cd.parseLine(line)
			if msg == nil {
				t.Fatal("parseLine returned nil for a well-formed PRIVMSG")
			}
			if msg.Message != tc.wantText {
				t.Errorf("Message = %q, want %q", msg.Message, tc.wantText)
			}
			if msg.IsAction != tc.wantAction {
				t.Errorf("IsAction = %v, want %v", msg.IsAction, tc.wantAction)
			}
			if len(msg.Emotes) != len(tc.wantEmotes) {
				t.Fatalf("Emotes = %+v, want %+v", msg.Emotes, tc.wantEmotes)
			}
			for i := range tc.wantEmotes {
				if msg.Emotes[i] != tc.wantEmotes[i] {
					t.Errorf("Emotes[%d] = %+v, want %+v", i, msg.Emotes[i], tc.wantEmotes[i])
				}
			}
			// The lossless line is the archive's only record of what Twitch
			// actually sent, so the wrapper must still be in it.
			if msg.Raw != line {
				t.Errorf("Raw was rewritten; it must stay the verbatim wire line")
			}
		})
	}
}

// TestActionIsNotStrippedFromUsernotice pins the narrowness of the strip.
// USERNOTICE bodies (subs, raids, announcements) are never CTCP-wrapped, and a
// strip there would silently eat the head of any system message that happened
// to start with the marker.
func TestActionIsNotStrippedFromUsernotice(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
	}, &testLogger{})
	const soh = "\x01"
	line := "@id=u1;tmi-sent-ts=1700000000000;msg-id=sub;display-name=Viewer;user-id=u1 " +
		":tmi.twitch.tv USERNOTICE #testchan :" + soh + "ACTION Kappa" + soh
	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("parseLine returned nil for a well-formed USERNOTICE")
	}
	if msg.Message != soh+"ACTION Kappa"+soh {
		t.Errorf("USERNOTICE body = %q, want it verbatim", msg.Message)
	}
	if msg.IsAction {
		t.Error("IsAction set on a USERNOTICE — the strip must be PRIVMSG-only")
	}
}
