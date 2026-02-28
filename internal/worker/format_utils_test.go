package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

func intPtr(v int) *int { return &v }

func TestIsProgressiveFormat(t *testing.T) {
	tests := []struct {
		name     string
		format   youtube.Format
		expected bool
	}{
		{
			name: "progressive format with audio and video dimensions",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_MEDIUM",
				Width:        intPtr(1920),
				Height:       intPtr(1080),
			},
			expected: true,
		},
		{
			name: "audio only - no width or height",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_MEDIUM",
			},
			expected: false,
		},
		{
			name: "video only - no audio quality",
			format: youtube.Format{
				Width:  intPtr(1920),
				Height: intPtr(1080),
			},
			expected: false,
		},
		{
			name: "empty format",
			format: youtube.Format{},
			expected: false,
		},
		{
			name: "audio quality set but only width",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_LOW",
				Width:        intPtr(640),
			},
			expected: false,
		},
		{
			name: "audio quality set but only height",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_LOW",
				Height:       intPtr(480),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsProgressiveFormat(&tt.format)
			if result != tt.expected {
				t.Errorf("IsProgressiveFormat() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestSelectBestDashStream(t *testing.T) {
	tests := []struct {
		name        string
		streams     []DashStreamInfo
		preferItag  int
		maxRes      int
		isVideo     bool
		expectedIdx int // -1 means nil expected
	}{
		{
			name:        "empty streams returns nil",
			streams:     nil,
			preferItag:  0,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: -1,
		},
		{
			name: "prefer itag selects exact match",
			streams: []DashStreamInfo{
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  136,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 2,
		},
		{
			name: "prefer itag not found falls through to bandwidth selection",
			streams: []DashStreamInfo{
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
			},
			preferItag:  999,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "highest bandwidth video selected",
			streams: []DashStreamInfo{
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  0,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "highest bandwidth audio selected",
			streams: []DashStreamInfo{
				{Itag: 140, MimeType: "audio/mp4", Bandwidth: 128000},
				{Itag: 141, MimeType: "audio/mp4", Bandwidth: 256000},
				{Itag: 139, MimeType: "audio/mp4", Bandwidth: 48000},
			},
			preferItag:  0,
			maxRes:      0,
			isVideo:     false,
			expectedIdx: 1,
		},
		{
			name: "video filter excludes audio streams",
			streams: []DashStreamInfo{
				{Itag: 140, MimeType: "audio/mp4", Bandwidth: 9999999},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			preferItag:  0,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "audio filter excludes video streams",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 9999999},
				{Itag: 140, MimeType: "audio/mp4", Bandwidth: 128000},
			},
			preferItag:  0,
			maxRes:      0,
			isVideo:     false,
			expectedIdx: 1,
		},
		{
			name: "maxRes filters out high resolution streams",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			preferItag:  0,
			maxRes:      1280,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "maxRes uses max of width and height",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			preferItag:  0,
			maxRes:      1000,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "maxRes zero means no limit",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 3840, Height: 2160, Bandwidth: 8000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  0,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "all streams filtered out returns nil",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  0,
			maxRes:      360,
			isVideo:     true,
			expectedIdx: -1,
		},
		{
			name: "maxRes does not apply to audio",
			streams: []DashStreamInfo{
				{Itag: 140, MimeType: "audio/mp4", Width: 0, Height: 0, Bandwidth: 128000},
			},
			preferItag:  0,
			maxRes:      360,
			isVideo:     false,
			expectedIdx: 0,
		},
		{
			name: "prefer itag ignores type filter",
			streams: []DashStreamInfo{
				{Itag: 140, MimeType: "audio/mp4", Bandwidth: 128000},
			},
			preferItag:  140,
			maxRes:      0,
			isVideo:     true,
			expectedIdx: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SelectBestDashStream(tt.streams, tt.preferItag, tt.maxRes, tt.isVideo)
			if tt.expectedIdx == -1 {
				if result != nil {
					t.Errorf("SelectBestDashStream() = %+v, want nil", result)
				}
				return
			}
			if result == nil {
				t.Errorf("SelectBestDashStream() = nil, want stream at index %d", tt.expectedIdx)
				return
			}
			expected := &tt.streams[tt.expectedIdx]
			if result.Itag != expected.Itag {
				t.Errorf("SelectBestDashStream().Itag = %d, want %d", result.Itag, expected.Itag)
			}
		})
	}
}
