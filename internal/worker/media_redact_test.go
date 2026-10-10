package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// mediaClientIP and mediaSig stand in for the operator's public address and
// the URL's signature in a signed googlevideo URL.
const (
	mediaClientIP = "203.0.113.77"
	mediaSig      = "AOq0SECRETSIGVALUE"
)

// assertJobErrorClean fails when the stored job error, the Job Failed embed's
// Error field or any captured log line carries one of secrets.
func assertJobErrorClean(t *testing.T, db *database.Database, jobID string, rec *notificationtest.Recorder, logs *captureLogger, secrets ...string) string {
	t.Helper()
	row, err := db.GetJob(jobID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Error == "" {
		t.Fatal("no error was stored")
	}
	calls := rec.ByEvent("error")
	if len(calls) != 1 {
		t.Fatalf("recorded %d Job Failed sends, want 1", len(calls))
	}
	embedErr := notifyField(t, calls[0], "Error")
	for _, secret := range secrets {
		if strings.Contains(row.Error, secret) {
			t.Errorf("stored job error carries %s: %q", secret, row.Error)
		}
		if strings.Contains(embedErr, secret) {
			t.Errorf("Job Failed embed's Error field carries %s: %q", secret, embedErr)
		}
		if line, ok := loggedSecret(logs, secret); ok {
			t.Errorf("a log line carries %s: %q", secret, line)
		}
	}
	return row.Error
}

// TestLiveManifestFetchErrorRedactsSignedURL pins the signed-URL half of the
// live manifest leak. A live DASH or HLS manifest URL is a googlevideo URL
// signed for this client: /ip/<the operator's public IP>/ and /sig/<the
// signature> sit in its path, whether or not a PO token rides with them. A
// transport failure on the fetch quoted it whole into "setup download: fetch
// DASH manifest: Get \"…\"", and from there into the job's stored error, the
// "job error" log line and the Job Failed embed — the operator's address to
// everyone who reads the webhook channel.
//
// Mutant (run): redact.MediaError's walker given redact.PoTokenURL instead of
// redact.MediaURL (the rule before this fix) — the strategy error of both rows
// keeps the IP and the signature. (setJobError's backstop still cleans the
// stored error then; TestJobErrorRedactsSignedURL pins that one.)
func TestLiveManifestFetchErrorRedactsSignedURL(t *testing.T) {
	signedPath := "/expire/1900000000/ei/x/ip/" + mediaClientIP + "/id/abc.1/source/yt_live_broadcast/sparams/expire,ei,ip,id,source/sig/" + mediaSig
	rows := []struct {
		name string
		run  func(*JobContext) error
	}{
		{"dash", func(job *JobContext) error {
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    refusedPotBase(t) + "/api/manifest/dash" + signedPath,
				DashManifestSource: "android_vr",
			}
			_, err := DownloadDash(context.Background(), job, info, nil, nil, nil, nil)
			return err
		}},
		{"hls", func(job *JobContext) error {
			info := &youtube.VideoInfo{
				StreamStatus:      youtube.StreamLive,
				HlsManifestURL:    refusedPotBase(t) + "/api/manifest/hls_variant" + signedPath + "/file/index.m3u8",
				HlsManifestSource: "android_vr",
			}
			_, err := DownloadHls(context.Background(), job, info, nil, nil, nil, nil)
			return err
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			jobCtx, jobLogs := vodPotJob(t)
			dlErr := row.run(jobCtx)
			if dlErr == nil {
				t.Fatal("want a manifest fetch error from a refused port")
			}
			for _, secret := range []string{mediaClientIP, mediaSig} {
				if strings.Contains(dlErr.Error(), secret) {
					t.Errorf("strategy error carries %s: %q", secret, dlErr.Error())
				}
				if line, ok := loggedSecret(jobLogs, secret); ok {
					t.Errorf("a job log line carries %s: %q", secret, line)
				}
			}
			var ue *url.Error
			if !errors.As(dlErr, &ue) {
				t.Errorf("errors.As(*url.Error) lost on %q — transport classification needs it", dlErr.Error())
			}

			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			logs := &captureLogger{}
			w.logger = logs
			rec := notificationtest.New()
			w.notifier = rec
			job := &database.Job{ID: "yt_signed_" + row.name, VideoID: "signed" + row.name, Platform: "youtube", Status: database.StatusDownloading}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			w.setJobError(job, fmt.Errorf("setup download: %w", dlErr))
			stored := assertJobErrorClean(t, db, job.ID, rec, logs, mediaClientIP, mediaSig)
			// The cause in the platform's own words: "connect: connection
			// refused" on Linux, "connectex: No connection could be made…"
			// on Windows.
			var opErr *net.OpError
			if !errors.As(dlErr, &opErr) {
				t.Fatalf("errors.As(*net.OpError) lost on %q", dlErr.Error())
			}
			if !strings.Contains(stored, "/ip/<redacted>/") || !strings.Contains(stored, opErr.Err.Error()) {
				t.Errorf("stored error = %q, want the IP's slot shown as <redacted> and the cause (%q) kept", stored, opErr.Err.Error())
			}
		})
	}
}

