package tui

import (
	"image/color"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// --- extractYouTubeVideoID ---

func TestExtractYouTubeVideoID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Direct 11-char ID
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Standard watch URL
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtube.com/watch?v=dQw4w9WgXcQ&t=10", "dQw4w9WgXcQ"},
		// Short URL
		{"https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ?t=5", "dQw4w9WgXcQ"},
		// Live URL
		{"https://www.youtube.com/live/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Shorts URL
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Embed URL
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Trailing slash
		{"https://youtu.be/dQw4w9WgXcQ/", "dQw4w9WgXcQ"},
		// Empty
		{"", ""},
		// Invalid (too short)
		{"dQw4w9W", ""},
	}

	for _, tc := range tests {
		got := extractYouTubeVideoID(tc.input)
		if got != tc.want {
			t.Errorf("extractYouTubeVideoID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- extractTwitchTarget ---

func TestExtractTwitchTarget(t *testing.T) {
	tests := []struct {
		input    string
		wantID   string
		wantType string
	}{
		// Channel URL
		{"https://www.twitch.tv/shroud", "shroud", "twitch"},
		{"https://twitch.tv/shroud", "shroud", "twitch"},
		{"twitch.tv/shroud", "shroud", "twitch"},
		// VOD URL
		{"https://www.twitch.tv/videos/1234567890", "tw_v1234567890", "twitch"},
		// Login/video/{id} format
		{"https://www.twitch.tv/shroud/video/1234567890", "tw_v1234567890", "twitch"},
		// Clip URL (clips subdomain)
		{"https://clips.twitch.tv/SomeClipSlug", "", "twitch_clip"},
		// Clip URL (login/clip/slug)
		{"https://www.twitch.tv/shroud/clip/SomeSlug", "", "twitch_clip"},
		// Raw VOD with v prefix
		{"v1234567890", "tw_v1234567890", "twitch"},
		// Raw numeric VOD (7-12 digits)
		{"1234567890", "tw_v1234567890", "twitch"},
		// Raw username
		{"shroud", "shroud", "twitch"},
		// Reserved path (not a channel)
		{"https://twitch.tv/directory", "", ""},
		// Empty
		{"", "", ""},
	}

	for _, tc := range tests {
		gotID, gotType := extractTwitchTarget(tc.input)
		if gotID != tc.wantID || gotType != tc.wantType {
			t.Errorf("extractTwitchTarget(%q) = (%q, %q), want (%q, %q)",
				tc.input, gotID, gotType, tc.wantID, tc.wantType)
		}
	}
}

// --- extractMediaID ---

func TestExtractMediaID(t *testing.T) {
	tests := []struct {
		input    string
		wantID   string
		wantType string
	}{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ", "youtube"},
		{"https://twitch.tv/shroud", "shroud", "twitch"},
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ", "youtube"},
		{"", "", ""},
	}

	for _, tc := range tests {
		gotID, gotType := extractMediaID(tc.input)
		if gotID != tc.wantID || gotType != tc.wantType {
			t.Errorf("extractMediaID(%q) = (%q, %q), want (%q, %q)",
				tc.input, gotID, gotType, tc.wantID, tc.wantType)
		}
	}
}

// --- parseTimeToSeconds ---

func TestParseTimeToSeconds(t *testing.T) {
	tests := []struct {
		input   string
		want    float64
		wantErr bool
	}{
		// Raw seconds
		{"120", 120, false},
		{"0", 0, false},
		// MM:SS
		{"1:30", 90, false},
		{"0:05", 5, false},
		{"59:59", 3599, false},
		// HH:MM:SS
		{"1:00:00", 3600, false},
		{"1:30:45", 5445, false},
		{"0:01:05", 65, false},
		// Errors
		{"", 0, true},
		{"abc", 0, true},
		{"1:60", 0, true},    // seconds > 59
		{"1:00:60", 0, true}, // seconds > 59
	}

	for _, tc := range tests {
		got, err := parseTimeToSeconds(tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseTimeToSeconds(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseTimeToSeconds(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- feedbackColor ---

func TestFeedbackColor(t *testing.T) {
	yellow := lipgloss.Color("#f1c40f")
	tests := []struct {
		msg       string
		wantColor color.Color
	}{
		// Chord feedback (yellow)
		{"Press D to confirm delete", yellow},
		{"Action: A Add | I Import", yellow},
		// Error (red)
		{"Cancelled: Some Job", ColorRed},
		{"Update check failed: timeout", ColorRed},
		{"Invalid Chord: A X", ColorRed},
		// Warning (yellow)
		{"Already up to date", yellow},
		{"No stream URL available", yellow},
		// Deletion (gray)
		{"Deleted: Some Job", ColorGray},
		// Success (green, default)
		{"Imported: archive", ColorGreen},
		{"Added to queue", ColorGreen},
	}

	for _, tc := range tests {
		got := feedbackColor(tc.msg)
		if got != tc.wantColor {
			t.Errorf("feedbackColor(%q) = %v, want %v", tc.msg, got, tc.wantColor)
		}
	}
}

// --- streamURL ---

func TestStreamURL(t *testing.T) {
	tests := []struct {
		name string
		job  *database.Job
		want string
	}{
		{"direct URL", &database.Job{URL: "https://example.com/stream"}, "https://example.com/stream"},
		{"youtube videoID", &database.Job{VideoID: "dQw4w9WgXcQ"}, "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		{"twitch VOD", &database.Job{VideoID: "tw_v1234567890", Platform: "twitch", IsVod: true}, "https://www.twitch.tv/videos/1234567890"},
		{"twitch channel", &database.Job{VideoID: "shroud", Platform: "twitch", ChannelName: "shroud"}, "https://www.twitch.tv/shroud"},
		{"twitch no channelName", &database.Job{VideoID: "123", Platform: "twitch"}, ""},
		{"empty", &database.Job{}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := streamURL(tc.job)
			if got != tc.want {
				t.Errorf("streamURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- isCompletedStatus ---

func TestIsCompletedStatus(t *testing.T) {
	tests := []struct {
		status database.JobStatus
		want   bool
	}{
		{database.StatusFinished, true},
		{database.StatusCancelled, true},
		{database.StatusDownloading, false},
		{database.StatusError, false},
		{database.StatusLive, false},
		{database.StatusUpcoming, false},
	}

	for _, tc := range tests {
		got := isCompletedStatus(tc.status)
		if got != tc.want {
			t.Errorf("isCompletedStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// --- hasDisplayChange (DECISIONS #21 / audit tui.md F20) ---

func TestHasDisplayChange(t *testing.T) {
	tests := []struct {
		name    string
		changes []string
		want    bool
	}{
		{"empty changes", nil, false},
		{"empty slice", []string{}, false},
		{"progress-only update (10/sec path)", []string{"progress", "percent", "speed", "eta", "last_video_seq", "last_audio_seq"}, false},
		{"silent column slipped through (shouldn't reach here, defensive)", []string{"resume_position", "chat_offset"}, false},
		{"status transition", []string{"status"}, true},
		{"title rename", []string{"title"}, true},
		{"channel name rename", []string{"channel_name"}, true},
		{"thumbnail update", []string{"thumbnail_url"}, true},
		{"description update", []string{"description"}, true},
		{"stream start/end timestamps", []string{"stream_start_time", "stream_end_time"}, true},
		{"error message attached", []string{"error"}, true},
		{"output file finalized", []string{"output_file", "filename"}, true},
		{"is_vod flag flipped", []string{"is_vod"}, true},
		{"chat status changed", []string{"chat_status"}, true},
		{"mixed: progress + status (status wins)", []string{"progress", "status", "eta"}, true},
		{"mixed: progress + error (error wins)", []string{"percent", "error"}, true},
		{"unknown column ignored", []string{"some_unknown_column"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasDisplayChange(tc.changes)
			if got != tc.want {
				t.Errorf("hasDisplayChange(%v) = %v, want %v", tc.changes, got, tc.want)
			}
		})
	}
}

// TestDisplayColumnsCoverage guards against drift between handleJobUpdate's
// pre-migration 12-field compare and the column-set check that replaced it.
// If a future audit recommends skipping a column (or adding one), update
// both this test and the displayColumns map together.
func TestDisplayColumnsCoverage(t *testing.T) {
	want := []string{
		"status", "title", "channel_name", "thumbnail_url", "description",
		"stream_start_time", "stream_end_time", "error",
		"output_file", "filename", "is_vod", "chat_status",
	}
	if len(displayColumns) != len(want) {
		t.Fatalf("displayColumns size = %d, want %d (drift from handleJobUpdate's pre-migration field set)", len(displayColumns), len(want))
	}
	for _, col := range want {
		if _, ok := displayColumns[col]; !ok {
			t.Errorf("displayColumns missing %q", col)
		}
	}
}
