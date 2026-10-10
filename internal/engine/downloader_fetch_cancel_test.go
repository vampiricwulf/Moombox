package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countingReporter is an atomic ConnectivityReporter: the engine's reporter is
// a package global, so a plain-int recorder is not safe to install while any
// other test is in flight. It is the package's ONLY reporter double.
type countingReporter struct {
	fails     atomic.Int32
	successes atomic.Int32
}

func (c *countingReporter) ReportFailure(string) { c.fails.Add(1) }
func (c *countingReporter) ReportSuccess(string) { c.successes.Add(1) }

// goCancelAfter runs cancel after d in a goroutine carrying the project's
// inline recover (every goroutine has one). A panic there would otherwise take
// the whole test binary down with a stack naming only this line. It cannot
// call t.Fatal itself — no t method is legal from a goroutine that may outlive
// the test — so the value is handed back through a buffered channel and the
// returned join func, which the test body calls while it is still running,
// reports it against the right test.
func goCancelAfter(t *testing.T, d time.Duration, cancel context.CancelFunc) func() {
	t.Helper()
	panicked := make(chan any, 1)
	done := make(chan struct{})
	go func() {
		defer close(done) // registered first, so it runs AFTER the recover below
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		time.Sleep(d)
		cancel()
	}()
	return func() {
		t.Helper()
		<-done
		select {
		case r := <-panicked:
			t.Fatalf("the cancel goroutine panicked: %v", r)
		default:
		}
	}
}

// fetchSite is one of the six places the engine reports a connectivity
// failure from (reportFetchFailure: fetchSegment, probeHeadAt, probeFileSize
// and fetchChunk in downloader_fetch.go, streamDirectOnce in
// downloader_direct.go, and eviction_probe.go's ProbeSegmentAvailable). call
// drives exactly one of them to its error return and asserts the call failed
// where the signature exposes that; probeFileSize returns no error, so its
// row asserts a 0 size instead.
type fetchSite struct {
	name string
	call func(t *testing.T, ctx context.Context, d *SegmentDownloader)
}

var engineFetchSites = []fetchSite{
	{"fetchSegment", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.fetchSegment(ctx, d.buildSegmentURL(1)); err == nil {
			t.Fatal("fetchSegment returned nil error")
		}
	}},
	{"probeHeadAt", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, err := d.probeHeadAt(ctx, 999999999); err == nil {
			t.Fatal("probeHeadAt returned nil error")
		}
	}},
	{"probeFileSize", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if size, _ := d.probeFileSize(ctx); size != 0 {
			t.Fatalf("probeFileSize = %d, want 0 on a request that never answered", size)
		}
	}},
	{"fetchChunk", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.fetchChunk(ctx, 0, 1023); err == nil {
			t.Fatal("fetchChunk returned nil error")
		}
	}},
	{"streamDirectOnce", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.streamDirectOnce(ctx); err == nil {
			t.Fatal("streamDirectOnce returned nil error")
		}
	}},
	{"ProbeSegmentAvailable", func(t *testing.T, ctx context.Context, d *SegmentDownloader) {
		if _, _, err := d.ProbeSegmentAvailable(ctx, 1); err == nil {
			t.Fatal("ProbeSegmentAvailable returned nil error")
		}
	}},
}

// newFetchSiteDownloader builds a downloader pointed at srv with a throwaway
// output path. The query-style base URL is what buildSegmentURL turns into
// `&sq=N`, so every site below issues a real request at the test server.
func newFetchSiteDownloader(t *testing.T, srvURL string) *SegmentDownloader {
	t.Helper()
	return NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srvURL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
}

// TestEveryFetchSiteIgnoresACallerCancel pins T4-35 across all six sites: a
// shutdown, a quality split or a superseded refresh cancels the download
// context, the in-flight request dies with it, and that is a decision Moombox
// made — not evidence about the network. Counting it drags the connectivity
// oracle toward "offline" on every clean stop.
//
// The handler blocks until the client goes away, so the ONLY way each request
// ends is the caller's cancel. probeHeadAt and probeFileSize derive a
// hardcoded 10 s timeout, well past this test, so the parent cancel is the
// only thing that can finish them — which is exactly the guard under test.
//
// Mutant this kills: reporting unconditionally at any one of the six sites
// (that row's fails becomes 1). It does NOT discriminate a guard on parent
// from a guard on the SHADOWED derived ctx: cancelling parent finishes both.
// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure below is what kills
// that mutant.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestEveryFetchSiteIgnoresACallerCancel(t *testing.T) {
	for _, site := range engineFetchSites {
		t.Run(site.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)

			rec := &countingReporter{}
			SetConnectivityReporter(rec)
			t.Cleanup(func() { SetConnectivityReporter(nil) })

			ctx, cancel := context.WithCancel(t.Context())
			joinCancel := goCancelAfter(t, 50*time.Millisecond, cancel)
			site.call(t, ctx, newFetchSiteDownloader(t, srv.URL))
			joinCancel()
			cancel()

			if got := rec.fails.Load(); got != 0 {
				t.Errorf("connectivity failures = %d, want 0 — a caller cancel was recorded as a network failure", got)
			}
		})
	}
}

