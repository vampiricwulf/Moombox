package engine

import (
	"context"
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
