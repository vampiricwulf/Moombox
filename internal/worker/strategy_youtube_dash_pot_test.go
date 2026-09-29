package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// manifestServer serves body for every request and records each request's
// path, so a test can see whether the manifest fetch carried a /pot/ segment.
type manifestServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newManifestServer(t *testing.T, body string) *manifestServer {
	t.Helper()
	ms := &manifestServer{}
	ms.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		ms.paths = append(ms.paths, r.URL.Path)
		ms.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ms.Close)
	return ms
}

// onlyPath returns the one path the server saw; the strategies fetch their
// manifest exactly once.
func (ms *manifestServer) onlyPath(t *testing.T) string {
	t.Helper()
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if len(ms.paths) != 1 {
		t.Fatalf("manifest requests = %v, want exactly one", ms.paths)
	}
	return ms.paths[0]
}

// potLines returns the args of every line logged with msg, as key → value.
func potLines(logs *captureLogger, msg string) []map[string]any {
	var out []map[string]any
	for _, m := range logs.msgs {
		if m[0] != msg {
			continue
		}
		kv := map[string]any{}
		for i := 1; i+1 < len(m); i += 2 {
			if k, ok := m[i].(string); ok {
				kv[k] = m[i+1]
			}
		}
		out = append(out, kv)
	}
	return out
}

// assertSkipLines checks that exactly the wanted "[POT] GVS token skipped"
// lines were logged, in order, each naming the source (and stream, when the
// strategy decides per stream).
func assertSkipLines(t *testing.T, logs *captureLogger, want ...map[string]any) {
	t.Helper()
	got := potLines(logs, "[POT] GVS token skipped")
	if len(got) != len(want) {
		t.Fatalf("[POT] GVS token skipped lines = %v, want %d of them", got, len(want))
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("skip line %d: %s = %v, want %v (line %v)", i, k, got[i][k], v, got[i])
			}
		}
	}
}

const potTestMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <Representation id="299" bandwidth="9000000" width="1920" height="1080">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/299/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4">
      <Representation id="140" bandwidth="128000">
        <BaseURL>https://r4---sn.googlevideo.com/videoplayback/id/abc.1/itag/140/sq/1</BaseURL>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// TestDownloadDashAttachesWebPOOnlyToWebPOManifests pins the DASH half of
// the 2026-09-29 live-path fix. The manifest's recorded client decides: a
// TV/WEB manifest is minted for and carries the token on the manifest path
// and both segment downloaders, exactly as before; a visionos or android_vr
// manifest — clients upstream never attaches a WebPO to, and the pairing
// types.go names as the 2026-08-15 403 cause — is not minted for at all,
// and neither is a manifest whose client was never recorded.
//
// Mutant: dropping the IsWebPOSource gate fails every non-WebPO row (a mint,
// a /pot/ path, a token on both downloaders, no skip line).
func TestDownloadDashAttachesWebPOOnlyToWebPOManifests(t *testing.T) {
	for _, tc := range []struct {
		source  string
		wantPot bool
	}{
		{source: "tv_auth", wantPot: true},
		{source: "web_safari", wantPot: true},
		{source: "visionos"},
		{source: "android_vr_dash_fallback"},
		{source: ""},
	} {
		t.Run("source="+tc.source, func(t *testing.T) {
			calls := fakeVodMint(t, "tok123", nil)
			srv := newManifestServer(t, potTestMPD)
			job, logs := vodPotJob(t)
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    srv.URL + "/api/manifest/dash/id/abc",
				DashManifestSource: tc.source,
			}

			res, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			if err != nil {
				t.Fatalf("DownloadDash: %v", err)
			}
			if res.VideoDownloader == nil || res.AudioDownloader == nil {
				t.Fatalf("want both downloaders, got video=%v audio=%v", res.VideoDownloader, res.AudioDownloader)
			}
			path := srv.onlyPath(t)
			if tc.wantPot {
				if n := calls.Load(); n != 1 {
					t.Errorf("mint ran %d times, want 1", n)
				}
				if !strings.HasSuffix(path, "/pot/tok123") {
					t.Errorf("manifest path = %q, want the /pot/tok123 suffix", path)
				}
				for name, d := range map[string]interface{ PoToken() string }{"video": res.VideoDownloader, "audio": res.AudioDownloader} {
					if got := d.PoToken(); got != "tok123" {
						t.Errorf("%s downloader token = %q, want tok123", name, got)
					}
				}
				lines := potLines(logs, "[POT] GVS mint")
				if len(lines) != 1 || lines[0]["source"] != tc.source {
					t.Errorf("[POT] GVS mint lines = %v, want one naming source=%s", lines, tc.source)
				}
				assertSkipLines(t, logs)
				return
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("mint ran %d times, want 0 — a %q manifest takes no WebPO token", n, tc.source)
			}
			if strings.Contains(path, "/pot/") {
				t.Errorf("manifest path = %q, want no /pot/ segment", path)
			}
			for name, d := range map[string]interface{ PoToken() string }{"video": res.VideoDownloader, "audio": res.AudioDownloader} {
				if got := d.PoToken(); got != "" {
					t.Errorf("%s downloader token = %q, want none", name, got)
				}
			}
			assertSkipLines(t, logs, skipLine(job, tc.source))
		})
	}
}

// skipLine is the "[POT] GVS token skipped" line a URL from source must
// produce: the source by name (or "unknown" when none was recorded) and why.
func skipLine(job *JobContext, source string, extra ...any) map[string]any {
	want := map[string]any{"jobID": job.Job.ID, "source": source, "reason": "non-WebPO client"}
	if source == "" {
		want["source"], want["reason"] = "unknown", "source not recorded"
	}
	for i := 0; i+1 < len(extra); i += 2 {
		want[extra[i].(string)] = extra[i+1]
	}
	return want
}
