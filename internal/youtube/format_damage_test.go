package youtube

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// damageResponse is a VOD player response for damageVideoID whose
// videoDetails states lengthSeconds (omitted when empty), whose microformat
// states microLength (likewise), and whose adaptiveFormats are the given
// JSON objects.
func damageResponse(t *testing.T, lengthSeconds, microLength string, formats ...string) map[string]any {
	t.Helper()
	vd := fmt.Sprintf(`"videoId": %q, "title": "t", "author": "a"`, damageVideoID)
	if lengthSeconds != "" {
		vd += fmt.Sprintf(`, "lengthSeconds": %q`, lengthSeconds)
	}
	mf := `{}`
	if microLength != "" {
		mf = fmt.Sprintf(`{"lengthSeconds": %q}`, microLength)
	}
	return decodePlayerJSON(t, fmt.Sprintf(`{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {%s},
		"microformat": {"playerMicroformatRenderer": %s},
		"streamingData": {"adaptiveFormats": [%s]}
	}`, vd, mf, strings.Join(formats, ",")))
}

const damageVideoID = "damageVid01"

// damageFormat is one adaptive format; approx is its approxDurationMs JSON
// value, or "" for none.
func damageFormat(itag int, mime, approx string) string {
	s := fmt.Sprintf(`{"itag": %d, "url": "https://x/%d", "mimeType": %q, "bitrate": %d`, itag, itag, mime, 1000+itag)
	if strings.Contains(mime, "video") {
		s += `, "width": 1920, "height": 1080, "fps": 30`
	}
	if approx != "" {
		s += `, "approxDurationMs": ` + approx
	}
	return s + "}"
}

const (
	damageAV1  = `video/mp4; codecs="av01.0.08M.08"`
	damageVP9  = `video/webm; codecs="vp9"`
	damageOpus = `audio/webm; codecs="opus"`
	damageAAC  = `audio/mp4; codecs="mp4a.40.2"`
)

