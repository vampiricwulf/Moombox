package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// refusedPotBase returns http://host:port on a loopback port nothing listens
// on, so a fetch fails at dial with a *url.Error quoting the full URL.
func refusedPotBase(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// loggedSecret reports the first captured line, message or argument, that
// carries secret.
func loggedSecret(logs *captureLogger, secret string) (string, bool) {
	for _, line := range logs.lines() {
		if s := fmt.Sprint(line...); strings.Contains(s, secret) {
			return s, true
		}
	}
	return "", false
}

// TestLiveManifestFetchErrorRedactsPoToken pins W24-08. A live job whose DASH
// or HLS manifest comes from a WEB-family client mints a GVS PO token and
// appends it to the manifest URL as a /pot/<token> path segment, then fetches
// through fetchURL. fetchURL returned http.Client.Do's *url.Error unchanged,
// so any transport failure carried the token into "setup download: fetch DASH
// manifest: Get \"…/pot/<token>\": …" — the job's stored error, the "job
// error" log line and the Job Failed embed.
//
// Mutant (run): fetchURL returning the Do error bare — both rows keep the
// token.
func TestLiveManifestFetchErrorRedactsPoToken(t *testing.T) {
	const secret = "SECRETPOTOKEN123"
	rows := []struct {
		name string
		run  func(*JobContext) error
	}{
		{"dash", func(job *JobContext) error {
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    refusedPotBase(t) + "/api/manifest/dash/id/abc",
				DashManifestSource: "web_creator",
			}
			_, err := DownloadDash(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			return err
		}},
		{"hls", func(job *JobContext) error {
			info := &youtube.VideoInfo{
				StreamStatus:      youtube.StreamLive,
				HlsManifestURL:    refusedPotBase(t) + "/api/manifest/hls_variant/id/abc/file/index.m3u8",
				HlsManifestSource: "web",
			}
			_, err := DownloadHls(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			return err
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			mints := fakeVodMint(t, secret, nil)
			job, logs := vodPotJob(t)
			err := row.run(job)
			if mints.Load() == 0 {
				t.Fatal("no GVS token was minted — the row does not exercise the tokenised URL")
			}
			if err == nil {
				t.Fatal("want a manifest fetch error from a refused port")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error carries the PO token: %q", err.Error())
			}
			if !strings.Contains(err.Error(), "/pot/<redacted>") {
				t.Errorf("error = %q, want the token's slot shown as /pot/<redacted>", err.Error())
			}
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Errorf("errors.As(*url.Error) lost on %q — transport classification needs it", err.Error())
			}
			if line, ok := loggedSecret(logs, secret); ok {
				t.Errorf("a job log line carries the PO token: %q", line)
			}
		})
	}
}

// TestJobErrorRedactsPoToken pins the job-error funnel: setJobError's one
// message feeds the "job error" log line, the stored error the dashboard and
// the TUI show, and the Job Failed embed. A token that reached it by a route
// no producer redacted — here a *url.Error flattened with %v, which leaves no
// *url.Error in the chain for redact.PoToken to find — is cut out there, in
// both forms, before any of the three sees it.
//
// Mutant (run): errMsg := err.Error() without redact.PoTokenText — the stored
// error, the log line and the embed's Error field all carry both tokens.
func TestJobErrorRedactsPoToken(t *testing.T) {
	const pathToken, queryToken = "SECRETPATHTOKEN", "SECRETQUERYTOKEN"
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	logs := &captureLogger{}
	w.logger = logs
	rec := notificationtest.New()
	w.notifier = rec

	job := &database.Job{ID: "yt_potfunnel", VideoID: "potfunnel", Platform: "youtube", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	ue := &url.Error{Op: "Get", URL: "https://rr1---sn-x.googlevideo.com/videoplayback/pot/" + pathToken + "?itag=1&pot=" + queryToken, Err: fmt.Errorf("connection reset by peer")}
	w.setJobError(job, fmt.Errorf("setup download: %v", ue))

	row, err := db.GetJob(job.ID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	stored := row.Error
	if stored == "" {
		t.Fatal("no error was stored")
	}
	calls := rec.ByEvent("error")
	if len(calls) != 1 {
		t.Fatalf("recorded %d Job Failed sends, want 1", len(calls))
	}
	embedErr := notifyField(t, calls[0], "Error")
	for _, secret := range []string{pathToken, queryToken} {
		if strings.Contains(stored, secret) {
			t.Errorf("stored job error carries %s: %q", secret, stored)
		}
		if strings.Contains(embedErr, secret) {
			t.Errorf("Job Failed embed's Error field carries %s: %q", secret, embedErr)
		}
		if line, ok := loggedSecret(logs, secret); ok {
			t.Errorf("a log line carries %s: %q", secret, line)
		}
	}
	if !strings.HasPrefix(stored, "setup download: Get ") || !strings.Contains(stored, "connection reset by peer") {
		t.Errorf("stored error = %q, want the rest of the message kept", stored)
	}
}

// TestFetchURLBuildErrorRedactsPoToken pins fetchURL's other error: a URL
// net/url refuses is a *url.Error quoting it whole, the /pot/ segment the
// live strategies append included.
//
// Mutant (run): fetchURL returning http.NewRequestWithContext's error bare.
func TestFetchURLBuildErrorRedactsPoToken(t *testing.T) {
	const secret = "SECRETPOTOKEN123"
	// "%zz" is not an escape, so url.Parse refuses the URL.
	_, _, err := fetchURL(context.Background(), "http://127.0.0.1:1/api/manifest/dash/id/%zz/pot/"+secret)
	if err == nil {
		t.Fatal("fetchURL accepted an unparseable URL")
	}
	if !strings.Contains(err.Error(), "invalid URL escape") {
		t.Fatalf("error = %q, want url.Parse's refusal", err.Error())
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error carries the PO token: %q", err.Error())
	}
}
