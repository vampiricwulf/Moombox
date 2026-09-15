package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// chatRow returns the details panel's "Chat" row, or nil.
func chatRow(m *JobDetailsModel) *detailRow {
	for i := range m.rows {
		if m.rows[i].kind == rowField && m.rows[i].label == "Chat" {
			return &m.rows[i]
		}
	}
	return nil
}

// TestChatStatusColorSeparatesIncompleteFromComplete is the substring trap.
//
// chatStatusColor matches on Contains, and "incomplete" CONTAINS "complete" —
// so the arm for a completed archive swallows the value for a truncated one
// unless the incomplete test runs first.
//
// Mutant: move the incomplete branch below the "finished"/"complete" branch. A
// short archive then renders in exactly the cyan a complete one does, which is
// the misreport this whole arc exists to end.
func TestChatStatusColorSeparatesIncompleteFromComplete(t *testing.T) {
	m := NewJobDetailsModel()
	if got := m.chatStatusColor("incomplete"); got != ColorWarning {
		t.Errorf("chatStatusColor(\"incomplete\") = %v, want ColorWarning (%v) — \"incomplete\" "+
			"contains \"complete\", so the order of the branches is the whole test", got, ColorWarning)
	}
	if got := m.chatStatusColor("finished"); got != ColorFinished {
		t.Errorf("chatStatusColor(\"finished\") = %v, want ColorFinished (%v)", got, ColorFinished)
	}
	if got := m.chatStatusColor("downloading"); got != ColorGreen {
		t.Errorf("chatStatusColor(\"downloading\") = %v, want ColorGreen (%v)", got, ColorGreen)
	}
	if got := m.chatStatusColor("unavailable"); got != ColorGray {
		t.Errorf("chatStatusColor(\"unavailable\") = %v, want ColorGray (%v)", got, ColorGray)
	}
}

// TestFinishedJobShowsAnIncompleteChatVerdict closes the parity gap the Web UI
// never had: the panel's Chat row lives in the Progress section, which is gated
// on isActiveState, so a FINISHED job showed no chat row at all — and the one
// verdict an operator has to act on was the one the TUI could not say.
//
// Mutants this kills:
//   - leaving the row gated on isActiveState (today): chatRow is nil and the
//     TUI never reports the stall while the Web badge does.
//   - lifting the row unconditionally for terminal jobs: the second subtest
//     fails, because every finished job in the fleet would grow a new row.
//   - formatting the value differently from the Web badge's text and count.
func TestFinishedJobShowsAnIncompleteChatVerdict(t *testing.T) {
	count := 4211
	newModel := func(status string) *JobDetailsModel {
		m := NewJobDetailsModel()
		m.SetSize(80, 24)
		m.SetJob(&database.Job{
			ID:                "j1",
			Title:             "A VOD",
			Platform:          "twitch",
			Status:            database.StatusFinished,
			ChatStatus:        status,
			TotalChatMessages: &count,
		})
		return m
	}

	t.Run("incomplete", func(t *testing.T) {
		row := chatRow(newModel("incomplete"))
		if row == nil {
			t.Fatal("a Finished job whose chat stopped short has no Chat row — the Web details " +
				"badge shows \"incomplete\" and the TUI shows nothing at all")
		}
		if row.value != "incomplete (4211 messages)" {
			t.Errorf("Chat row value = %q, want %q (same value and count the Web badge renders)",
				row.value, "incomplete (4211 messages)")
		}
		if row.color != ColorWarning {
			t.Errorf("Chat row color = %v, want ColorWarning (%v)", row.color, ColorWarning)
		}
	})

	t.Run("finished stays quiet", func(t *testing.T) {
		if row := chatRow(newModel("finished")); row != nil {
			t.Errorf("a Finished job with a complete chat grew a Chat row (%q) — the terminal row "+
				"is for the one verdict that asks for action, not for every finished job", row.value)
		}
	})

	// The ACTIVE job's Chat row comes from the Progress section, which is a
	// DIFFERENT call site from the terminal row above — and it is reachable:
	// recordChatOutcome (internal/worker) writes the verdict off the chat
	// goroutine's own exit, independent of the video capture, so a Twitch IRC
	// session that exhausts its reconnect budget mid-stream shows "incomplete"
	// here while the download runs on.
	//
	// Mutant this kills: a refactor that special-cases the terminal path and
	// leaves the Progress site on some other colour rule — e.g. the shared
	// StatusColor, which knows job statuses and not chat ones, so "incomplete"
	// falls to its default ColorWhite. The colour unit test and the terminal
	// subtest both stay green under that; this one does not.
	t.Run("an active job shows the same row", func(t *testing.T) {
		m := NewJobDetailsModel()
		m.SetSize(80, 24)
		m.SetJob(&database.Job{
			ID:                "j2",
			Title:             "A live stream",
			Platform:          "twitch",
			Status:            database.StatusDownloading,
			ChatStatus:        "incomplete",
			TotalChatMessages: &count,
		})

		row := chatRow(m)
		if row == nil {
			t.Fatal("a running job whose chat gave up has no Chat row — the Progress section " +
				"shows one for every other chat status")
		}
		if row.value != "incomplete (4211 messages)" {
			t.Errorf("Chat row value = %q, want %q — the same text the terminal row and the Web "+
				"badge render", row.value, "incomplete (4211 messages)")
		}
		if row.color != ColorWarning {
			t.Errorf("Chat row color = %v, want ColorWarning (%v) — a chat that stopped short "+
				"must not read as a healthy one just because the video is still downloading",
				row.color, ColorWarning)
		}
	})
}
