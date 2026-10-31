package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// preRollNow is the instant the parse tests measure from: 400 ms into a
// second, so upstream's math.ceil(now) is visible as the next whole second.
var preRollNow = time.Unix(1_700_000_000, 400_000_000)

// preRollBase is ceil(preRollNow), the second every expected instant counts
// its wait from.
var preRollBase = time.Unix(1_700_000_001, 0)

// startPlacement is an adPlacements entry of the given kind whose renderer is
// an instreamVideoAdRenderer with the given fields.
func startPlacement(kind string, renderer map[string]any) map[string]any {
	return map[string]any{"adPlacementRenderer": map[string]any{
		"config":   map[string]any{"adPlacementConfig": map[string]any{"kind": kind}},
		"renderer": map[string]any{"instreamVideoAdRenderer": renderer},
	}}
}

// adSlot is an adSlots entry of the given trigger whose renderingContent is
// content.
func adSlot(trigger string, content map[string]any) map[string]any {
	return map[string]any{"adSlotRenderer": map[string]any{
		"adSlotMetadata": map[string]any{"triggerEvent": trigger},
		"fulfillmentContent": map[string]any{"fulfilledLayout": map[string]any{
			"playerBytesAdLayoutRenderer": map[string]any{"renderingContent": content},
		}},
	}}
}

// sequentialSlot is a BEFORE_CONTENT adSlots entry whose renderingContent is
// a sequence of layouts, one per renderer.
func sequentialSlot(renderers ...map[string]any) map[string]any {
	layouts := make([]any, 0, len(renderers))
	for _, r := range renderers {
		layouts = append(layouts, map[string]any{"playerBytesAdLayoutRenderer": map[string]any{
			"renderingContent": map[string]any{"instreamVideoAdRenderer": r},
		}})
	}
	return adSlot("SLOT_TRIGGER_EVENT_BEFORE_CONTENT", map[string]any{
		"playerBytesSequentialLayoutRenderer": map[string]any{"sequentialLayouts": layouts},
	})
}

// adRenderer is an instreamVideoAdRenderer: length_seconds in its playerVars
// (lengthSecs < 0: none), skipOffsetMilliseconds when skipMs >= 0.
func adRenderer(lengthSecs int, skipMs float64) map[string]any {
	r := map[string]any{}
	if lengthSecs >= 0 {
		r["playerVars"] = fmt.Sprintf("video_id=adVideo123&length_seconds=%d&ad_type=video", lengthSecs)
	}
	if skipMs >= 0 {
		r["skipOffsetMilliseconds"] = skipMs
	}
	return r
}