// TestEveryFetchSiteReportsATransportError is the other half across all six
// sites: with a perfectly healthy caller context, a request that dies on the
// wire IS network evidence and must still be reported.
//
// The server hijacks and closes without answering, which is a transport error
// at the client and happens instantly — long before any derived deadline — so
// each site's own context is still alive when it reports.
//
// Mutant this kills: suppressing every error outright at any of the six
// sites (that row's fails drops to 0), which is what a guard written against
// the wrong condition would do.
//
// Do not add t.Parallel(): the reporter is a package global.
func TestEveryFetchSiteReportsATransportError(t *testing.T) {
	for _, site := range engineFetchSites {
		t.Run(site.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("test server does not support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close() // no response at all
				}
			}))
			t.Cleanup(srv.Close)

			rec := &countingReporter{}
			SetConnectivityReporter(rec)
			t.Cleanup(func() { SetConnectivityReporter(nil) })

			site.call(t, t.Context(), newFetchSiteDownloader(t, srv.URL))

			if got := rec.fails.Load(); got != 1 {
				t.Errorf("connectivity failures = %d, want 1 — a transport error with a healthy caller IS network evidence", got)
			}
		})
	}
}

// TestFetchSegmentDerivedTimeoutIsAConnectivityFailure is the discriminator
// the two tables above cannot be: it is the only scenario where the parent
// stays alive but the DERIVED (shadowed) context is the one that finishes.
// SegmentTimeout is shrunk to a few milliseconds so the derived context times
// out against a server that never answers, while the caller's own context
// (t.Context()) never expires. A request that genuinely ran out of time with a
// healthy caller IS network evidence and must still be reported. Only
// fetchSegment and fetchChunk take their timeout from SegmentTimeout, and only
// fetchSegment needs no byte range, which is why this stays a single case
// rather than a sixth table.
//
// Mutant this kills: guarding on the SHADOWED/derived ctx instead of parent —
// fails would stay 0, exactly backwards from what a guard against
// caller-cancellation is supposed to catch.
//
// Do not add t.Parallel(): shrinks the package-global SegmentTimeout, and the
// reporter is a package global.
func TestFetchSegmentDerivedTimeoutIsAConnectivityFailure(t *testing.T) {
	orig := SegmentTimeout
	SegmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { SegmentTimeout = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	rec := &countingReporter{}
	SetConnectivityReporter(rec)
	t.Cleanup(func() { SetConnectivityReporter(nil) })

	d := newFetchSiteDownloader(t, srv.URL)
	if _, _, err := d.fetchSegment(t.Context(), d.buildSegmentURL(1)); err == nil {
		t.Fatal("fetchSegment against a server that never answers returned nil error")
	}
	if got := rec.fails.Load(); got != 1 {
		t.Errorf("connectivity failures = %d, want 1 — a derived-context timeout with a healthy caller IS network evidence", got)
	}
}

// TestFetchSegmentWithRetryReportsACancelOnTheFinalAttempt: the loop checks
// for cancellation only at the top of each attempt, so a cancel that landed
// during the LAST one fell out of the loop as ErrSegmentRetriesExhausted. A
// catch-up worker then logged "retries exhausted" and damped its window for a
// segment that never failed.
//
// Mutant: drop the cancelErr check after the loop — the error is
// ErrSegmentRetriesExhausted.
func TestFetchSegmentWithRetryReportsACancelOnTheFinalAttempt(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := deadlineTestContext(t)

	d := NewSegmentDownloader(DownloaderOptions{BaseURL: srv.URL, MaxRetries: 1})
	d.delays = fastDelays()

	errc := make(chan error, 1)
	go func() {
		_, err := d.fetchSegmentWithRetry(ctx, srv.URL+"/seg0", nil)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch never reached the server")
	}
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrSegmentRetriesExhausted) {
			t.Errorf("a cancelled fetch was reported as retries exhausted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetchSegmentWithRetry did not return within 5s of the cancel")
	}
}
