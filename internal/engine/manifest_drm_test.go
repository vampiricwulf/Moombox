package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestParseHlsRecordsEncryption pins G32's parse half: a media playlist's
// first #EXT-X-KEY or #EXT-X-SESSION-KEY whose METHOD is not NONE — or an
// #EXT-X-FAXS-CM tag — is recorded on HlsPlaylist.Encryption, and DRM()
// tells the DRM systems yt-dlp's HlsFD._has_drm names (FairPlay by skd:// or
// its KEYFORMAT, PlayReady, Flash Access) from plain AES. parseMediaPlaylist
// used to have no case for the tags at all, so segments under a key were
// written as fetched.
//
// Mutant: dropping the #EXT-X-KEY case — every key row reads as clear.
// Mutant: matching #EXT-X-KEY: alone — the SESSION-KEY row reads as clear.
// Mutant: dropping parseHlsKey's NONE check — the NONE rows read as
// encrypted. Mutant: letting every key tag overwrite the last — the
// AES-then-NONE row reads as clear and the AES-then-FairPlay row names the
// second key. Mutant: dropping the #EXT-X-FAXS-CM case — the Flash Access
// row reads as clear. Mutant: DRM() without the skd:// test — the
// FairPlay-by-URI row reads as plain AES.
func TestParseHlsRecordsEncryption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tags    []string
		want    *HlsKey // nil: in the clear
		wantDRM bool
	}{
		{"no key at all", nil, nil, false},
		{"METHOD=NONE", []string{`#EXT-X-KEY:METHOD=NONE`}, nil, false},
		{"AES-128", []string{`#EXT-X-KEY:METHOD=AES-128,URI="https://k.example/key?a=1,b=2",IV=0x1234`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "AES-128", URI: "https://k.example/key?a=1,b=2"}, false},
		{"NONE, then AES-128 — the first key that is not NONE", []string{`#EXT-X-KEY:METHOD=NONE`, `#EXT-X-KEY:METHOD=AES-128,URI="k1"`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "AES-128", URI: "k1"}, false},
		{"AES-128, then NONE — the segments before the NONE are still ciphertext", []string{`#EXT-X-KEY:METHOD=AES-128,URI="k1"`, `#EXT-X-KEY:METHOD=NONE`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "AES-128", URI: "k1"}, false},
		{"AES-128, then FairPlay — the first names the playlist", []string{`#EXT-X-KEY:METHOD=AES-128,URI="k1"`, `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://k2"`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "AES-128", URI: "k1"}, false},
		{"FairPlay by its skd:// URI", []string{`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://abc"`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "SAMPLE-AES", URI: "skd://abc"}, true},
		{"FairPlay by KEYFORMAT", []string{`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:x",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "SAMPLE-AES", URI: "data:x", KeyFormat: "com.apple.streamingkeydelivery"}, true},
		{"PlayReady", []string{`#EXT-X-KEY:METHOD=SAMPLE-AES-CTR,URI="data:y",KEYFORMAT="com.microsoft.playready"`},
			&HlsKey{Tag: "EXT-X-KEY", Method: "SAMPLE-AES-CTR", URI: "data:y", KeyFormat: "com.microsoft.playready"}, true},
		{"a SESSION-KEY", []string{`#EXT-X-SESSION-KEY:METHOD=AES-128,URI="sk"`},
			&HlsKey{Tag: "EXT-X-SESSION-KEY", Method: "AES-128", URI: "sk"}, false},
		{"a SESSION-KEY of METHOD=NONE", []string{`#EXT-X-SESSION-KEY:METHOD=NONE`}, nil, false},
		{"Flash Access", []string{`#EXT-X-FAXS-CM:MII...`}, &HlsKey{Tag: "EXT-X-FAXS-CM"}, true},
		{"a key tag stating no METHOD", []string{`#EXT-X-KEY:URI="k"`}, &HlsKey{Tag: "EXT-X-KEY", URI: "k"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for _, tag := range tc.tags {
				b.WriteString(tag + "\n#EXTINF:2.0,\nseg.ts\n")
			}
			b.WriteString("#EXTINF:2.0,\nlast.ts\n")
			res := ParseHls(b.String(), "https://cdn.example/v/index.m3u8")
			if res == nil || res.Playlist == nil {
				t.Fatal("ParseHls returned no media playlist")
			}
			if got := len(res.Playlist.Segments); got != len(tc.tags)+1 {
				t.Errorf("segments = %d, want %d (a key tag is not a segment)", got, len(tc.tags)+1)
			}
			got := res.Playlist.Encryption
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("Encryption = %+v, want nil (in the clear)", got)
			case tc.want != nil && got == nil:
				t.Fatalf("Encryption = nil, want %+v", tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("Encryption = %+v, want %+v", got, tc.want)
			}
			if got != nil && got.DRM() != tc.wantDRM {
				t.Errorf("DRM() = %v, want %v", got.DRM(), tc.wantDRM)
			}
		})
	}
}