// TestParsePlayerResponseMarksDamagedRenditions pins yt-dlp's is_damaged
// (_video.py:3430-3438): a format whose approxDurationMs is under the video's
// length halved in whole seconds — `format_duration < duration // 2` — is
// Damaged, read off the parsed response's own length (videoDetails, else its
// microformat, upstream's `duration` chain at _video.py:4036-4038).
//
// Mutants this kills:
//   - approxDurationMs never parsed, or the stamp dropped → the damaged rows
//     come back intact
//   - `<=` for `<`                                       → "exactly half"
//   - the half taken in milliseconds (lengthSeconds*1000/2) rather than in
//     whole seconds                                      → "an odd length's
//     half is whole seconds"
//   - a stated 0 read as unstated                        → "a stated 0"
//   - an unparsable value read as 0                      → "unparsable"
//   - the microformat fallback dropped                   → "microformat"
func TestParsePlayerResponseMarksDamagedRenditions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		length, micro string
		approx        string
		want          bool
	}{
		{"well under half is damaged", "600", "", `"120000"`, true},
		{"one millisecond under half is damaged", "600", "", `"299999"`, true},
		{"exactly half is intact", "600", "", `"300000"`, false},
		{"an odd length's half is whole seconds", "601", "", `"300400"`, false},
		{"the whole length is intact", "600", "", `"600046"`, false},
		{"a stated 0 is damaged, as upstream's 0.0 is", "600", "", `"0"`, true},
		{"unstated is intact", "600", "", "", false},
		{"unparsable is intact", "600", "", `"soon"`, false},
		{"microformat length when videoDetails states none", "0", "600", `"120000"`, true},
		{"microformat length when videoDetails is absent", "", "600", `"120000"`, true},
		{"no length anywhere: nothing is damaged", "", "", `"1000"`, false},
		{"a one-second video's half is zero", "1", "", `"0"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlayerAPI(nil, noopLogger{})
			data := damageResponse(t, tc.length, tc.micro, damageFormat(399, damageAV1, tc.approx))
			info, err := p.parsePlayerResponse(context.Background(), data, "", nil, damageVideoID)
			if err != nil {
				t.Fatalf("parsePlayerResponse: %v", err)
			}
			if len(info.Formats) != 1 {
				t.Fatalf("parsed %d formats, want 1", len(info.Formats))
			}
			if got := info.Formats[0].Damaged; got != tc.want {
				t.Errorf("Damaged = %v, want %v (lengthSeconds %q, microformat %q, approxDurationMs %s)",
					got, tc.want, tc.length, tc.micro, tc.approx)
			}
		})
	}
}

// TestDamagedRenditionsRankBelowEveryIntactOne pins the selector's first rung:
// yt-dlp's `'preference': -10 if is_damaged` (_video.py:3474), which ranks
// through ie_pref, a priority field, ahead of every sort a user asks for
// (utils/_utils.py:5392, 5532-5536) — so ahead of the cap too. A damaged
// rendition is taken only when no intact one of its kind exists.
//
// Mutants this kills:
//   - the selection loop's video skip dropped     → "a short top AV1"
//   - the cap pre-pass's skip dropped             → "the only rendition at
//     the cap", which then resolves the cap to 1080 and finds no video
//     there, and "ahead of the cap", which resolves it to 720 likewise
//   - the audio skip dropped                      → "a short Opus"
//   - the intact test dropped (always pass over), or a damaged rendition
//     counted as intact                           → both "only" rows: no
//     video, no audio
//   - one intact flag for both kinds              → both "only" rows, whose
//     other kind is intact
func TestDamagedRenditionsRankBelowEveryIntactOne(t *testing.T) {
	video := func(itag int, mime string, w, h int, damaged bool) Format {
		return Format{Itag: itag, MimeType: mime, Bitrate: 1000 + itag, Width: new(w), Height: new(h), Fps: new(30),
			URL: fmt.Sprintf("https://x/%d", itag), Damaged: damaged}
	}
	audio := func(itag int, mime string, damaged bool) Format {
		return Format{Itag: itag, MimeType: mime, Bitrate: 1000 + itag, AudioQuality: "AUDIO_QUALITY_MEDIUM",
			URL: fmt.Sprintf("https://x/%d", itag), Damaged: damaged}
	}
	for _, tc := range []struct {
		name                 string
		formats              []Format
		maxRes               int
		wantVideo, wantAudio int
	}{
		{
			name: "a short top AV1 loses to the intact VP9 beside it",
			formats: []Format{
				video(399, damageAV1, 1920, 1080, true), video(248, damageVP9, 1920, 1080, false),
				audio(251, damageOpus, false),
			},
			maxRes: 1080, wantVideo: 248, wantAudio: 251,
		},
		{
			name: "the only rendition at the cap is short: the cap resolves over the intact ones",
			formats: []Format{
				video(399, damageAV1, 1920, 1080, true), video(136, `video/mp4; codecs="avc1.4d401f"`, 1280, 720, false),
				audio(251, damageOpus, false),
			},
			maxRes: 1080, wantVideo: 136, wantAudio: 251,
		},
		{
			name: "damage ranks ahead of the cap: an intact rendition above it wins",
			formats: []Format{
				video(398, damageAV1, 1280, 720, true), video(399, damageAV1, 1920, 1080, false),
				audio(251, damageOpus, false),
			},
			maxRes: 720, wantVideo: 399, wantAudio: 251,
		},
		{
			name: "only damaged video: it is still taken, ranked as before",
			formats: []Format{
				video(248, damageVP9, 1920, 1080, true), video(399, damageAV1, 1920, 1080, true),
				audio(251, damageOpus, false),
			},
			maxRes: 1080, wantVideo: 399, wantAudio: 251,
		},
		{
			name: "a short Opus loses to the intact AAC",
			formats: []Format{
				video(248, damageVP9, 1920, 1080, false),
				audio(251, damageOpus, true), audio(140, damageAAC, false),
			},
			maxRes: 1080, wantVideo: 248, wantAudio: 140,
		},
		{
			name: "only damaged audio: it is still taken, ranked as before",
			formats: []Format{
				video(248, damageVP9, 1920, 1080, false),
				audio(140, damageAAC, true), audio(251, damageOpus, true),
			},
			maxRes: 1080, wantVideo: 248, wantAudio: 251,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectBestFormats(tc.formats, tc.maxRes, true)
			if got.Video == nil || got.Video.Itag != tc.wantVideo {
				t.Errorf("video = %v, want itag %d", itagOf(got.Video), tc.wantVideo)
			}
			if got.Audio == nil || got.Audio.Itag != tc.wantAudio {
				t.Errorf("audio = %v, want itag %d", itagOf(got.Audio), tc.wantAudio)
			}
		})
	}
}

// TestSelectorNamesThePassedOverRenditions pins the selector's Debug line:
// the damaged renditions it passed over, and only those — an intact one, or a
// damaged one taken because nothing intact of its kind exists, is not passed
// over.
func TestSelectorNamesThePassedOverRenditions(t *testing.T) {
	formats := []Format{
		{Itag: 399, MimeType: damageAV1, Width: new(1920), Height: new(1080), URL: "https://x/399", Damaged: true},
		{Itag: 248, MimeType: damageVP9, Width: new(1920), Height: new(1080), URL: "https://x/248"},
		{Itag: 251, MimeType: damageOpus, URL: "https://x/251", Damaged: true},
	}
	lg := &damageLogger{}
	SelectBestFormatsWithLogger(formats, 1080, true, lg)
	got := lg.damaged(lg.debugs)
	if len(got) != 1 || !strings.Contains(got[0], "itag 399") || strings.Contains(got[0], "itag 248") || strings.Contains(got[0], "itag 251") {
		t.Errorf("passed-over lines = %q, want one naming itag 399 alone", got)
	}
}

func itagOf(f *Format) any {
	if f == nil {
		return "none"
	}
	return f.Itag
}

// TestAShortRenditionIsNotArchived is the case end to end, from the wire: the
// response's top rendition — an AV1, which outranks VP9 — states a third of
// the video's length (yt-dlp's 2-nOtRESiUc, #2823), and the parsed pool's
// selection takes the intact VP9 and the intact Opus. Every link of the chain
// is load-bearing: the approxDurationMs parse, the stamp against
// lengthSeconds, and the selector's rung.
func TestAShortRenditionIsNotArchived(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := damageResponse(t, "600", "",
		damageFormat(399, damageAV1, `"200000"`),
		damageFormat(248, damageVP9, `"600046"`),
		damageFormat(251, damageOpus, `"600061"`),
	)
	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, damageVideoID)
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	got := SelectBestFormats(info.Formats, 1080, true)
	if got.Video == nil || got.Video.Itag != 248 {
		t.Errorf("video = %v, want the intact VP9 248 — the short AV1 would stop at 200 s of 600", itagOf(got.Video))
	}
	if got.Audio == nil || got.Audio.Itag != 251 {
		t.Errorf("audio = %v, want 251", itagOf(got.Audio))
	}
}

// damageLogger records the Warn and Debug lines a parse logs.
type damageLogger struct{ warns, debugs []string }

func (l *damageLogger) Debug(msg string, args ...any) {
	l.debugs = append(l.debugs, fmt.Sprintf("%s %v", msg, args))
}
func (l *damageLogger) Info(string, ...any) {}
func (l *damageLogger) Warn(msg string, args ...any) {
	l.warns = append(l.warns, fmt.Sprintf("%s %v", msg, args))
}
func (l *damageLogger) Error(string, ...any) {}

func (l *damageLogger) damaged(lines []string) []string {
	var out []string
	for _, s := range lines {
		if strings.Contains(s, "damaged") {
			out = append(out, s)
		}
	}
	return out
}

// TestDamagedRenditionsAreReportedOncePerExtraction pins the report's level,
// cardinality and content. Upstream warns with `report_warning(f'Some
// {client_name} client {proto} formats are possibly damaged. They will be
// deprioritized', only_once=True)` (_video.py:3436-3438): a Warn, naming the
// client, once per run — here once per extraction, as the DRM report is. A
// probe records it at Debug, and a response with nothing damaged says nothing.
//
// Mutants this kills:
//   - the line demoted to Debug, or never logged     → no Warn
//   - the once-per-extraction flag dropped           → two Warns from one
//     extraction of two responses
//   - the flag made per-process (a package var, or on PlayerAPI) → the second
//     extraction stays silent
//   - the probe arm dropped                          → the probe Warns
//   - the client or the itags left off the line      → the content checks
//   - reported when nothing is damaged               → the intact response
//     Warns
func TestDamagedRenditionsAreReportedOncePerExtraction(t *testing.T) {
	short := damageResponse(t, "600", "",
		damageFormat(399, damageAV1, `"200000"`), damageFormat(248, damageVP9, `"600046"`))
	intact := damageResponse(t, "600", "", damageFormat(248, damageVP9, `"600046"`))

	lg := &damageLogger{}
	p := NewPlayerAPI(nil, lg)
	parse := func(ctx context.Context, client string, data map[string]any) {
		t.Helper()
		if _, err := p.parsePlayerResponse(withPlayerClient(ctx, client), data, "", nil, damageVideoID); err != nil {
			t.Fatalf("parsePlayerResponse (%s): %v", client, err)
		}
	}

	parse(withExtractionState(context.Background()), "web", intact)
	if got := lg.damaged(lg.warns); len(got) != 0 {
		t.Fatalf("a response with nothing damaged warned: %q", got)
	}

	first := withExtractionState(context.Background())
	parse(first, "tv", short)
	parse(first, "web", short)
	got := lg.damaged(lg.warns)
	if len(got) != 1 {
		t.Fatalf("one extraction of two damaged responses logged %d Warns, want 1: %q", len(got), got)
	}
	for _, want := range []string{"client tv", "itags [399]"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the Warn %q does not name %q (the first client to carry them, the damaged itag)", got[0], want)
		}
	}
	if strings.Contains(got[0], "248") {
		t.Errorf("the Warn %q names the intact itag 248", got[0])
	}

	parse(withExtractionState(context.Background()), "tv", short)
	if got := lg.damaged(lg.warns); len(got) != 2 {
		t.Fatalf("two extractions logged %d Warns, want 2: %q", len(got), got)
	}

	debugsBefore := len(lg.damaged(lg.debugs))
	parse(withProbeOnlyCall(withExtractionState(context.Background())), "android_vr", short)
	if got := lg.damaged(lg.warns); len(got) != 2 {
		t.Errorf("a probe warned: %q", got[2:])
	}
	if got := lg.damaged(lg.debugs); len(got) != debugsBefore+1 || !strings.Contains(got[len(got)-1], "client android_vr") || !strings.Contains(got[len(got)-1], "itags [399]") {
		t.Errorf("a probe's report = %q, want one Debug line naming android_vr and itag 399", got[debugsBefore:])
	}
}

// damageTrack is damageFormat for an audio rendition of one track: its
// audioTrack id, displayName and audioIsDefault, as a dubbed video lists them.
func damageTrack(itag int, mime, approx, id, name string, isDefault bool) string {
	s := fmt.Sprintf(`{"itag": %d, "url": "https://x/%d-%s", "mimeType": %q, "bitrate": %d, "audioTrack": {"id": %q, "displayName": %q, "audioIsDefault": %v}`,
		itag, itag, id, mime, 1000+itag, id, name, isDefault)
	if approx != "" {
		s += `, "approxDurationMs": ` + approx
	}
	return s + "}"
}

// TestTheCollapseKeepsAnIntactTrackOverADamagedOne pins the damage rung where
// the selector cannot reach it: collapseToPreferredRendition, which keeps one
// rendition per itag before the selector ever sees the pool. yt-dlp keeps
// every track as a format of its own and ranks ie_pref (the -10 a damaged one
// carries, _video.py:3474) ahead of lang (utils/_utils.py:5363), so where
// every original-language copy is encoded short and the dub's is intact it
// archives the dub. The case runs from the wire, through two clients'
// responses, the dedup and the selector, so it also pins that the stamp
// survives the cross-client dedup and rides into the shadow: the short AV1
// would otherwise win the video.
//
// Mutants this kills:
//   - the collapse's damage rung dropped, or ranked after the track →
//     "every original copy is short": itag 251 and 140 keep the short
//     original, and the selector, with no intact audio left, archives it
//   - the collapse's damage rung inverted (a damaged copy preferred) → the
//     same row
//   - the stamp lost in the dedup → the short AV1 399 wins the video
//
// "an intact original elsewhere still beats the dub" is the guard the other
// way: the rung decides only between a damaged and an intact copy, so an
// intact original under another itag keeps the original language.
func TestTheCollapseKeepsAnIntactTrackOverADamagedOne(t *testing.T) {
	const (
		short = `"200000"`
		whole = `"600061"`
	)
	original := func(itag int, mime, approx string) string {
		return damageTrack(itag, mime, approx, "en.4", "English original", true)
	}
	dub := func(itag int, mime, approx string) string {
		return damageTrack(itag, mime, approx, "es.3", "Spanish", false)
	}
	for _, tc := range []struct {
		name      string
		audio     []string
		wantItag  int
		wantTrack string
		// per itag, the track the collapse must keep: the first entry of an
		// itag is what resolveFormatURLByItag returns at setup and on a 403
		// refresh, so the kept one is the one every itag lookup archives.
		wantKept map[int]string
	}{
		{
			name: "every original copy is short: the intact dub is archived",
			audio: []string{
				original(251, damageOpus, short), dub(251, damageOpus, whole),
				original(140, damageAAC, short), dub(140, damageAAC, whole),
			},
			wantItag: 251, wantTrack: "es.3",
			wantKept: map[int]string{251: "es.3", 140: "es.3"},
		},
		{
			name: "an intact original elsewhere still beats the dub",
			audio: []string{
				original(251, damageOpus, short), dub(251, damageOpus, whole),
				original(140, damageAAC, whole), dub(140, damageAAC, whole),
			},
			wantItag: 140, wantTrack: "en.4",
			wantKept: map[int]string{251: "es.3", 140: "en.4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlayerAPI(nil, noopLogger{})
			ctx := withExtractionState(context.Background())
			formats := append([]string{
				damageFormat(399, damageAV1, short), damageFormat(248, damageVP9, `"600046"`),
			}, tc.audio...)
			var pool []Format
			for _, c := range []struct {
				client string
				level  int
			}{{"tv", AuthLevelTVAuth}, {"web", AuthLevelWeb}} {
				info, err := p.parsePlayerResponse(withPlayerClient(ctx, c.client), damageResponse(t, "600", "", formats...), "", nil, damageVideoID)
				if err != nil {
					t.Fatalf("parsePlayerResponse (%s): %v", c.client, err)
				}
				collectFormats(&pool, info.Formats, c.client, c.level)
			}

			deduped := deduplicateFormats(ctx, pool)
			kept := map[int]*Format{}
			for i := range deduped {
				if _, seen := kept[deduped[i].Itag]; !seen {
					kept[deduped[i].Itag] = &deduped[i]
				}
			}
			for itag, want := range tc.wantKept {
				if f := kept[itag]; f == nil || f.AudioTrackID != want {
					t.Errorf("itag %d resolves to track %v, want %q", itag, trackOf(f), want)
				}
			}
			if av1 := kept[399]; av1 == nil || !av1.Damaged || av1.TokenAlternate == nil || !av1.TokenAlternate.Damaged {
				t.Errorf("the short AV1 after the dedup = %+v, want it and its web shadow still Damaged", av1)
			}

			got := SelectBestFormats(deduped, 1080, true)
			if got.Video == nil || got.Video.Itag != 248 {
				t.Errorf("video = %v, want the intact VP9 248", itagOf(got.Video))
			}
			if got.Audio == nil || got.Audio.Itag != tc.wantItag || got.Audio.AudioTrackID != tc.wantTrack || got.Audio.Damaged {
				t.Errorf("audio = itag %v track %v, want the intact itag %d %s", itagOf(got.Audio), trackOf(got.Audio), tc.wantItag, tc.wantTrack)
			}
		})
	}
}

func trackOf(f *Format) any {
	if f == nil {
		return "none"
	}
	return fmt.Sprintf("%q (damaged=%v)", f.AudioTrackID, f.Damaged)
}
