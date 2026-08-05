package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestFindOldestAvailableSeq is the brief's canonical bisection case: a
// pure probe function (no HTTP) marks everything below `front` unavailable
// and everything at/above it available. The bisection must land exactly on
// front.
func TestFindOldestAvailableSeq(t *testing.T) {
	const front = 437123
	probe := func(_ context.Context, seq int) (bool, error) { return seq >= front, nil }
	got, err := FindOldestAvailableSeq(context.Background(), 500000, probe)
	if err != nil || got != front {
		t.Fatalf("FindOldestAvailableSeq = %d, %v; want %d", got, err, front)
	}
}

// TestFindOldestAvailableSeqDeadURL models a URL that 403s everywhere,
// including the head itself — a dead/expired URL, not an eviction. The
// bisection must error rather than report a bogus "everything evicted"
// boundary.
func TestFindOldestAvailableSeqDeadURL(t *testing.T) {
	probe := func(context.Context, int) (bool, error) { return false, nil } // everything 403s
	if got, err := FindOldestAvailableSeq(context.Background(), 500000, probe); err == nil {
		t.Fatalf("dead URL must error, got %d", got)
	}
}

// TestFindOldestAvailableSeqRetriesTransient models one transient probe
// error (timeout) exactly at the point the bisection lands on, followed by
// a successful retry. A single flake must not abort the search or corrupt
// the result.
func TestFindOldestAvailableSeqRetriesTransient(t *testing.T) {
	flaky := 0
	probe := func(_ context.Context, seq int) (bool, error) {
		if seq == 250000 && flaky == 0 {
			flaky++
			return false, errors.New("timeout")
		}
		return seq >= 200000, nil
	}
	got, err := FindOldestAvailableSeq(context.Background(), 500000, probe)
	if err != nil || got != 200000 {
		t.Fatalf("= %d, %v; want 200000", got, err)
	}
}

// TestFindOldestAvailableSeqFakeGVS is the end-to-end case: a real
// SegmentDownloader's ProbeSegmentAvailable driving the bisection against a
// fake GVS server (query-style &sq=N addressing, 403 below the retention
// boundary and 200 above it — the real eviction shape). The bisection is
// O(log head) probes (~20 round trips for head=500000), so the brief's
// stated head/front values run fast against a local httptest server; no
// need to shrink them.
func TestFindOldestAvailableSeqFakeGVS(t *testing.T) {
	t.Parallel()
	const head = 500000
	const front = 437123
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq < front {
			return http.StatusForbidden
		}
		return http.StatusOK
	})

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/videoplayback?id=itest.evict&itag=140",
	})
	probe := func(ctx context.Context, seq int) (bool, error) {
		avail, _, err := d.ProbeSegmentAvailable(ctx, seq)
		return avail, err
	}

	got, err := FindOldestAvailableSeq(context.Background(), head, probe)
	if err != nil || got != front {
		t.Fatalf("FindOldestAvailableSeq (fake GVS) = %d, %v; want %d", got, err, front)
	}
}