// playerResponseWithAds is a playable VOD response with a split pair of
// formats and both manifests, carrying the given adPlacements and adSlots —
// round-tripped through JSON so the parse sees exactly the shapes a real
// response decodes to.
func playerResponseWithAds(t *testing.T, placements, slots []any) map[string]any {
	t.Helper()
	pr := map[string]any{
		"playabilityStatus": map[string]any{"status": "OK"},
		"videoDetails":      map[string]any{"videoId": "test1234567", "title": "t", "author": "a"},
		"streamingData": map[string]any{
			"adaptiveFormats": []any{
				map[string]any{"itag": 299, "url": "https://example.com/v", "mimeType": `video/mp4; codecs="avc1.64002a"`, "width": 1920, "height": 1080, "contentLength": "1000"},
				map[string]any{"itag": 140, "url": "https://example.com/a", "mimeType": `audio/mp4; codecs="mp4a.40.2"`, "contentLength": "100"},
			},
			"dashManifestUrl": "https://m.example/v.mpd",
			"hlsManifestUrl":  "https://m.example/v.m3u8",
		},
	}
	if placements != nil {
		pr["adPlacements"] = placements
	}
	if slots != nil {
		pr["adSlots"] = slots
	}
	blob, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// adLogRecorder keeps every Debug line a PlayerAPI logs, message and args.
type adLogRecorder struct {
	noopLogger
	mu   sync.Mutex
	logs [][]any
}

func (d *adLogRecorder) Debug(msg string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logs = append(d.logs, append([]any{msg}, args...))
}

// adLines returns the "ad" field of every pre-roll Debug line, and the client
// each named.
func (d *adLogRecorder) adLines() (ads, clients []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.logs {
		if l[0] != "[PlayerApi] pre-roll ad detected" {
			continue
		}
		for i := 1; i+1 < len(l); i += 2 {
			switch l[i] {
			case "ad":
				ads = append(ads, l[i+1].(string))
			case "client":
				clients = append(clients, l[i+1].(string))
			}
		}
	}
	return ads, clients
}

// TestPlaybackAvailableAtReadsThePreRollAds pins the port of yt-dlp's
// _get_available_at_timestamp (715af0c63 / #14646, the adPlacements half
// 3c279b33c / #17062): a response's media may first be requested at
// ceil(now) plus the waits of its pre-roll ads — each the time until a
// skippable ad may be skipped, else its length — and at once when it carries
// none. A mid-roll is not waited for, and two pre-rolls add up. Each ad that
// counts is logged at Debug in upstream's words, naming the client.
//
// Mutants (each run, each fails here): the skip time not overriding the
// length (the skippable cases wait 15 s); the first length_seconds taken
// instead of the last ("the last length_seconds wins"); the kind or trigger
// filter dropped (the mid-roll case waits); the sequential layouts not
// walked (that case waits 0); the sum replaced by the last ad's wait (the
// two-pre-roll case waits 5 s); now used as the base instead of its ceiling
// (every waiting case lands 600 ms early); a response with no ad stamped
// with now instead of the zero time (the no-ad case).
func TestPlaybackAvailableAtReadsThePreRollAds(t *testing.T) {
	cases := []struct {
		name       string
		placements []any
		slots      []any
		wait       time.Duration // 0: the zero time
		ads        []string
	}{
		{name: "no ads"},
		{
			name:       "a skippable START placement waits until it may be skipped",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(15, 5000))},
			wait:       5 * time.Second,
			ads:        []string{"a 15s ad skippable after 5s"},
		},
		{
			name:       "an unskippable START placement waits its length",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(15, -1))},
			wait:       15 * time.Second,
			ads:        []string{"a 15s ad"},
		},
		{
			name:  "a BEFORE_CONTENT slot",
			slots: []any{adSlot("SLOT_TRIGGER_EVENT_BEFORE_CONTENT", map[string]any{"instreamVideoAdRenderer": adRenderer(6, -1)})},
			wait:  6 * time.Second,
			ads:   []string{"a 6s ad"},
		},
		{
			name:  "a BEFORE_CONTENT slot of sequential layouts adds every layout",
			slots: []any{sequentialSlot(adRenderer(6, -1), adRenderer(20, 5000))},
			wait:  11 * time.Second,
			ads:   []string{"a 6s ad", "a 20s ad skippable after 5s"},
		},
		{
			name:       "a mid-roll placement and a later slot are not waited for",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_MILLISECONDS", adRenderer(30, -1))},
			slots:      []any{adSlot("SLOT_TRIGGER_EVENT_LAYOUT_ID_ENTERED", map[string]any{"instreamVideoAdRenderer": adRenderer(30, -1)})},
		},
		{
			name:       "two pre-rolls add up",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(15, -1))},
			slots:      []any{adSlot("SLOT_TRIGGER_EVENT_BEFORE_CONTENT", map[string]any{"instreamVideoAdRenderer": adRenderer(30, 5000)})},
			wait:       20 * time.Second,
			ads:        []string{"a 15s ad", "a 30s ad skippable after 5s"},
		},
		{
			name:       "a fractional skip time",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(-1, 5500))},
			wait:       5500 * time.Millisecond,
			ads:        []string{"an ad skippable after 5.5s"},
		},
		{
			name:       "an ad stating neither its length nor a skip time adds nothing",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(-1, -1))},
		},
		{
			name: "the last length_seconds wins",
			placements: []any{startPlacement("AD_PLACEMENT_KIND_START", map[string]any{
				"playerVars": "length_seconds=10&video_id=adVideo123&length_seconds=12&length_seconds=",
			})},
			wait: 12 * time.Second,
			ads:  []string{"a 12s ad"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lg := &adLogRecorder{}
			p := &PlayerAPI{logger: lg, playbackClock: func() time.Time { return preRollNow }}
			ctx := withPlayerClient(context.Background(), "TVHTML5")
			got := p.playbackAvailableAt(ctx, playerResponseWithAds(t, tc.placements, tc.slots), "test1234567")
			var want time.Time
			if tc.wait > 0 {
				want = preRollBase.Add(tc.wait)
			}
			if !got.Equal(want) {
				t.Errorf("available at %v, want %v", got, want)
			}
			ads, clients := lg.adLines()
			if fmt.Sprint(ads) != fmt.Sprint(tc.ads) {
				t.Errorf("pre-roll Debug lines = %q, want %q", ads, tc.ads)
			}
			for _, c := range clients {
				if c != "TVHTML5" {
					t.Errorf("pre-roll Debug line names client %q, want TVHTML5", c)
				}
			}
		})
	}

	t.Run("now on a whole second is its own ceiling", func(t *testing.T) {
		p := &PlayerAPI{logger: noopLogger{}, playbackClock: func() time.Time { return preRollBase }}
		pr := playerResponseWithAds(t, []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(15, -1))}, nil)
		if got, want := p.playbackAvailableAt(context.Background(), pr, "test1234567"), preRollBase.Add(15*time.Second); !got.Equal(want) {
			t.Errorf("available at %v, want %v", got, want)
		}
	})
}

