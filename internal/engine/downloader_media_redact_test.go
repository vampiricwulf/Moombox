package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mediaClientIP and mediaSig stand in for the operator's public address and
// the URL's signature in a signed googlevideo URL.
const (
	mediaClientIP = "203.0.113.77"
	mediaSig      = "AOq0SECRETSIGVALUE"
)

// TestDirectDownloadErrorRedactsSignedURL pins the engine half of the signed
// googlevideo URL leak. A whole-file VOD download whose host refuses ends with
// http.Client.Do's *url.Error, which quotes the request URL — the format's
// /videoplayback URL, carrying ip=<the operator's public IP> and the URL's
// sig= — and that text became the job's error, the "job error" log line and
// the Job Failed embed's Error field. Only the PO token was cut.
//
// Mutant (run): MediaError's walker given PoTokenURL instead of MediaURL (the
// rule before this fix) — Start's error keeps the IP and the signature.
func TestDirectDownloadErrorRedactsSignedURL(t *testing.T) {
	const potToken = "SECRETQUERYTOKEN"
	lg := &secretLogger{}
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     refusedBase(t) + "/videoplayback?expire=1&ei=x&ip=" + mediaClientIP + "&id=o-abc&itag=140&source=youtube&sig=" + mediaSig,
		OutputFile:  filepath.Join(t.TempDir(), "audio.m4a"),
		IsDirectURL: true,
		PoToken:     potToken,
		Logger:      lg,
	})
	d.delays = fastDelays()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil against a refused port")
	}
	msg := err.Error()
	for _, secret := range []string{mediaClientIP, mediaSig, potToken} {
		if strings.Contains(msg, secret) {
			t.Errorf("Start() error carries %s: %q", secret, msg)
		}
		if hits := lg.containing(secret); len(hits) > 0 {
			t.Errorf("log lines carry %s: %q", secret, hits)
		}
	}
	if !strings.Contains(msg, "ip=<redacted>") || !strings.Contains(msg, "itag=140") {
		t.Errorf("Start() = %q, want the IP's slot shown as <redacted> and the itag kept", msg)
	}
}

// TestURLPrefixLogLineRedactsSignedURL pins the log-line half: the engine logs
// the first 120 characters of a failing media URL ("url_prefix", and the HLS
// init segment's "uri"/"newInit"), and a googlevideo URL carries the client's
// public IP within them. truncateURL, which all six of those sites call, now
// cuts the URL's credentials before it truncates.
//
// Mutant (run): truncateURL without its redact.MediaURL call — the direct
// download's "direct URL failed" line and the unit row keep the IP.
func TestURLPrefixLogLineRedactsSignedURL(t *testing.T) {
	signed := "https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1700000000&ei=x&ip=" + mediaClientIP + "&id=o-abc&itag=140&sig=" + mediaSig
	if got := truncateURL(signed, 120); strings.Contains(got, mediaClientIP) || strings.Contains(got, "SECRET") {
		t.Errorf("truncateURL = %q keeps a credential", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	lg := &secretLogger{}
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     srv.URL + "/videoplayback?expire=1&ip=" + mediaClientIP + "&itag=140&sig=" + mediaSig,
		OutputFile:  filepath.Join(t.TempDir(), "audio.m4a"),
		IsDirectURL: true,
		Logger:      lg,
	})
	d.delays = fastDelays()
	status, _, err := d.streamDirectOnce(t.Context())
	if status != http.StatusForbidden || err == nil {
		t.Fatalf("streamDirectOnce = %d, %v; want the 403", status, err)
	}
	if len(lg.containing("url_prefix")) == 0 {
		t.Fatal("no url_prefix line was logged — the row does not reach the log site")
	}
	for _, secret := range []string{mediaClientIP, mediaSig} {
		if hits := lg.containing(secret); len(hits) > 0 {
			t.Errorf("log lines carry %s: %q", secret, hits)
		}
	}
}

// TestTwitchHlsErrorsRedactSessionPath pins the Twitch half. The variant
// playlist a Twitch live capture polls — a video-weaver
// /v1/playlist/<session>.m3u8 — and every segment it lists — an edge's
// /v1/segment/<session>.ts — carry the viewer's playback session as their
// path, and a fetch that failed quoted it whole: into the give-up error the
// job stores (its error column, the "job error" line, the Job Failed embed)
// and into the segment's retry line. The media rule read no Twitch URL.
//
// Mutant (run): redact.MediaURL without its isTwitchMedia branch — both rows
// keep their session, in the error and in the log lines alike.
func TestTwitchHlsErrorsRedactSessionPath(t *testing.T) {
	const weaverSession, edgeSession = "CsoESECRETWEAVERSESSION", "CuwESECRETEDGESESSION"

	t.Run("playlist poll", func(t *testing.T) {
		lg := &secretLogger{}
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:    refusedBase(t) + "/v1/playlist/" + weaverSession + ".m3u8",
			OutputFile: filepath.Join(t.TempDir(), "video.ts"),
			StartSeq:   -1,
			IsHls:      true,
			Logger:     lg,
		})
		d.delays = fastDelays()
		d.delays.hlsPlaylistRetry = 10 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		err := d.Start(ctx)
		if err == nil || !strings.Contains(err.Error(), "consecutive errors") {
			t.Fatalf("Start() = %v, want the playlist give-up error", err)
		}
		msg := err.Error()
		if strings.Contains(msg, "SECRET") || len(lg.containing("SECRET")) > 0 {
			t.Errorf("the session reached the error or a log line: %q / %q", msg, lg.containing("SECRET"))
		}
		if !strings.Contains(msg, "/v1/playlist/<redacted>.m3u8") {
			t.Errorf("Start() = %q, want the playlist's session shown as <redacted>", msg)
		}
	})

	t.Run("segment", func(t *testing.T) {
		edge := refusedBase(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n"+
				"#EXTINF:2.000,live\n"+edge+"/v1/segment/"+edgeSession+".ts\n")
		}))
		defer srv.Close()
		lg := &secretLogger{}
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:    srv.URL + "/v1/playlist/" + weaverSession + ".m3u8",
			OutputFile: filepath.Join(t.TempDir(), "video.ts"),
			StartSeq:   -1,
			IsHls:      true,
			Logger:     lg,
		})
		d.delays = fastDelays()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("Start panicked: %v", r)
				}
			}()
			done <- d.Start(ctx)
		}()
		// The live loop retries the segment until the context ends; stop it
		// once the retry line has been written.
		for len(lg.containing("HLS segment failed")) == 0 && ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("Start() error carries a session: %q", err.Error())
		}
		if len(lg.containing("/v1/segment/<redacted>.ts")) == 0 {
			t.Fatalf("no line names the failed segment's redacted URL — the row does not reach the retry line: %q", lg.lines)
		}
		if hits := lg.containing("SECRET"); len(hits) > 0 {
			t.Errorf("log lines carry a session: %q", hits)
		}
	})
}