// TestJobErrorRedactsSignedURL pins setJobError's backstop for the signed
// URL: a whole-file VOD whose host refuses ends with
// `download: Get "https://rr3---sn-….googlevideo.com/videoplayback?…&ip=…&sig=…"`,
// and an error flattened with %v on the way leaves no *url.Error for the
// producers' redact.MediaError to find. The one message setJobError hands the
// "job error" line, the stored error and the Job Failed embed is cut there.
//
// Mutant (run): setJobError's errMsg through redact.PoTokenText (the rule
// before this fix) instead of redact.MediaText — the stored error, the log
// line and the embed's Error field all keep the IP and the signature.
func TestJobErrorRedactsSignedURL(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	logs := &captureLogger{}
	w.logger = logs
	rec := notificationtest.New()
	w.notifier = rec
	job := &database.Job{ID: "yt_signedfunnel", VideoID: "signedfunnel", Platform: "youtube", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	ue := &url.Error{
		Op:  "Get",
		URL: "https://rr3---sn-abc.googlevideo.com/videoplayback?expire=1&ei=x&ip=" + mediaClientIP + "&id=o-abc&itag=140&source=youtube&sig=" + mediaSig + "&pot=<redacted>",
		Err: errors.New("dial tcp: connect: connection refused"),
	}
	w.setJobError(job, fmt.Errorf("download: %v", ue))
	stored := assertJobErrorClean(t, db, job.ID, rec, logs, mediaClientIP, mediaSig)
	if !strings.Contains(stored, "ip=<redacted>") || !strings.Contains(stored, "itag=140") || !strings.Contains(stored, "connection refused") {
		t.Errorf("stored error = %q, want the IP's slot shown as <redacted>, the itag and the cause kept", stored)
	}
}

// TestDecryptNParamParseErrorRedactsSignedURL pins the cipher retry's one
// error that quotes a media URL: url.Parse's refusal of the segment URL,
// which the DASH 403 retry logs at Warn ("[Cipher] retry: n-param decrypt
// failed").
//
// Mutant (run): decryptNParamInURL returning url.Parse's error bare — the
// error keeps the IP and the signature.
func TestDecryptNParamParseErrorRedactsSignedURL(t *testing.T) {
	// "%zz" is not an escape, so url.Parse refuses the URL.
	raw := "https://rr3---sn-abc.googlevideo.com/videoplayback/%zz?ip=" + mediaClientIP + "&sig=" + mediaSig + "&n=abc"
	_, err := decryptNParamInURL(raw, func(s string) (string, error) { return s, nil })
	if err == nil {
		t.Fatal("decryptNParamInURL accepted an unparseable URL")
	}
	if !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("error = %q, want url.Parse's refusal", err.Error())
	}
	for _, secret := range []string{mediaClientIP, mediaSig} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error carries %s: %q", secret, err.Error())
		}
	}
}
