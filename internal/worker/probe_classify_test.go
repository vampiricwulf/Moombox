package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestClassifyProbeErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want probeErrClass
	}{
		{"nil", nil, classServer},
		{"context canceled", context.Canceled, classCancelled},
		{"deadline exceeded", context.DeadlineExceeded, classNetwork},
		{"url wrapping opError", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}}, classNetwork},
		{"dns error", &net.DNSError{Err: "no such host", Name: "x"}, classNetwork},
		{"unexpected eof", io.ErrUnexpectedEOF, classNetwork},
		{"eof", io.EOF, classNetwork},
		{"opError direct", &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}, classNetwork},
		{"http 503 string", fmt.Errorf("WEB API error: HTTP 503"), classNetwork},
		{"http 429 string", fmt.Errorf("ANDROID_VR API error: HTTP 429"), classNetwork},
		{"tls string", fmt.Errorf("tls: handshake failure"), classNetwork},
		{"http 404 string", fmt.Errorf("WEB API error: HTTP 404"), classServer},
		{"http 410 string", fmt.Errorf("WEB API error: HTTP 410"), classServer},
		{"http 401 is transient", fmt.Errorf("WEB API error: HTTP 401"), classNetwork},
		{"http 403 is transient", fmt.Errorf("ANDROID_VR API error: HTTP 403"), classNetwork},
		{"unknown defaults to network", errors.New("something weird happened"), classNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeErr(tc.err); got != tc.want {
				t.Errorf("classifyProbeErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifierIgnoresTheAppendedInnertubeBody: since YOUTUBE-7 a non-200
// Innertube error appends YouTube's own `status: message` after " — ", and
// that text is not ours. The switch below matches "timeout" / "eof" / "tls" /
// "connection reset" BEFORE the "http 4" terminal arm, so a body token used to
// beat the status code the design promised to key on: a genuinely deleted
// video answering 404 with a deadline-shaped message classified NETWORK, the
// wait loop's give-up counter never advanced, and reportProbeResult cast a
// false "the network is down" vote into the passive connectivity oracle — which
// the asymmetric-default argument does NOT cover.
//
// Only the "<client> API error: HTTP <code>" head is a classification signal.
//
// Mutants this kills:
//   - the window not cut at " — "     → the four body-token rows go network
//   - the cut applied to a string that
//     has no delimiter (over-eager)   → the transport rows change class
//   - the cut taking the LAST " — "   → the two-delimiter row goes network
func TestClassifierIgnoresTheAppendedInnertubeBody(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want probeErrClass
	}{
		// The reviewer's executed flip cases: a terminal 4xx whose BODY
		// carries a transient-looking token.
		{"404 whose message says timeout", fmt.Errorf("Innertube API error: HTTP 404 — DEADLINE_EXCEEDED: Backend timeout while resolving the player response."), classServer},
		{"400 whose message says EOF", fmt.Errorf("Innertube API error: HTTP 400 — INVALID_ARGUMENT: unexpected EOF in request body"), classServer},
		{"410 whose message says tls", fmt.Errorf("Innertube API error: HTTP 410 — FAILED_PRECONDITION: tls handshake policy violation"), classServer},
		{"browse 404 whose message says connection reset", fmt.Errorf("browse API error: HTTP 404 — NOT_FOUND: connection reset by the upstream service"), classServer},
		// Two delimiters: only the FIRST bounds the head, so a transient token
		// between them is still body text and still ignored.
		{"a second delimiter cannot smuggle the body back in", fmt.Errorf("Innertube API error: HTTP 404 — NOT_FOUND: connection reset by peer — see the developer console"), classServer},

		// The transient codes stay transient with a body attached.
		{"403 with a benign message stays transient", fmt.Errorf("ANDROID_VR API error: HTTP 403 — FAILED_PRECONDITION: Precondition check failed."), classNetwork},
		{"503 with a benign message stays transient", fmt.Errorf("WEB API error: HTTP 503 — UNAVAILABLE: backend overloaded"), classNetwork},

		// A genuine transport error carries no delimiter, so nothing is cut.
		{"a flattened transport timeout is untouched", fmt.Errorf("dial tcp 142.250.0.1:443: i/o timeout"), classNetwork},
		{"a flattened TLS error is untouched", fmt.Errorf("tls: handshake failure"), classNetwork},

		// The full string the worker actually stores on the job row: the
		// stream processor's "full fetch failed: %w" wrap around the
		// all-clients-failed verdict around the client's own HTTP error.
		// Copied from a run: the stream processor's "full fetch failed: %w"
		// around the cascade's exhaustion verdict around the last client's
		// own HTTP error.
		{"the worker's own wrap still reaches the code", fmt.Errorf("full fetch failed: failed to extract any player response: ANDROID_VR API error: HTTP 404"), classServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeErr(tc.err); got != tc.want {
				t.Errorf("classifyProbeErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestApplyProbeError is the regression test for the reported bug: network-class
// probe failures must NEVER advance the give-up counter (so a waiting stream is
// never errored by an outage), while definitive server-class failures still
// count and eventually give up. This exercises the exact counting + threshold
// logic the YouTube and Twitch wait loops delegate to.
func TestApplyProbeError(t *testing.T) {
	// Network-class errors NEVER advance the count, no matter how many arrive.
	count := 0
	for i := 1; i <= maxConsecutiveProbeErrors*2; i++ {
		var giveUp, cancelled bool
		var report probeReport
		count, giveUp, report, cancelled = applyProbeError(&net.DNSError{Err: "no such host"}, count)
		if count != 0 || giveUp || cancelled || report != reportFailure {
			t.Fatalf("network error #%d: want count=0 giveUp=false cancelled=false report=failure; got count=%d giveUp=%v cancelled=%v report=%v",
				i, count, giveUp, cancelled, report)
		}
	}

	// Server-class errors advance the count and trigger give-up at the threshold.
	count = 0
	for i := 1; i <= maxConsecutiveProbeErrors; i++ {
		var giveUp bool
		var report probeReport
		count, giveUp, report, _ = applyProbeError(fmt.Errorf("WEB API error: HTTP 404"), count)
		if report != reportSuccess {
			t.Fatalf("server error #%d: want report=success, got %v", i, report)
		}
		if count != i {
			t.Fatalf("server error #%d: want count=%d, got %d", i, i, count)
		}
		if want := i >= maxConsecutiveProbeErrors; giveUp != want {
			t.Fatalf("server error #%d: want giveUp=%v, got %v", i, want, giveUp)
		}
	}

	// Cancellation is surfaced and does not advance the count.
	if c, giveUp, _, cancelled := applyProbeError(context.Canceled, 5); !cancelled || giveUp || c != 5 {
		t.Fatalf("cancelled: want cancelled=true giveUp=false count=5; got cancelled=%v giveUp=%v count=%d", cancelled, giveUp, c)
	}

	// An unknown error defaults to network (asymmetric default): even one below
	// the threshold, it must not count or give up.
	if c, giveUp, report, _ := applyProbeError(errors.New("mystery"), maxConsecutiveProbeErrors-1); c != maxConsecutiveProbeErrors-1 || giveUp || report != reportFailure {
		t.Fatalf("unknown: want count unchanged, giveUp=false, report=failure; got count=%d giveUp=%v report=%v", c, giveUp, report)
	}
}
