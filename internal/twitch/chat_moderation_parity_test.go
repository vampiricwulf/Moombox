package twitch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// moderationParityFixture is the table the Go recorder and the JS player
// share (CLAUDE.md, State machines rule 2). Each case is a run of IRC lines,
// the rows parseLine makes of them exactly as they are marshalled into a chat
// file, and what the player's load pass must make of those rows.
//
// This test owns "messages": it fails when parseLine's rows are no longer the
// fixture's bytes. web/tests/twitch-moderation.test.mjs owns "want": it runs
// applyTwitchModeration over the same rows. A JSON key or a vocabulary word
// renamed on the Go side therefore fails here until the fixture is updated,
// and the updated fixture fails the JS suite until the player reads the new
// word.
const moderationParityFixture = "../../web/tests/fixtures/twitch-moderation-parity.json"

func TestModerationRowsMatchTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(moderationParityFixture))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string            `json:"name"`
			Lines    []string          `json:"lines"`
			Messages []json.RawMessage `json:"messages"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	kinds := map[ModerationKind]bool{}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cd := newSidecarTestChatDownloader(t)
			var rows []*TwitchChatMessage
			for _, line := range tc.Lines {
				msg := cd.parseLine(line)
				if msg == nil {
					t.Fatalf("parseLine returned nil for %q", line)
				}
				// The offset is the recorder's clock, not the wire's.
				msg.OffsetMs = 0
				rows = append(rows, msg)
				if msg.MessageType == MessageTypeModeration {
					kinds[msg.Moderation] = true
				}
			}
			if len(rows) != len(tc.Messages) {
				got, _ := json.MarshalIndent(rows, "      ", "  ")
				t.Fatalf("parseLine made %d rows, the fixture has %d. The rows:\n%s", len(rows), len(tc.Messages), got)
			}
			for i, row := range rows {
				got, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				var want bytes.Buffer
				if err := json.Compact(&want, tc.Messages[i]); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want.Bytes()) {
					t.Errorf("row %d differs from the fixture.\n got: %s\nwant: %s\n"+
						"If the change is meant, update the fixture's row and the player (applyTwitchModeration, chat-timeline.js).",
						i, got, want.Bytes())
				}
			}
		})
	}
	for _, k := range []ModerationKind{ModerationDeleted, ModerationAuthorDeleted, ModerationChatCleared} {
		if !kinds[k] {
			t.Errorf("no fixture case produces a %q row", k)
		}
	}
}