// TestHlsRefusesAnEncryptedPlaylistBeforeTheFirstWrite pins G32's download
// half: runHlsLoop refuses an encrypted media playlist with ErrDRMProtected
// on the round that fetched it, before any segment or init is fetched or a
// byte written — for a VOD (ENDLIST, which hands off to runHlsVodParallel),
// a live window, and an fMP4 playlist whose init comes first. The wording
// says which it is. Segments under a key were archived as ciphertext and the
// failure showed only at mux time.
//
// Mutant: dropping the `pl.Encryption != nil` refusal from runHlsLoop —
// every row fetches its segments and writes them. Mutant: moving the refusal
// below the VOD hand-off — the VOD row writes its segments.
func TestHlsRefusesAnEncryptedPlaylistBeforeTheFirstWrite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		endList   bool
		fmp4      bool
		stopOnGap bool
		wantWords string
	}{
		{"an AES-128 VOD", `#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`, true, false, false, "is encrypted (#EXT-X-KEY METHOD=AES-128)"},
		{"a live window under a SESSION-KEY", `#EXT-X-SESSION-KEY:METHOD=AES-128,URI="key.bin"`, false, false, true, "is encrypted (#EXT-X-SESSION-KEY METHOD=AES-128)"},
		{"a FairPlay fMP4 VOD", `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://k",KEYFORMAT="com.apple.streamingkeydelivery"`, true, true, false,
			"is DRM-protected (#EXT-X-KEY METHOD=SAMPLE-AES KEYFORMAT=com.apple.streamingkeydelivery)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var media atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/playlist.m3u8" {
					var b strings.Builder
					b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
					if tc.fmp4 {
						b.WriteString(`#EXT-X-MAP:URI="init.mp4"` + "\n")
					}
					b.WriteString(tc.key + "\n")
					for i := range 4 {
						fmt.Fprintf(&b, "#EXTINF:2.000,\nseg%d.ts\n", i)
					}
					if tc.endList {
						b.WriteString("#EXT-X-ENDLIST\n")
					}
					fmt.Fprint(w, b.String())
					return
				}
				media.Add(1) // a segment, the init or the key: none may be asked for
				fmt.Fprint(w, "CIPHERTEXT")
			}))
			t.Cleanup(srv.Close)

			out := filepath.Join(t.TempDir(), "video.ts")
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL: srv.URL + "/playlist.m3u8", OutputFile: out, StartSeq: -1, IsHls: true, StopOnGap: tc.stopOnGap,
			})
			d.delays = fastDelays()
			ctx, cancel := context.WithTimeout(t.Context(), safetyNet)
			defer cancel()
			err := d.Start(ctx)
			if !errors.Is(err, ErrDRMProtected) {
				t.Fatalf("Start = %v, want ErrDRMProtected", err)
			}
			if !strings.Contains(err.Error(), tc.wantWords) {
				t.Errorf("error = %q, want it to say %q", err, tc.wantWords)
			}
			if n := media.Load(); n != 0 {
				t.Errorf("%d segment/init requests made, want none before the refusal", n)
			}
			if fi, statErr := os.Stat(out); statErr == nil && fi.Size() != 0 {
				t.Errorf("output holds %d bytes, want nothing written", fi.Size())
			}
		})
	}
}

