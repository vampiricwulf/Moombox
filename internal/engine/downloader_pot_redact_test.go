package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// secretLogger records every log line, message and arguments both, so a test
// can assert that no line carried a secret.
type secretLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *secretLogger) add(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(args...))
}
func (l *secretLogger) Debug(msg string, args ...any) { l.add("DEBUG", msg, args...) }
func (l *secretLogger) Info(msg string, args ...any)  { l.add("INFO", msg, args...) }
func (l *secretLogger) Warn(msg string, args ...any)  { l.add("WARN", msg, args...) }
func (l *secretLogger) Error(msg string, args ...any) { l.add("ERROR", msg, args...) }

func (l *secretLogger) containing(secret string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var hits []string
	for _, line := range l.lines {
		if strings.Contains(line, secret) {
			hits = append(hits, line)
		}
	}
	return hits
}

// TestHlsPlaylistPollErrorRedactsPathToken pins the engine half of W24-08.
// The YouTube HLS variant URL the downloader polls ends in /pot/<token>
// (strategy_youtube_hls.go appends it), and fetchSegment adds the same token
// as ?pot= besides. A playlist fetch that fails at the transport is a
// *url.Error quoting that URL, and the loop's give-up error — "HLS playlist
// fetch failed after N consecutive errors: %w" — carried the path copy into
// the job's error: the redactor knew only the query form.
//
// Mutant (run): redact.MediaError returning err unchanged — Start's error
// keeps both tokens. (redact.PoTokenURL without its redactPotPath call no
// longer fails this row: the variant URL is a media URL, whose path pairs
// redact.MediaURL cuts as well; TestPoTokenURL pins that call.)
func TestHlsPlaylistPollErrorRedactsPathToken(t *testing.T) {
	const pathToken, queryToken = "SECRETPATHTOKEN", "SECRETQUERYTOKEN"
	lg := &secretLogger{}
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    refusedBase(t) + "/api/manifest/hls_playlist/id/x/itag/95/playlist/index.m3u8/pot/" + pathToken,
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
		PoToken:    queryToken,
		Logger:     lg,
	})
	d.delays = fastDelays()
	d.delays.hlsPlaylistRetry = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil against a refused port, want the consecutive-error failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "consecutive errors") {
		t.Fatalf("Start() = %q, want the playlist give-up error", msg)
	}
	for _, secret := range []string{pathToken, queryToken} {
		if strings.Contains(msg, secret) {
			t.Errorf("Start() error carries the PO token %s: %q", secret, msg)
		}
		if hits := lg.containing(secret); len(hits) > 0 {
			t.Errorf("log lines carry the PO token %s: %q", secret, hits)
		}
	}
	if !strings.Contains(msg, "/pot/<redacted>") || !strings.Contains(msg, "pot=<redacted>") {
		t.Errorf("Start() = %q, want both token slots shown as <redacted>", msg)
	}
}

// TestFetchRequestBuildErrorsRedactPoToken pins the request-building half of
// every fetch that carries the token. http.NewRequestWithContext refuses a URL
// net/url cannot parse with a *url.Error that quotes it whole, and those sites
// returned that error bare — only the transport failure was redacted.
//
// Mutants (run): each row's site returning err instead of redact.MediaError(err)
// — fetchSegment, fetchChunk, probeHeadAt, ProbeSegmentAvailable and
// streamDirectOnce — fails that row alone.
func TestFetchRequestBuildErrorsRedactPoToken(t *testing.T) {
	const pathToken, queryToken = "SECRETPATHTOKEN", "SECRETQUERYTOKEN"
	// "%zz" is not an escape, so url.Parse refuses the URL.
	base := "http://127.0.0.1:1/api/%zz/pot/" + pathToken + "?itag=1"
	newD := func(t *testing.T) *SegmentDownloader {
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:    base,
			OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
			PoToken:    queryToken,
			Logger:     &secretLogger{},
		})
		d.delays = fastDelays()
		return d
	}
	rows := []struct {
		name string
		call func(context.Context, *SegmentDownloader) error
	}{
		{"fetchSegment", func(ctx context.Context, d *SegmentDownloader) error {
			_, _, err := d.fetchSegment(ctx, d.getBaseURL())
			return err
		}},
		{"fetchChunk", func(ctx context.Context, d *SegmentDownloader) error {
			_, _, err := d.fetchChunk(ctx, 0, 1)
			return err
		}},
		{"probeHeadAt", func(ctx context.Context, d *SegmentDownloader) error {
			_, err := d.probeHeadSequence(ctx)
			return err
		}},
		{"ProbeSegmentAvailable", func(ctx context.Context, d *SegmentDownloader) error {
			_, _, err := d.ProbeSegmentAvailable(ctx, 5)
			return err
		}},
		{"streamDirectOnce", func(ctx context.Context, d *SegmentDownloader) error {
			_, _, err := d.streamDirectOnce(ctx)
			return err
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := row.call(t.Context(), newD(t))
			if err == nil {
				t.Fatal("want the request-building error for an unparseable URL")
			}
			if !strings.Contains(err.Error(), "invalid URL escape") {
				t.Fatalf("error = %q, want url.Parse's refusal", err.Error())
			}
			for _, secret := range []string{pathToken, queryToken} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error carries the PO token %s: %q", secret, err.Error())
				}
			}
		})
	}
}

// TestProbeHeadTransportErrorRedactsPoToken covers probeHeadAt's transport
// failure, the one fetch whose Do error was returned bare. Its callers drop
// the error today; the contract is that no fetch error carries the token, so
// none can start to.
//
// Mutant (run): probeHeadAt returning the Do error bare.
func TestProbeHeadTransportErrorRedactsPoToken(t *testing.T) {
	const queryToken = "SECRETQUERYTOKEN"
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    refusedBase(t) + "/videoplayback?itag=136",
		OutputFile: filepath.Join(t.TempDir(), "video.mp4"),
		PoToken:    queryToken,
	})
	_, err := d.probeHeadSequence(t.Context())
	if err == nil {
		t.Fatal("probeHeadSequence() = nil against a refused port")
	}
	if strings.Contains(err.Error(), queryToken) {
		t.Fatalf("error carries the PO token: %q", err.Error())
	}
}

// refusedBase returns http://host:port on a loopback port nothing listens on.
func refusedBase(t *testing.T) string {
	t.Helper()
	u := refusedURL(t)
	return u[:strings.Index(u[len("http://"):], "/")+len("http://")]
}
