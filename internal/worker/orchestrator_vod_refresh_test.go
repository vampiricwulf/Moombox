package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

func TestComputeIncompleteTail(t *testing.T) {
	if !computeIncompleteTail(true, false) || !computeIncompleteTail(false, true) {
		t.Error("either downloader behind head must flag the job")
	}
	if computeIncompleteTail(false, false) {
		t.Error("clean finish must not flag")
	}
}

func TestVodRefreshDecision(t *testing.T) {
	cases := []struct {
		name                       string
		behindHead, progressed     bool
		attempt                    int
		manifestlessStillAvailable bool
		want                       bool
	}{
		{"incomplete with progress refreshes", true, true, 1, true, true},
		{"complete finalize stops", false, true, 1, true, false},
		{"no progress stops (avoid API spin)", true, false, 1, true, false},
		{"attempts exhausted stops", true, true, maxVodRefreshAttempts, true, false},
		{"stream became true VOD stops", true, true, 1, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldRefreshVodDownload(c.behindHead, c.progressed, c.attempt, c.manifestlessStillAvailable)
			if got != c.want {
				t.Errorf("shouldRefreshVodDownload = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRefreshFormatMatches guards the VOD refresh loop's mixed-codec-append
// prevention: refreshDownload re-runs format selection against a freshly
// re-extracted pool every attempt, and SelectBestDashStream silently falls
// back to a different itag when the previously pinned one has vanished. If
// the fresh selection picked a different video identity than the one
// already on disk, appending fresh's segments under the old init produces a
// silently corrupt mixed-codec file — refreshFormatMatches is the guard
// that must catch that before the caller commits to `fresh`.
func TestRefreshFormatMatches(t *testing.T) {
	itag137 := &youtube.Format{Itag: 137}
	itag248 := &youtube.Format{Itag: 248}
	itag140 := &youtube.Format{Itag: 140}
	itag251 := &youtube.Format{Itag: 251}

	base := func() *DownloadResult {
		return &DownloadResult{
			HasVideo: true, HasAudio: true,
			VideoWidth: 1920, VideoHeight: 1080, VideoFps: 30,
		}
	}

	tests := []struct {
		name string
		old  *DownloadResult
		new  *DownloadResult
		want bool
	}{
		{
			name: "identical format matches",
			old:  base(),
			new:  base(),
			want: true,
		},
		{
			name: "resolution drift does not match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.VideoWidth, r.VideoHeight = 1280, 720
				return r
			}(),
			want: false,
		},
		{
			name: "fps drift does not match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.VideoFps = 60
				return r
			}(),
			want: false,
		},
		{
			name: "video disappearing is a shape change, not a match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.HasVideo = false
				return r
			}(),
			want: false,
		},
		{
			name: "audio disappearing is a shape change, not a match",
			old:  base(),
			new: func() *DownloadResult {
				r := base()
				r.HasAudio = false
				return r
			}(),
			want: false,
		},
		{
			name: "audio-only jobs match on the audio-only shape alone",
			old:  &DownloadResult{HasAudio: true},
			new:  &DownloadResult{HasAudio: true},
			want: true,
		},
		{
			name: "identical resolution but different video itag does not match",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  func() *DownloadResult { r := base(); r.VideoFormat = itag248; return r }(),
			want: false,
		},
		{
			name: "same video itag matches",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			want: true,
		},
		{
			name: "different audio itag does not match even with identical video",
			old:  func() *DownloadResult { r := base(); r.AudioFormat = itag140; return r }(),
			new:  func() *DownloadResult { r := base(); r.AudioFormat = itag251; return r }(),
			want: false,
		},
		{
			name: "one side missing VideoFormat skips itag check (falls back to dims)",
			old:  func() *DownloadResult { r := base(); r.VideoFormat = itag137; return r }(),
			new:  base(),
			want: true,
		},
		{
			name: "nil old is never a match",
			old:  nil,
			new:  base(),
			want: false,
		},
		{
			name: "nil fresh is never a match",
			old:  base(),
			new:  nil,
			want: false,
		},
		{
			name: "both nil is never a match",
			old:  nil,
			new:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refreshFormatMatches(tt.old, tt.new); got != tt.want {
				t.Errorf("refreshFormatMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}