// TestParseDashSkipsContentProtection pins G32's DASH half: a Representation
// that carries ContentProtection, or whose AdaptationSet does, is left out of
// ParseDash's streams — yt-dlp's is_drm_protected marks it has_drm and the
// format is dropped (extractor/common.py, YoutubeDL.py) — and a manifest with
// nothing else left is ErrDRMProtected, in the DRM wording, so DashStrategy
// fails with it rather than archive ciphertext (and falls to the HLS
// manifest when there is one, drmHlsFallback). ParseDash took every Representation,
// so a protected manifest was recorded as ciphertext.
//
// Mutant: dropping the isDRMProtected skip — the protected representations
// come back. Mutant: isDRMProtected reading the AdaptationSet alone — the
// representation-level row keeps itag 137. Mutant: returning (nil, nil)
// when everything was skipped — the all-protected row gets no error.
func TestParseDashSkipsContentProtection(t *testing.T) {
	const cp = `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>
        <ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"><cenc:pssh>AAAA</cenc:pssh></ContentProtection>`
	mpd := func(setCP, repCP string) string {
		return `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      ` + setCP + `
      <Representation id="137" bandwidth="4000000" width="1920" height="1080">
        ` + repCP + `
        <BaseURL>https://cdn.example/137/</BaseURL>
      </Representation>
      <Representation id="136" bandwidth="2000000" width="1280" height="720">
        <BaseURL>https://cdn.example/136/</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="140" bandwidth="128000">
        <BaseURL>https://cdn.example/140/</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
	}
	itags := func(streams []DashStream) []int {
		var out []int
		for _, s := range streams {
			out = append(out, s.Itag)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		doc  string
		want []int
	}{
		{"in the clear", mpd("", ""), []int{137, 136, 140}},
		{"one Representation protected", mpd("", cp), []int{136, 140}},
		{"a video AdaptationSet protected", mpd(cp, ""), []int{140}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streams, err := ParseDash(tc.doc, "https://manifest.example/dash.mpd")
			if err != nil {
				t.Fatalf("ParseDash = %v, want the clean representations", err)
			}
			if got := itags(streams); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("itags = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("every Representation protected", func(t *testing.T) {
		doc := strings.ReplaceAll(mpd(cp, ""), `<AdaptationSet mimeType="audio/mp4">`, `<AdaptationSet mimeType="audio/mp4">`+cp)
		streams, err := ParseDash(doc, "https://manifest.example/dash.mpd")
		if !errors.Is(err, ErrDRMProtected) {
			t.Fatalf("ParseDash = (%v, %v), want ErrDRMProtected", itags(streams), err)
		}
		if !strings.Contains(err.Error(), "DRM-protected") || !strings.Contains(err.Error(), "ContentProtection (3 skipped)") {
			t.Errorf("error = %q, want the DRM wording and the count", err)
		}
	})
}

// TestParseDashDRMSaysWhichKindWasLost: ParseDashDRM reports a kind whose
// every Representation the DRM skip took (owner decision FE-1) — a protected
// video AdaptationSet beside clear audio parses without error, its audio
// kept, and says Video; one protected video Representation beside a clean
// one says nothing; every Representation protected says both beside
// ErrDRMProtected. The kind comes from the Representation's mimeType, else
// its AdaptationSet's.
//
// Mutants: the kind read from the AdaptationSet alone (the
// representation-mimeType row says nothing); a kind lost on any skip, kept
// ones ignored (the one-protected row says Video); the flags never set
// (the protected-set rows say nothing).
func TestParseDashDRMSaysWhichKindWasLost(t *testing.T) {
	const cp = `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`
	rep := func(id, mime, prot string) string {
		attr := ""
		if mime != "" {
			attr = ` mimeType="` + mime + `"`
		}
		return `<Representation id="` + id + `" bandwidth="1000"` + attr + `>` + prot + `<BaseURL>https://cdn.example/` + id + `/</BaseURL></Representation>`
	}
	set := func(mime, prot string, reps ...string) string {
		attr := ""
		if mime != "" {
			attr = ` mimeType="` + mime + `"`
		}
		return `<AdaptationSet` + attr + `>` + prot + strings.Join(reps, "") + `</AdaptationSet>`
	}
	doc := func(sets ...string) string {
		return `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period>` + strings.Join(sets, "") + `</Period></MPD>`
	}
	for _, tc := range []struct {
		name    string
		doc     string
		want    DashDRM
		wantErr bool
	}{
		{"in the clear", doc(set("video/mp4", "", rep("137", "", "")), set("audio/mp4", "", rep("140", "", ""))), DashDRM{}, false},
		{"protected video set, clear audio", doc(set("video/mp4", cp, rep("137", "", ""), rep("136", "", "")), set("audio/mp4", "", rep("140", "", ""))), DashDRM{Video: true}, false},
		{"protected audio set, clear video", doc(set("video/mp4", "", rep("137", "", "")), set("audio/mp4", cp, rep("140", "", ""))), DashDRM{Audio: true}, false},
		{"one video protected, one clear", doc(set("video/mp4", "", rep("137", "", cp), rep("136", "", "")), set("audio/mp4", "", rep("140", "", ""))), DashDRM{}, false},
		{"the kind on the Representation", doc(set("", "", rep("137", "video/mp4", cp), rep("140", "audio/mp4", ""))), DashDRM{Video: true}, false},
		{"everything protected", doc(set("video/mp4", cp, rep("137", "", "")), set("audio/mp4", cp, rep("140", "", ""))), DashDRM{Video: true, Audio: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, drm, err := ParseDashDRM(tc.doc, "https://manifest.example/dash.mpd")
			if (err != nil) != tc.wantErr || (tc.wantErr && !errors.Is(err, ErrDRMProtected)) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if drm != tc.want {
				t.Errorf("DashDRM = %+v, want %+v", drm, tc.want)
			}
		})
	}
}
