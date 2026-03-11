package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestExtractTwitchLoginFromJob(t *testing.T) {
	tests := []struct {
		name     string
		job      *database.Job
		expected string
	}{
		{
			name:     "from URL",
			job:      &database.Job{URL: "https://www.twitch.tv/somestreamer"},
			expected: "somestreamer",
		},
		{
			name:     "from URL with path",
			job:      &database.Job{URL: "https://www.twitch.tv/somestreamer/videos"},
			expected: "somestreamer",
		},
		{
			name:     "from URL with query",
			job:      &database.Job{URL: "https://www.twitch.tv/somestreamer?ref=123"},
			expected: "somestreamer",
		},
		{
			name:     "from channel name",
			job:      &database.Job{ChannelName: "SomeStreamer"},
			expected: "somestreamer",
		},
		{
			name:     "from videoID tw_login",
			job:      &database.Job{VideoID: "tw_somestreamer"},
			expected: "somestreamer",
		},
		{
			name:     "from videoID tw_manual_login_timestamp",
			job:      &database.Job{VideoID: "tw_manual_somestreamer_1234567890"},
			expected: "somestreamer",
		},
		{
			name:     "VOD ID returns empty",
			job:      &database.Job{VideoID: "tw_v123456"},
			expected: "",
		},
		{
			name:     "unknown channel name ignored",
			job:      &database.Job{ChannelName: "Unknown", VideoID: "tw_somestreamer"},
			expected: "somestreamer",
		},
		{
			name:     "empty job",
			job:      &database.Job{},
			expected: "",
		},
		{
			name:     "URL takes priority over channel name",
			job:      &database.Job{URL: "https://www.twitch.tv/urllogin", ChannelName: "OtherLogin"},
			expected: "urllogin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractTwitchLoginFromJob(tt.job)
			if result != tt.expected {
				t.Errorf("extractTwitchLoginFromJob() = %q, want %q", result, tt.expected)
			}
		})
	}
}
