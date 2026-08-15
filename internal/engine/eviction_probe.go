package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// probeSegmentMaxBodyBytes caps the body ProbeSegmentAvailable reads for
// diagnostic inspection. A CMAF segment's box headers (ftyp/moov/sidx) all
// live in the first few KB; 4 MB is enormous headroom for InspectSegment's
// boundary-segment diagnosis while still bounding memory against a
// misbehaving response.
const probeSegmentMaxBodyBytes = 4 << 20

// evictionProbeMaxAttempts and evictionProbeRetryDelay bound how hard
// FindOldestAvailableSeq retries a single bisection point before giving up.
// A transient blip (one dropped connection, one slow edge) must not abort
// an entire eviction diagnosis; three attempts 250ms apart absorbs that
// without turning a persistently dead URL into a long stall per step.
const (
	evictionProbeMaxAttempts = 3
	evictionProbeRetryDelay  = 250 * time.Millisecond
)

// FindOldestAvailableSeq bisects [0, head] for the first sequence the CDN
// still serves. probe returns (available, error); a transient per-point
// error is retried up to evictionProbeMaxAttempts times, evictionProbeRetryDelay
// apart, before aborting the whole search with that error.
//
// head itself is checked first: if it's unavailable, the URL is dead (wrong
// itag, revoked token, expired grant) rather than evicted — segments don't
// disappear from the LIVE end of a stream, so an unavailable head is a URL
// problem, not an eviction signal, and the search returns (-1, err) rather
// than mis-reporting a bisection boundary. Segment 0 is checked next: if
// it's still available, nothing has been evicted and the search returns 0
// immediately without a full bisection.
func FindOldestAvailableSeq(ctx context.Context, head int, probe func(ctx context.Context, seq int) (bool, error)) (int, error) {
	headAvail, err := probeWithRetry(ctx, probe, head)
	if err != nil {
		return -1, err
	}
	if !headAvail {
		return -1, fmt.Errorf("head segment unavailable — URL problem, not eviction")
	}

	zeroAvail, err := probeWithRetry(ctx, probe, 0)
	if err != nil {
		return -1, err
	}
	if zeroAvail {
		return 0, nil
	}

	// Standard lower-bound bisection: probe(lo) is known unavailable,
	// probe(hi) is known available; converge until they're adjacent. hi
	// is the first available sequence.
	lo, hi := 0, head
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		avail, err := probeWithRetry(ctx, probe, mid)
		if err != nil {
			return -1, err
		}
		if avail {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, nil
}

// probeWithRetry calls probe at seq, retrying up to evictionProbeMaxAttempts
// times on a transport error (evictionProbeRetryDelay apart) before giving
// up and returning that error. A definitive result — including
// (unavailable, nil) — is not an error and returns immediately: 403/410/404
// is exactly the signal the bisection searches for, not a failure to search.
func probeWithRetry(ctx context.Context, probe func(ctx context.Context, seq int) (bool, error), seq int) (bool, error) {
	var lastErr error
	for attempt := 0; attempt < evictionProbeMaxAttempts; attempt++ {
		avail, err := probe(ctx, seq)
		if err == nil {
			return avail, nil
		}
		lastErr = err
		if attempt < evictionProbeMaxAttempts-1 {
			if sleepErr := utils.Sleep(ctx, evictionProbeRetryDelay); sleepErr != nil {
				return false, sleepErr
			}
		}
	}
	return false, lastErr
}

// ProbeSegmentAvailable GETs one segment (PO token + standard headers
// applied, same request construction as fetchSegment) purely to answer
// "does the CDN still serve this sequence" for FindOldestAvailableSeq's
// bisection. Unlike fetchSegmentWithRetry it does not retry and does not
// distinguish 403 from 410 from any other 4xx/5xx — availability is
// reported by status code alone (< 400 = available). The body — capped at
// probeSegmentMaxBodyBytes — is returned only when available, for the
// caller's boundary-segment InspectSegment call; an unavailable response
// returns a nil body.
//
// This deliberately does not touch other downloader state: it runs as a
// bisection probe outside the ordinary fetch loop, on a downloader whose
// normal download has already finished (successfully or not). The one
// exception is noteHeadSeqFromResponse, which is harmless and correct here
// too — GVS attaches X-Head-Seqnum to every response, success or error.
func (d *SegmentDownloader) ProbeSegmentAvailable(ctx context.Context, seq int) (bool, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	segURL := applyPoTokenQuery(d.buildSegmentURL(seq), d.getPoToken())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return false, nil, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		reportFailure("engine/fetch")
		return false, nil, err
	}
	reportSuccess("engine/fetch")
	defer resp.Body.Close()

	d.noteHeadSeqFromResponse(resp)

	body, err := io.ReadAll(io.LimitReader(resp.Body, probeSegmentMaxBodyBytes))
	if err != nil {
		return false, nil, err
	}
	if resp.StatusCode >= 400 {
		return false, nil, nil
	}
	return true, body, nil
}
