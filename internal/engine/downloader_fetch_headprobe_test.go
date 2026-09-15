package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestProbeHeadSequenceSkipsFallbackOnTransportError pins T3-29: during an
// outage the first probe never gets a response at all, and retrying at
// currentSeq+1000 just pays a second doomed round-trip. The fallback exists
// for an edge that ANSWERED without the header, not for a dead network.
//
// The server hijacks and closes the connection, which is a transport error
// (EOF) at the client while still letting the handler count the attempt.
//
// Mutant this kills: the pre-fix `else if cur > 0` with no error
// discrimination — probes becomes 2.
func TestProbeHeadSequenceSkipsFallbackOnTransportError(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close() // no response at all: a transport error at the client
		}
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500) // > 0, so the fallback WOULD be reachable

	if _, err := d.probeHeadSequence(t.Context()); err == nil {
		t.Fatal("probeHeadSequence() = nil error against a server that answers nothing")
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("server saw %d probes, want exactly 1 — a transport error must not trigger the currentSeq+1000 fallback", got)
	}
}

// TestProbeHeadSequenceFallsBackOnMissingHeader pins the half that must
// SURVIVE the fix: an edge that answers 200 without X-Head-Seqnum (rejected
// the absurd sequence, served an opaque error page) still gets the
// near-future retry, and its value is returned.
//
// Mutant this kills: deleting the fallback entirely, or widening the
// sentinel check to something a missing header does not satisfy.
func TestProbeHeadSequenceFallsBackOnMissingHeader(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if strings.Contains(r.URL.String(), "999999999") {
			fmt.Fprint(w, "nope") // answered, but no usable header
			return
		}
		w.Header().Set("X-Head-Seqnum", "777")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500)

	seq, err := d.probeHeadSequence(t.Context())
	if err != nil {
		t.Fatalf("probeHeadSequence() = %v, want the fallback's answer", err)
	}
	if seq != 777 {
		t.Errorf("probeHeadSequence() = %d, want 777 (the fallback probe's header)", seq)
	}
	if got := probes.Load(); got != 2 {
		t.Errorf("server saw %d probes, want 2 — the fallback must still fire when the edge answered without a usable header", got)
	}
}

// TestProbeHeadSequenceFallsBackOnAnUnparseableHeader pins the sub-case Arc 2
// Task 4's review parked: the edge ANSWERED and sent X-Head-Seqnum, but the
// value is not a number. probeHeadAt wraps that as
// `%w: parse %q: %v` around errNoHeadSeqUsable, so it is still an
// answered-but-unusable outcome and the currentSeq+1000 fallback must fire —
// exactly as it does for a missing header. An opaque error page from a CDN
// that stamps a non-numeric header is the field shape.
//
// Mutant this kills: comparing with `err == errNoHeadSeqUsable` instead of
// errors.Is in probeHeadSequence — the wrapped parse error then fails the
// check, no fallback runs, and the server sees 1 probe instead of 2.
func TestProbeHeadSequenceFallsBackOnAnUnparseableHeader(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if strings.Contains(r.URL.String(), "999999999") {
			w.Header().Set("X-Head-Seqnum", "not-a-number") // present, unparseable
			fmt.Fprint(w, "nope")
			return
		}
		w.Header().Set("X-Head-Seqnum", "1500")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
	})
	d.currentSeq.Store(500) // > 0, so the fallback is reachable

	seq, err := d.probeHeadSequence(t.Context())
	if err != nil {
		t.Fatalf("probeHeadSequence() = %v, want the fallback's answer", err)
	}
	if seq != 1500 {
		t.Errorf("probeHeadSequence() = %d, want 1500 (the fallback probe's header)", seq)
	}
	if got := probes.Load(); got != 2 {
		t.Errorf("server saw %d probes, want 2 — an unparseable header is answered-but-unusable, same as a missing one", got)
	}
}
