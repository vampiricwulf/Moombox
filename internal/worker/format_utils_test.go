package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

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
				Width:        new(1920),
				Height:       new(1080),
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
				Width:  new(1920),
				Height: new(1080),
			},
			expected: false,
		},
		{
			name:     "empty format",
			format:   youtube.Format{},
			expected: false,
		},
		{
			name: "audio quality set but only width",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_LOW",
				Width:        new(640),
			},
			expected: false,
		},
		{
			name: "audio quality set but only height",
			format: youtube.Format{
				AudioQuality: "AUDIO_QUALITY_LOW",
				Height:       new(480),
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
		qualityPref string
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
			name: "the cap compares the short edge, so 1280 admits 1920x1080",
			// Mutant: long edge restored — idx 1 (1280x720) is selected,
			// because 1920 > 1280 dropped the 1080p stream from the list.
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			preferItag:  0,
			maxRes:      1280,
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "maxRes uses the SHORTER of width and height",
			// 1080 > 1000 and 360 <= 1000, so the 360p stream is the only one
			// at or below the cap. Mutant: long edge restored — 1920 and 640
			// are compared instead, and the answer is the same by accident, so
			// this row is kept only as the companion to the one above.
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
			name: "nothing at or below the cap picks the closest above",
			// Mutant: nil restored (the pre-arc behaviour) — expectedIdx -1.
			// Mutant: closest-above replaced by largest — idx 0 (1080p).
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  0,
			maxRes:      360,
			isVideo:     true,
			expectedIdx: 1,
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

		// Quality preference tests
		{
			name: "quality pref targets exact height",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			qualityPref: "720p",
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "quality pref targets height+fps",
			streams: []DashStreamInfo{
				{Itag: 299, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 60, Bandwidth: 6000000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, FPS: 30, Bandwidth: 2500000},
			},
			qualityPref: "1080p60",
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "quality pref 1080p gets highest bandwidth at 1080",
			streams: []DashStreamInfo{
				{Itag: 299, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 60, Bandwidth: 6000000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, FPS: 30, Bandwidth: 2500000},
			},
			qualityPref: "1080p",
			isVideo:     true,
			expectedIdx: 0, // highest bandwidth among 1080p matches
		},
		{
			name: "quality pref descends to next lower height when target not found",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			qualityPref: "480p",
			isVideo:     true,
			expectedIdx: 2, // 480p not found, next lower is 360p
		},
		{
			name: "quality pref falls back to source when no lower heights",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			qualityPref: "360p",
			isVideo:     true,
			expectedIdx: 0, // no lower heights, fallback to source/best
		},
		{
			name: "quality pref is honoured within the cap",
			// The cap resolves to 1080 (the largest short edge at or below
			// 1280), the preference then picks 1080p from everything at or
			// below that. Mutant: long edge restored — 1080p is filtered out
			// and idx 1 is returned. Mutant: the preference restricted to the
			// chosen size only — a "360p" preference would stop working.
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			qualityPref: "1080p",
			maxRes:      1280,
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "a lower quality pref still works under a high cap",
			// The rung R1 must not eat: the cap resolves to 1080, but a job
			// that asked for 360p gets 360p. Mutant: the default path's
			// chosen-size restriction applied to the preference path too —
			// idx 0 is returned.
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 134, MimeType: "video/mp4", Width: 640, Height: 360, Bandwidth: 500000},
			},
			qualityPref: "360p",
			maxRes:      2160,
			isVideo:     true,
			expectedIdx: 1,
		},
		{
			name: "3840x2160 under the shipped default cap",
			// Spec §3's per-site matrix, and the headline of ruling R1: the
			// default of 2160 used to REJECT 4K outright (long edge 3840) and
			// hand back the 1080p stream. Mutant: long edge restored — idx 1.
			streams: []DashStreamInfo{
				{Itag: 401, MimeType: "video/mp4", Width: 3840, Height: 2160, Bandwidth: 18000000},
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
			},
			preferItag:  0,
			maxRes:      2160,
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "2160x3840 portrait under the shipped default cap",
			// The same frame the other way round reads the same. Mutant: long
			// edge restored — 3840 > 2160 drops the portrait source while the
			// 720x1280 transcode (long edge 1280) survives, and idx 1 comes
			// back.
			streams: []DashStreamInfo{
				{Itag: 400, MimeType: "video/mp4", Width: 2160, Height: 3840, Bandwidth: 18000000},
				{Itag: 136, MimeType: "video/mp4", Width: 720, Height: 1280, Bandwidth: 2500000},
			},
			preferItag:  0,
			maxRes:      2160,
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "quality pref best acts like no preference",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			qualityPref: "best",
			isVideo:     true,
			expectedIdx: 0,
		},
		{
			name: "quality pref ignored for audio streams",
			streams: []DashStreamInfo{
				{Itag: 140, MimeType: "audio/mp4", Bandwidth: 128000},
				{Itag: 141, MimeType: "audio/mp4", Bandwidth: 256000},
			},
			qualityPref: "720p",
			isVideo:     false,
			expectedIdx: 1, // highest bandwidth, pref ignored
		},
		{
			name: "quality pref 1080p60 falls back to 1080p30 when no 60fps available",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, FPS: 30, Bandwidth: 2500000},
			},
			qualityPref: "1080p60",
			isVideo:     true,
			expectedIdx: 0, // height matches, FPS doesn't but still returns height match
		},
		{
			name: "prefer itag overrides quality pref",
			streams: []DashStreamInfo{
				{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 4000000},
				{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, Bandwidth: 2500000},
			},
			preferItag:  136,
			qualityPref: "1080p",
			isVideo:     true,
			expectedIdx: 1, // itag override takes priority
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SelectBestDashStream(tt.streams, tt.preferItag, tt.maxRes, tt.isVideo, tt.qualityPref, true)
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
