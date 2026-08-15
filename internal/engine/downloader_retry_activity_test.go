package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFetchSegmentWithRetryEmitsRetrying pins the ActivityRetrying emission:
// a failed attempt must surface the backoff in the progress line before the
// retry sleep, so a stalled segment fetch doesn't read as a frozen download.
func TestFetchSegmentWithRetryEmitsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: "x", MaxRetries: 1})
	var got []DownloadActivity
	d.OnActivity = func(a DownloadActivity) { got = append(got, a) }

	// The context deadline cuts the 5s retry backoff short — the emission
	// under test happens before the sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := d.fetchSegmentWithRetry(ctx, srv.URL+"/seg", nil)

	if !errors.Is(err, ErrSegmentRetriesExhausted) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fetchSegmentWithRetry err = %v, want retries-exhausted or deadline", err)
	}
	found := false
	for _, a := range got {
		if a == ActivityRetrying {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("OnActivity emissions = %v, want to include ActivityRetrying", got)
	}
}
