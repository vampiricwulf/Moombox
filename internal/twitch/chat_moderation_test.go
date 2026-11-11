package twitch

import (
	"encoding/json"
	"os"
	"testing"
)

// Owner ruling R-G1 (twitch-chat-7): CLEARCHAT and CLEARMSG are stored in the
// chat file as rows of their own. parseLine returned nil for both, so a
// deleted message and a banned user's messages replayed as ordinary chat, and
// IRC has no replay to fetch the events from afterwards.

// TestClearmsgBecomesAModerationRow: one deleted message, named by its id.
//
// Mutant: dropping the CLEARMSG case from parseLine's switch (nil row).
func TestClearmsgBecomesAModerationRow(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	line := `@login=ronni;room-id=1;target-msg-id=abc-123-de;tmi-sent-ts=1700000005000 ` +
		`:tmi.twitch.tv CLEARMSG #testchan :HeyGuys this is spam`
	msg := cd.parseLine(line)
	if msg == nil {
		t.Fatal("parseLine returned nil for a CLEARMSG")
	}
	if msg.MessageType != MessageTypeModeration || msg.Moderation != ModerationDeleted {
		t.Errorf("type/kind = %q/%q, want %q/%q", msg.MessageType, msg.Moderation, MessageTypeModeration, ModerationDeleted)
	}
	if msg.TargetMsgID != "abc-123-de" || msg.TargetLogin != "ronni" {
		t.Errorf("target = id %q login %q, want abc-123-de / ronni", msg.TargetMsgID, msg.TargetLogin)
	}
	if msg.TimestampMs != 1700000005000 {
		t.Errorf("TimestampMs = %d, want the tmi-sent-ts", msg.TimestampMs)
	}
	if msg.ID != "clearmsg-abc-123-de" {
		t.Errorf("ID = %q, want the id derived from the target", msg.ID)
	}
	if msg.Raw != line {
		t.Errorf("Raw = %q, want the verbatim line", msg.Raw)
	}
	// What a player from before the rows existed shows: an empty flat row,
	// and nothing on the video (its overlay skips a message with no content).
	if msg.Message != "" || msg.AuthorID != "" {
		t.Errorf("Message/AuthorID = %q/%q, want both empty", msg.Message, msg.AuthorID)
	}
	if msg.SystemMsg != "A message from ronni was deleted by a moderator." {
		t.Errorf("SystemMsg = %q", msg.SystemMsg)
	}
}

// TestClearchatRows: a timeout, a ban and a full clear are told apart by the
// ban-duration tag and by whether a user is named at all (chatterino7
// IrcMessageHandler.cpp parseClearChatMessage: one parameter = clear all).
//
// Mutants: reading a missing ban-duration as a timeout; treating a clear
// with no user as a ban.
func TestClearchatRows(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cases := []struct {
		name, line    string
		kind          ModerationKind
		userID, login string
		dur           int
		id, sentence  string
	}{
		{
			name:   "timeout",
			line:   `@ban-duration=600;room-id=1;target-user-id=42;tmi-sent-ts=1700000001000 :tmi.twitch.tv CLEARCHAT #testchan :ronni`,
			kind:   ModerationAuthorDeleted,
			userID: "42", login: "ronni", dur: 600,
			id:       "clearchat-1700000001000-42",
			sentence: "ronni was timed out for 10m.",
		},
		{
			name:   "ban",
			line:   `@room-id=1;target-user-id=42;tmi-sent-ts=1700000002000 :tmi.twitch.tv CLEARCHAT #testchan :ronni`,
			kind:   ModerationAuthorDeleted,
			userID: "42", login: "ronni",
			id:       "clearchat-1700000002000-42",
			sentence: "ronni was banned.",
		},
		{
			name:     "full clear",
			line:     `@room-id=1;tmi-sent-ts=1700000003000 :tmi.twitch.tv CLEARCHAT #testchan`,
			kind:     ModerationChatCleared,
			id:       "clearchat-1700000003000-all",
			sentence: "Chat was cleared by a moderator.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := cd.parseLine(tc.line)
			if msg == nil {
				t.Fatal("parseLine returned nil for a CLEARCHAT")
			}
			if msg.MessageType != MessageTypeModeration || msg.Moderation != tc.kind {
				t.Errorf("type/kind = %q/%q, want %q/%q", msg.MessageType, msg.Moderation, MessageTypeModeration, tc.kind)
			}
			if msg.TargetUserID != tc.userID || msg.TargetLogin != tc.login {
				t.Errorf("target = user %q login %q, want %q / %q", msg.TargetUserID, msg.TargetLogin, tc.userID, tc.login)
			}
			if msg.BanDurationSec != tc.dur {
				t.Errorf("BanDurationSec = %d, want %d", msg.BanDurationSec, tc.dur)
			}
			if msg.ID != tc.id {
				t.Errorf("ID = %q, want %q", msg.ID, tc.id)
			}
			if msg.SystemMsg != tc.sentence {
				t.Errorf("SystemMsg = %q, want %q", msg.SystemMsg, tc.sentence)
			}
			if msg.Raw != tc.line || msg.Message != "" || msg.AuthorID != "" {
				t.Errorf("Raw/Message/AuthorID = %q/%q/%q", msg.Raw, msg.Message, msg.AuthorID)
			}
		})
	}
}