// TestParsedResponseStampsEveryFormatAndManifest: parsePlayerResponse stamps
// the response's instant on every format it yields and on both manifests,
// which is what lets every download path read it off whatever it fetches;
// a response with no ad stamps nothing.
//
// Mutants: the format loop dropped (the formats read zero); either manifest
// left unstamped (that manifest reads zero); a manifest stamped while its URL
// is absent (the no-manifest response's assertion).
func TestParsedResponseStampsEveryFormatAndManifest(t *testing.T) {
	p := &PlayerAPI{logger: noopLogger{}, playbackClock: func() time.Time { return preRollNow }}
	ctx := withPlayerClient(context.Background(), "TVHTML5")

	pr := playerResponseWithAds(t, []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(15, -1))}, nil)
	info, err := p.parsePlayerResponse(ctx, pr, "", nil, "test1234567")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	want := preRollBase.Add(15 * time.Second)
	if len(info.Formats) != 2 {
		t.Fatalf("formats = %d, want 2", len(info.Formats))
	}
	for _, f := range info.Formats {
		if !f.AvailableAt.Equal(want) {
			t.Errorf("itag %d available at %v, want %v", f.Itag, f.AvailableAt, want)
		}
	}
	if !info.DashManifestAvailableAt.Equal(want) || !info.HlsManifestAvailableAt.Equal(want) {
		t.Errorf("manifests available at (%v, %v), want %v for both", info.DashManifestAvailableAt, info.HlsManifestAvailableAt, want)
	}

	plain := playerResponseWithAds(t, nil, nil)
	info, err = p.parsePlayerResponse(ctx, plain, "", nil, "test1234567")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	for _, f := range info.Formats {
		if !f.AvailableAt.IsZero() {
			t.Errorf("no-ad response: itag %d available at %v, want the zero time", f.Itag, f.AvailableAt)
		}
	}
	if !info.DashManifestAvailableAt.IsZero() || !info.HlsManifestAvailableAt.IsZero() {
		t.Errorf("no-ad response: manifests available at (%v, %v), want zero", info.DashManifestAvailableAt, info.HlsManifestAvailableAt)
	}

	sd := pr["streamingData"].(map[string]any)
	delete(sd, "dashManifestUrl")
	delete(sd, "hlsManifestUrl")
	info, err = p.parsePlayerResponse(ctx, pr, "", nil, "test1234567")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	if !info.DashManifestAvailableAt.IsZero() || !info.HlsManifestAvailableAt.IsZero() {
		t.Errorf("no manifests: manifests available at (%v, %v), want zero — the instant moves with a URL", info.DashManifestAvailableAt, info.HlsManifestAvailableAt)
	}
}

