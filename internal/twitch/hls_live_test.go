package twitch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveTwitchMasterPlaylistParses is the arc's live gate for the
// enhanced-broadcast opt-in.
//
// WHY A LIVE GATE. A fixture cannot see this path die. If Usher ever refuses
// platform= or supported_codecs=, or renames a codec, the answer is not an
// error — it is a perfectly well-formed master playlist that simply no longer
// lists the enhanced renditions, and every fixture in hls_test.go keeps
// passing. The failure mode is silent quality loss on exactly the channels the
// feature was added for.
//
// WHAT IT ASSERTS — capabilities, never mechanisms:
//   - the two opt-in parameters reach the built URL;
//   - Usher answers a parseable master playlist with at least one usable variant;
//   - at least one variant carries a resolution (so the selector has something to rank);
//   - every variant that carries CODECS yields a recognised video family, or is
//     the audio-only rendition;
//   - SelectBestVariant picks something.
//
// It does NOT assert that an enhanced rendition exists: most channels do not
// multi-encode, and a gate that demanded one would fail for a reason that says
// nothing about Moombox.
//
// WHAT IT NEVER PRINTS. Not the URL, not the token, not the signature. The
// usher URL is a signed entitlement; the whole question here is answered by
// counts, families and booleans. The codec families ARE logged, because they
// are the measurement the operator running this gate wants.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_TEST=1
//	MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is LIVE right now>
//
// The channel is a required input rather than a hardcoded default, matching
// TestLivePlaybackTokenShape in playback_token_live_test.go: a default would
// rot, and an offline channel yields no stream playback token at all. Always
// run with -count=1 — a cached PASS on a live probe is not a measurement.
func TestLiveTwitchMasterPlaylistParses(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_TWITCH_TEST") != "1" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_TEST=1 to run the live Twitch master-playlist gate")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_CHANNEL=<login of a channel that is live right now> " +
			"to run the live Twitch master-playlist gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	api := NewAPI(&testLogger{})

	info, err := api.GetStreamInfo(ctx, channel, "")
	if err != nil {
		t.Fatalf("GetStreamInfo(%s) failed: %v", channel, err)
	}
	if info == nil || !info.IsLive {
		t.Skipf("%s is not live right now; nothing to measure", channel)
	}

	token, err := api.GetStreamAccessToken(ctx, channel, "")
	if err != nil {
		t.Fatalf("GetStreamAccessToken(%s) failed: %v", channel, err)
	}

	usherURL := BuildUsherLiveURL(channel, token)
	// Assert on the URL WITHOUT printing it: it carries the signed token.
	if !strings.Contains(usherURL, "platform=web") {
		t.Error("the built usher URL carries no platform=web — the enhanced-broadcast opt-in " +
			"never reaches Usher (URL not printed: it carries a signed entitlement)")
	}
	if !strings.Contains(usherURL, "supported_codecs=av1") {
		t.Error("the built usher URL carries no supported_codecs — the enhanced-broadcast " +
			"opt-in never reaches Usher")
	}

	variants, err := FetchHLSMasterPlaylist(ctx, usherURL)
	if err != nil {
		t.Fatalf("FetchHLSMasterPlaylist failed: %v", err)
	}
	if len(variants) == 0 {
		t.Fatal("Usher answered a master playlist with no variants — the capability this gate " +
			"exists for (a usable playlist) is gone")
	}

	sawResolution := false
	sawCodecs := false
	families := map[string]int{}
	for _, v := range variants {
		if v.URL == "" {
			t.Errorf("variant %q carries no URL", v.Name)
		}
		if v.Height > 0 && v.Width > 0 {
			sawResolution = true
		}
		if v.Codecs == "" {
			continue
		}
		sawCodecs = true
		families[v.VideoCodec]++
		audioOnly := strings.Contains(strings.ToLower(v.Name), "audio_only")
		switch v.VideoCodec {
		case "av01", "hevc", "avc1":
		case "":
			if !audioOnly {
				t.Errorf("variant %q carries CODECS but no recognised video family — "+
					"videoCodecFamily no longer understands what Twitch sends", v.Name)
			}
		default:
			t.Errorf("variant %q reported video family %q, which is not one this parser emits",
				v.Name, v.VideoCodec)
		}
	}
	if !sawResolution {
		t.Error("no variant carried a RESOLUTION — the selector has nothing to rank")
	}
	if !sawCodecs {
		t.Log("NOTE: no variant carried a CODECS attribute. That is not a failure (Usher may " +
			"omit it), but the codec-aware selection is dormant for this channel.")
	}
	t.Logf("variants=%d codec families=%v enhanced=%v",
		len(variants), families, families["av01"]+families["hevc"] > 0)

	if best := SelectBestVariant(variants, "best", 0, true); best == nil {
		t.Error("SelectBestVariant returned nil for a real live playlist")
	} else {
		t.Logf("selected: %s %dx%d fps=%.0f codec=%q source=%v",
			best.Name, best.Width, best.Height, best.FPS, best.VideoCodec, best.IsSource)
	}
}