// TestClearmsgWithoutATargetIsDropped: a deletion that names no message can
// mark nothing, and its derived id would collide with every other such line.
func TestClearmsgWithoutATargetIsDropped(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	if msg := cd.parseLine(`@login=ronni;tmi-sent-ts=1700000005000 :tmi.twitch.tv CLEARMSG #testchan :x`); msg != nil {
		t.Errorf("parseLine = %+v, want nil for a CLEARMSG with no target-msg-id", msg)
	}
}

// TestRoomstateIsStillDropped: the new cases must not widen what is stored.
func TestRoomstateIsStillDropped(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	if msg := cd.parseLine(`@emote-only=0;room-id=1 :tmi.twitch.tv ROOMSTATE #testchan`); msg != nil {
		t.Errorf("parseLine = %+v, want nil for ROOMSTATE", msg)
	}
}

// TestModerationRowsAreCountedAndWrittenLikeAnyRow is the verifier's shape
// (b): the row goes through addMessage, so the running count, the file's
// messageCount and the array all agree with no reconciliation of their own,
// and a repeat of the same event is deduplicated by its derived id.
//
// Mutant: storing the row outside addMessage (the count stays at 1).
func TestModerationRowsAreCountedAndWrittenLikeAnyRow(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	lines := []string{
		`@id=pm-1;user-id=42;display-name=Ronni;tmi-sent-ts=1700000000500 :ronni!ronni@ronni.tmi.twitch.tv PRIVMSG #testchan :spam`,
		`@ban-duration=600;room-id=1;target-user-id=42;tmi-sent-ts=1700000001000 :tmi.twitch.tv CLEARCHAT #testchan :ronni`,
		`@ban-duration=600;room-id=1;target-user-id=42;tmi-sent-ts=1700000001000 :tmi.twitch.tv CLEARCHAT #testchan :ronni`,
	}
	for _, l := range lines {
		if msg := cd.parseLine(l); msg != nil {
			cd.addMessage(msg)
		}
	}
	if got := cd.MessageCount(); got != 2 {
		t.Fatalf("MessageCount = %d, want 2 (the message and one moderation row)", got)
	}
	if err := cd.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	path := cd.currentOutputPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		MessageCount int              `json:"messageCount"`
		Messages     []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("chat file does not parse: %v", err)
	}
	if data.MessageCount != 2 || len(data.Messages) != 2 {
		t.Fatalf("messageCount = %d, rows = %d, want 2 and 2", data.MessageCount, len(data.Messages))
	}
	row := data.Messages[1]
	if row["messageType"] != "moderation" || row["moderation"] != "author_deleted" ||
		row["targetUserId"] != "42" || row["targetLogin"] != "ronni" || row["banDurationSec"] != float64(600) {
		t.Errorf("moderation row on disk = %v", row)
	}
	// An ordinary message gains no field from this change.
	for _, k := range []string{"moderation", "targetMsgId", "targetUserId", "targetLogin", "banDurationSec"} {
		if _, ok := data.Messages[0][k]; ok {
			t.Errorf("an ordinary message carries %q", k)
		}
	}
	if r, _ := row["raw"].(string); r != lines[1] {
		t.Errorf("the moderation row raw = %q, want the verbatim line", r)
	}
}

func TestFormatBanDuration(t *testing.T) {
	for sec, want := range map[int]string{1: "1s", 59: "59s", 60: "1m", 600: "10m", 3600: "1h", 5400: "1h 30m", 86400: "1d", 1209600: "14d", 90061: "1d 1h 1m 1s"} {
		if got := formatBanDuration(sec); got != want {
			t.Errorf("formatBanDuration(%d) = %q, want %q", sec, got, want)
		}
	}
}
