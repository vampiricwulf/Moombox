package twitch

import "testing"

// portraitLadder is a vertical broadcast: every rendition is taller than it is
// wide, so its raw height names the size its short edge does not.
func portraitLadder() []TwitchHLSVariant {
	return []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 8000000, Width: 1080, Height: 1920, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 720, Height: 1280, FPS: 60, VideoCodec: "avc1"},
		{Name: "480p30", Bandwidth: 1500000, Width: 480, Height: 854, FPS: 30, VideoCodec: "avc1"},
	}
}

// TestHeightPreferenceMeasuresTheShortEdge: a quality preference names a size
// by the frame's short edge, the measure the cap and rankAtChosenSize use, so
// a portrait stream's 720p is its 720x1280 rendition — at the size named, and
// when the descent comes down to it.
//
// Mutants: selectVariantByHeight comparing the raw Height (720p matches
// nothing, no raw height is below 720 to descend to, and the 1080x1920 source
// comes back); selectNextLowerVariant descending by the raw Height (900p
// descends to 854, which is no rendition's short edge, and the source comes
// back).
func TestHeightPreferenceMeasuresTheShortEdge(t *testing.T) {
	for _, tc := range []struct{ pref, want string }{
		{"720p", "720p60"},
		{"1080p60", "chunked"},
		{"900p", "720p60"},
	} {
		got := SelectBestVariant(portraitLadder(), tc.pref, 0, true)
		if got == nil || got.Name != tc.want {
			t.Errorf("portrait ladder, preference %s: selected %v, want %s", tc.pref, got, tc.want)
		}
	}
}

// TestHeightPreferenceRanksLikeTheChosenSize (D-Y2): at the size a
// preference names, the renditions are ranked exactly as rankAtChosenSize
// ranks the size the cap chose — codec, then the frame rate prefer_60fps asks
// for, then the source flag, then bandwidth. The height match took the
// highest bandwidth, so a suffix-less "720p" ignored prefer_60fps.
//
// Mutants: selectVariantByHeight ranking by bandwidth alone, as it did (the
// 60 fps transcode wins with prefer_60fps off, the 30 fps one with it on, the
// H.264 source over the AV1 rendition); selectVariantByHeight passing true
// for prefer60fps (the off case takes 60 fps).
func TestHeightPreferenceRanksLikeTheChosenSize(t *testing.T) {
	sixtyOutbid := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 8000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 2500000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"},
		{Name: "720p30", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 30, VideoCodec: "avc1"},
	}
	thirtyOutbid := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 8000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"},
		{Name: "720p30", Bandwidth: 2500000, Width: 1280, Height: 720, FPS: 30, VideoCodec: "avc1"},
	}
	if got := SelectBestVariant(thirtyOutbid, "720p", 0, false); got == nil || got.Name != "720p30" {
		t.Errorf("720p with prefer_60fps off selected %v, want 720p30 — the frame rate outranks bandwidth", got)
	}
	if got := SelectBestVariant(sixtyOutbid, "720p", 0, true); got == nil || got.Name != "720p60" {
		t.Errorf("720p with prefer_60fps on selected %v, want 720p60 — the frame rate outranks bandwidth", got)
	}

	codecs := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 9000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p60-av1", Bandwidth: 6000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "av01"},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"},
	}
	if got := SelectBestVariant(codecs, "1080p", 2160, true); got == nil || got.Name != "1080p60-av1" {
		t.Errorf("1080p selected %v, want 1080p60-av1 — the codec outranks the source flag and bandwidth", got)
	}
}

// TestHeightPreferenceFPSSuffix: an fps suffix asks for that rate at the size
// named, whatever prefer_60fps says, and falls back to the size's own ranking
// when the size offers no such rate. The descent keeps it: "1080p60" on a
// ladder whose largest size is 720 still asks for 60 fps there.
//
// The fallback ranks the frame rate by the suffix too, not by prefer_60fps: a
// 50 fps broadcast has no 59 fps rendition for "1080p60" to keep, and with
// prefer_60fps off the fallback took the 30 fps transcode beside its source,
// at the size named and at the size the descent came down to. A YouTube VOD
// takes the 50 fps stream for the same preference (vodSelectionBounds), and
// the YouTube live selectors take it by their bandwidth fallback.
//
// Mutants: the suffix filter dropped from selectVariantByHeight (prefer_60fps
// off takes 720p30 for a 720p60 preference, and for the descent's); the
// fallback to the whole size dropped (neither 720 nor 480 has a 60 fps
// rendition, so nothing matches and the source comes back);
// selectNextLowerVariant passing no fps (the descent takes 720p30); the
// suffix's rate not set over prefer60fps in selectVariantByHeight (the 50 fps
// ladder takes 1080p30, and 720p30 under a 720 cap).
func TestHeightPreferenceFPSSuffix(t *testing.T) {
	ladder := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 8000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"},
		{Name: "720p30", Bandwidth: 2500000, Width: 1280, Height: 720, FPS: 30, VideoCodec: "avc1"},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480, FPS: 30, VideoCodec: "avc1"},
	}
	if got := SelectBestVariant(ladder, "720p60", 0, false); got == nil || got.Name != "720p60" {
		t.Errorf("720p60 with prefer_60fps off selected %v, want 720p60 — the suffix is asked for outright", got)
	}

	thirtyOnly := []TwitchHLSVariant{ladder[0], ladder[2], ladder[3]}
	if got := SelectBestVariant(thirtyOnly, "720p60", 0, true); got == nil || got.Name != "720p30" {
		t.Errorf("720p60 with no 720p 60 fps rendition selected %v, want 720p30 — the size before the rate", got)
	}

	noTenEighty := []TwitchHLSVariant{ladder[1], ladder[2], ladder[3]}
	if got := SelectBestVariant(noTenEighty, "1080p60", 0, false); got == nil || got.Name != "720p60" {
		t.Errorf("1080p60 descending to 720 selected %v, want 720p60 — the suffix still asks for 60 fps", got)
	}

	fifty := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, FPS: 50, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p30", Bandwidth: 4500000, Width: 1920, Height: 1080, FPS: 30, VideoCodec: "avc1"},
		{Name: "720p50", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 50, VideoCodec: "avc1"},
		{Name: "720p30", Bandwidth: 2000000, Width: 1280, Height: 720, FPS: 30, VideoCodec: "avc1"},
	}
	if got := SelectBestVariant(fifty, "1080p60", 0, false); got == nil || got.Name != "chunked" {
		t.Errorf("1080p60 on a 50 fps ladder with prefer_60fps off selected %v, want the 50 fps source — the suffix ranks the fallback", got)
	}
	if got := SelectBestVariant(fifty, "1080p60", 720, false); got == nil || got.Name != "720p50" {
		t.Errorf("1080p60 under a 720 cap on a 50 fps ladder with prefer_60fps off selected %v, want 720p50 — the descent ranks by the suffix too", got)
	}
}
