package worker

import (
	"testing"
)

func TestFormatQualityLabel(t *testing.T) {
	tests := []struct {
		height   int
		fps      int
		expected string
	}{
		{1080, 60, "1080p60"},
		{1080, 30, "1080p"},
		{720, 0, "720p"},
		{720, 24, "720p"},
		{720, 31, "720p31"},
		{0, 60, "unknown"},
		{-1, 0, "unknown"},
		{2160, 60, "2160p60"},
		{480, 30, "480p"},
	}

	for _, tt := range tests {
		result := FormatQualityLabel(tt.height, tt.fps)
		if result != tt.expected {
			t.Errorf("FormatQualityLabel(%d, %d) = %q, want %q", tt.height, tt.fps, result, tt.expected)
		}
	}
}

func TestParseQualityPreference(t *testing.T) {
	tests := []struct {
		input      string
		wantHeight int
		wantFPS    int
	}{
		{"1080p60", 1080, 60},
		{"1080p", 1080, 0},
		{"720p", 720, 0},
		{"720p30", 720, 30},
		{"best", 0, 0},
		{"", 0, 0},
		{"audio_only", -1, 0},
		{"480", 480, 0},
		{"invalid", 0, 0},
		{"2160p60", 2160, 60},
		{"360p", 360, 0},
	}

	for _, tt := range tests {
		height, fps := ParseQualityPreference(tt.input)
		if height != tt.wantHeight || fps != tt.wantFPS {
			t.Errorf("ParseQualityPreference(%q) = (%d, %d), want (%d, %d)",
				tt.input, height, fps, tt.wantHeight, tt.wantFPS)
		}
	}
}

func TestQualityInfoChanged(t *testing.T) {
	tests := []struct {
		name    string
		a, b    QualityInfo
		changed bool
	}{
		{
			name:    "same resolution different fps",
			a:       QualityInfo{Width: 1920, Height: 1080, FPS: 30},
			b:       QualityInfo{Width: 1920, Height: 1080, FPS: 60},
			changed: false, // FPS-only changes are ignored
		},
		{
			name:    "different height",
			a:       QualityInfo{Width: 1920, Height: 1080, FPS: 60},
			b:       QualityInfo{Width: 1280, Height: 720, FPS: 60},
			changed: true,
		},
		{
			name:    "different width",
			a:       QualityInfo{Width: 1920, Height: 1080},
			b:       QualityInfo{Width: 1280, Height: 1080},
			changed: true,
		},
		{
			name:    "identical",
			a:       QualityInfo{Width: 1920, Height: 1080, FPS: 60},
			b:       QualityInfo{Width: 1920, Height: 1080, FPS: 60},
			changed: false,
		},
		{
			name:    "zero to non-zero",
			a:       QualityInfo{},
			b:       QualityInfo{Width: 1920, Height: 1080},
			changed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Changed(tt.b)
			if got != tt.changed {
				t.Errorf("QualityInfo.Changed() = %v, want %v", got, tt.changed)
			}
		})
	}
}

func TestQualityInfo_SameQualityAfterTransientError(t *testing.T) {
	// Simulates the scenario where ErrQualityLost fires but re-fetched quality is identical.
	// The orchestrator should NOT split when Changed() returns false.
	current := QualityInfo{Width: 1920, Height: 1080, FPS: 60, Label: "1080p60"}
	refetched := QualityInfo{Width: 1920, Height: 1080, FPS: 60, Label: "1080p60"}
	if current.Changed(refetched) {
		t.Error("Changed() should return false for identical quality — spurious split would occur")
	}
}

func TestQualityInfo_DifferentQualityAfterRealChange(t *testing.T) {
	current := QualityInfo{Width: 1920, Height: 1080, FPS: 60, Label: "1080p60"}
	refetched := QualityInfo{Width: 1280, Height: 720, FPS: 60, Label: "720p60"}
	if !current.Changed(refetched) {
		t.Error("Changed() should return true for different resolution — split should occur")
	}
}
