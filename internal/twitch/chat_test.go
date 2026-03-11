package twitch

import (
	"reflect"
	"strconv"
	"testing"
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
	// Emote position extends beyond message length — name should be empty
	refs := parseEmoteTags("25:0-99", "short")
	if refs == nil {
		t.Fatal("expected non-nil refs")
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	// Start 0, End 99, message "short" has 5 runes — end >= len so name is empty
	if refs[0].Name != "" {
		t.Errorf("expected empty name for out-of-bounds emote, got %q", refs[0].Name)
	}
}

func TestChatDedupAddMessage(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "test",
		StreamID:     "123",
	}, &testLogger{})

	// Add unique messages
	for i := 0; i < 100; i++ {
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
	for i := 0; i < totalMsgs; i++ {
		cd.addMessage(&TwitchChatMessage{
			ID:          "msg_" + strconv.Itoa(i),
			TimestampMs: int64(i * 1000),
		})
	}

	cd.mu.Lock()
	seenLen := len(cd.seenIDs)
	orderLen := len(cd.seenOrder)
	cd.mu.Unlock()

	// After inserting chatDedupMax*2+1 messages:
	// On the 10001st add, len(seenOrder) becomes 10001 > 10000,
	// pruning fires and keeps the last chatDedupMax (5000) entries.
	expectedLen := chatDedupMax
	if seenLen != expectedLen {
		t.Errorf("expected seenIDs=%d after pruning, got %d", expectedLen, seenLen)
	}
	if orderLen != expectedLen {
		t.Errorf("expected seenOrder=%d after pruning, got %d", expectedLen, orderLen)
	}

	// Recent messages should still be in the dedup set
	cd.mu.Lock()
	lastID := "msg_" + strconv.Itoa(totalMsgs-1)
	_, hasLast := cd.seenIDs[lastID]
	firstID := "msg_0"
	_, hasFirst := cd.seenIDs[firstID]
	cd.mu.Unlock()

	if !hasLast {
		t.Error("expected most recent message to be in dedup set")
	}
	if hasFirst {
		t.Error("expected oldest message to be pruned from dedup set")
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
		seenIDs:      make(map[string]struct{}),
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
		seenIDs:      make(map[string]struct{}),
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

func TestParseLineNoID(t *testing.T) {
	cd := &ChatDownloader{
		channelLogin: "testchannel",
		seenIDs:      make(map[string]struct{}),
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
		seenIDs:      make(map[string]struct{}),
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
		seenIDs:      make(map[string]struct{}),
		logger:       &testLogger{},
	}

	msg := cd.parseLine(":tmi.twitch.tv 001 justinfan12345 :Welcome, GLHF!")
	if msg != nil {
		t.Error("expected nil for unknown command")
	}
}
