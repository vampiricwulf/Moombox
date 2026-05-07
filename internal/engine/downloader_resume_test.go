package engine

import "testing"

func TestStreamIdentity_DashVideoplaybackURL(t *testing.T) {
	// Real shape from a YouTube live-stream resume warning. Identity must
	// extract videoID + itag and ignore the rotating session params.
	url := "https://rr2---sn-bvvbaxivnuxqjvhj5nu-n4vk.googlevideo.com/videoplayback/expire/1778054817/ei/QaL6aezONYCLsfIPn4u9mQk/ip/2601:647:c180:cad0:df57:8a0a:6e53:c854/id/BAKBgY4-rLs.1/itag/140/source/yt_live_broadcast/n/7Zq_f1YlpCUoHg/sig/AHEqNM4w/"
	got := streamIdentity(url)
	want := "BAKBgY4-rLs/140"
	if got != want {
		t.Errorf("streamIdentity(dash) = %q, want %q", got, want)
	}
}

func TestStreamIdentity_HlsVariantURL(t *testing.T) {
	url := "https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1778140939/ei/abc/id/ogQLaiRvZUQ.1/itag/301/source/yt_live_broadcast/n/r0pF3fE/playlist/index.m3u8/pot/Mt"
	got := streamIdentity(url)
	want := "ogQLaiRvZUQ/301"
	if got != want {
		t.Errorf("streamIdentity(hls) = %q, want %q", got, want)
	}
}

func TestStreamIdentity_DifferentSessionParams_SameIdentity(t *testing.T) {
	// Same logical stream, different session params (mirrors what every
	// post-restart resume sees). Identities must match so the resume URL
	// check doesn't throw away precise BytesWritten state.
	saved := "https://rr1---sn-abc.googlevideo.com/videoplayback/expire/1000/ei/old/id/foo123.1/itag/140/n/old/sig/old/"
	current := "https://rr2---sn-xyz.googlevideo.com/videoplayback/expire/2000/ei/new/id/foo123.1/itag/140/n/new/sig/new/"
	if streamIdentity(saved) != streamIdentity(current) {
		t.Errorf("expected identical identities; saved=%q current=%q",
			streamIdentity(saved), streamIdentity(current))
	}
}

func TestStreamIdentity_DifferentItags_DifferentIdentity(t *testing.T) {
	a := "https://x/videoplayback/id/foo123.1/itag/140/source/live/"
	b := "https://x/videoplayback/id/foo123.1/itag/299/source/live/"
	if streamIdentity(a) == streamIdentity(b) {
		t.Errorf("expected different identities for different itags; both = %q",
			streamIdentity(a))
	}
}

func TestStreamIdentity_NoMatch(t *testing.T) {
	// Non-YouTube URLs should return "" so the caller falls back to
	// full-URL equality (preserves the strict check for unfamiliar hosts).
	got := streamIdentity("https://example.com/video.mp4")
	if got != "" {
		t.Errorf("expected empty identity for non-YouTube URL, got %q", got)
	}
}
