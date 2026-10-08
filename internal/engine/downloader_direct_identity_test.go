package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestResumeIdentityWholeFileVodURL pins the whole-file VOD shape of the URL
// fingerprint. A finished VOD's format URL carries `id=o-…`, an opaque token
// with no `.N` stream suffix, so streamIdentity used to extract nothing from
// it: every whole-file resume compared "" with "" and was trusted, and a
// restart whose selection now picked another itag appended that rendition's
// bytes to the old one's checkpoint. The fingerprint is the itag plus `clen`;
// the `o-` token and every session parameter may rotate.
//
// Mutant: `streamIdentityWholeFileIDRe.MatchString(rawURL)` → `false` in
// streamIdentity — the different-itag case reads as the same stream. Mutant:
// dropping the `clen` append — the re-encode case does.
func TestResumeIdentityWholeFileVodURL(t *testing.T) {
	const (
		vod399     = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=399&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod399Next = "https://rr4---sn-b.googlevideo.com/videoplayback?expire=1700021600&ei=BBB&ip=5.6.7.8&id=o-BQrotated99&itag=399&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod137     = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=137&source=youtube&mime=video%2Fmp4&clen=73400320&dur=600.000"
		vod399Redo = "https://rr1---sn-a.googlevideo.com/videoplayback?expire=1700000000&ei=AAA&ip=1.2.3.4&id=o-AKzZqYexample&itag=399&source=youtube&mime=video%2Fmp4&clen=81920000&dur=600.000"
	)
	for _, tc := range []struct {
		name         string
		saved, now   string
		wantMismatch bool
	}{
		{"same rendition, every session parameter and the o- token rotated — resume", vod399, vod399Next, false},
		{"another itag — a different rendition, start over", vod399, vod137, true},
		{"same itag, another clen — a re-encode, start over", vod399, vod399Redo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := resumeIdentityMismatch(&ResumeState{BaseURL: tc.saved}, "", tc.now)
			if got != tc.wantMismatch {
				t.Errorf("mismatch = %v (reason %q, identities %q / %q), want %v",
					got, reason, streamIdentity(tc.saved), streamIdentity(tc.now), tc.wantMismatch)
			}
		})
	}
}

// headedBody is n bytes of fill behind an ftyp box header, so the finished
// file passes validateDownloadedMP4 and each rendition's bytes are countable.
func headedBody(n int, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, n)
	copy(b, "\x00\x00\x00\x18ftypdash")
	return b
}

// TestDirectResumeRefusesADifferentTotal pins the probed-total half of the
// whole-file resume check. The sidecar records the total the partial was a
// prefix of; a resume whose own probe answers another total is a different
// file — here behind a URL with no fingerprint and no StreamID, the shape the
// identity check cannot see — and starts over instead of appending.
//
// Mutant: dropping `TotalSize: d.directTotalSize` from saveResume — the
// sidecar records no total and the resume splices. Mutant: dropping
// `d.directTotalSize = state.TotalSize` from Start — the same splice. Mutant:
// dropping the `totalSize != d.directTotalSize` arm in runDirectDownload —
// the same splice.
func TestDirectResumeRefusesADifferentTotal(t *testing.T) {
	bodyA := headedBody(2*DownloadChunkSize+100, 'A')
	bodyB := headedBody(3*DownloadChunkSize, 'B')
	out := filepath.Join(t.TempDir(), "video.mp4")

	// Run 1: rendition A, cut after its first chunk was checkpointed.
	full := serveRangeFile(bodyA)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		if start >= DownloadChunkSize {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		full(w, r)
	}))
	defer srvA.Close()
	d1 := NewSegmentDownloader(DownloaderOptions{BaseURL: srvA.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d1.delays = fastDelays()
	d1.directResumeIntervalOverride = DownloadChunkSize
	if err := d1.Start(t.Context()); err == nil {
		t.Fatal("run 1 Start = nil, want the 404 that cuts it")
	}
	var saved ResumeState
	if data, err := os.ReadFile(out + resumeFileSuffix); err != nil {
		t.Fatalf("run 1 left no sidecar: %v", err)
	} else if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.TotalSize != int64(len(bodyA)) || saved.BytesWritten != DownloadChunkSize {
		t.Fatalf("sidecar = %+v, want totalSize %d at %d bytes", saved, len(bodyA), DownloadChunkSize)
	}

	// Run 2: the same path now serves a file of another length.
	srvB := httptest.NewServer(serveRangeFile(bodyB))
	defer srvB.Close()
	d2 := NewSegmentDownloader(DownloaderOptions{BaseURL: srvB.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d2.delays = fastDelays()
	if err := d2.Start(t.Context()); err != nil {
		t.Fatalf("run 2 Start: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, bodyB) {
		t.Errorf("output is %d bytes with %d of rendition A — want rendition B alone (%d bytes), not A's checkpoint with B appended",
			len(got), bytes.Count(got, []byte("A")), len(bodyB))
	}
}

// TestDirectResumeRefusesAPartialLongerThanTheFile pins the legacy half: a
// sidecar written before the total was recorded cannot be compared, but a
// partial already longer than the probed file is still a different file.
// The loop never ran for it — the offset was past the total — so the old
// partial "completed" as it stood.
//
// Mutant: dropping the `staged > totalSize` arm in runDirectDownload — Start
// returns nil over rendition A's partial.
func TestDirectResumeRefusesAPartialLongerThanTheFile(t *testing.T) {
	staged := headedBody(3000, 'A')
	bodyB := headedBody(2000, 'B')
	out := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(out, staged, 0o644); err != nil {
		t.Fatal(err)
	}
	store := utils.ResumeStore[ResumeState]{Path: out + resumeFileSuffix}
	if err := store.Save(ResumeState{BytesWritten: int64(len(staged)), Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(serveRangeFile(bodyB))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL + "/video.mp4", OutputFile: out, IsDirectURL: true})
	d.delays = fastDelays()
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, bodyB) {
		t.Errorf("output is %d bytes with %d of the old partial — want the %d-byte file the origin serves",
			len(got), bytes.Count(got, []byte("A")), len(bodyB))
	}
}