// TestDedupKeepsEachCopysOwnAvailability: a stream two clients returned keeps
// the winner's own instant, and the token-free shadow it carries keeps the
// instant of ITS response — the VOD missing_pot swap downloads the shadow,
// and must wait for that response's ad, not the winner's.
//
// Mutant: the stamp dropped from parsePlayerResponse (both read zero — the
// fixtures here are parsed, not hand-stamped).
func TestDedupKeepsEachCopysOwnAvailability(t *testing.T) {
	parse := func(clock time.Time, secs int) []Format {
		t.Helper()
		p := &PlayerAPI{logger: noopLogger{}, playbackClock: func() time.Time { return clock }}
		info, err := p.parsePlayerResponse(context.Background(),
			playerResponseWithAds(t, []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(secs, -1))}, nil),
			"", nil, "test1234567")
		if err != nil {
			t.Fatalf("parsePlayerResponse: %v", err)
		}
		return info.Formats
	}
	var pool []Format
	collectFormats(&pool, parse(preRollNow, 15), "web_creator", AuthLevelWebCreator)
	collectFormats(&pool, parse(preRollNow.Add(time.Minute), 6), "visionos", AuthLevelVisionOS)

	out := deduplicateFormats(context.Background(), pool)
	if len(out) != 2 {
		t.Fatalf("dedup kept %d streams, want 2", len(out))
	}
	for _, f := range out {
		if f.Source != "web_creator" {
			t.Fatalf("itag %d won by %s, want web_creator", f.Itag, f.Source)
		}
		if want := preRollBase.Add(15 * time.Second); !f.AvailableAt.Equal(want) {
			t.Errorf("itag %d winner available at %v, want %v", f.Itag, f.AvailableAt, want)
		}
		if f.TokenFreeAlternate == nil {
			t.Fatalf("itag %d carries no shadow", f.Itag)
		}
		if want := preRollBase.Add(time.Minute + 6*time.Second); !f.TokenFreeAlternate.AvailableAt.Equal(want) {
			t.Errorf("itag %d shadow available at %v, want %v — its own response's", f.Itag, f.TokenFreeAlternate.AvailableAt, want)
		}
	}
}

// TestPlaybackWaitAppliesAsUpstreamStamps: upstream stamps available_at on
// every format `if live_status not in ('is_live', 'post_live')`.
//
// Mutant: either exclusion dropped, or a finished stream excluded.
func TestPlaybackWaitAppliesAsUpstreamStamps(t *testing.T) {
	for status, want := range map[StreamStatus]bool{
		StreamLive:       false,
		StreamPostLive:   false,
		StreamVOD:        true,
		StreamNotAStream: true,
		StreamUpcoming:   true,
	} {
		if got := PlaybackWaitApplies(status); got != want {
			t.Errorf("PlaybackWaitApplies(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestPlaybackClockReadsTheWallClockByDefault pins what the tests above
// replace: a PlayerAPI from NewPlayerAPI has no playback clock of its own and
// measures a response's wait from the wall clock.
//
// Mutant: playbackNow answering anything but time.Now() when the seam is nil.
func TestPlaybackClockReadsTheWallClockByDefault(t *testing.T) {
	p := newRetryTestAPI()
	if p.playbackClock != nil {
		t.Fatal("NewPlayerAPI set a playback clock; production measures from the wall clock")
	}
	before := time.Now()
	got := p.playbackNow()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("playbackNow() = %v, want the wall clock (between %v and %v)", got, before, after)
	}
}
