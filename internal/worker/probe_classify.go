package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
)

// probeErrClass categorizes a probe/fetch error so the upcoming-stream wait
// loops can treat a connectivity blip differently from a definitive service
// refusal.
type probeErrClass int

const (
	classNetwork   probeErrClass = iota // transient/connectivity — DO NOT count
	classServer                         // definitive service verdict — count
	classCancelled                      // ctx cancelled — abandon
)

// classifyProbeErr categorizes a probe error. ASYMMETRIC DEFAULT: an
// unrecognized error is treated as classNetwork (do-not-count). Under the
// cost model (a false "give up" wrongly errors a waiting stream; a false
// "keep waiting" only delays giving up), a missed classification must only
// ever delay giving up.
//
// Verified against internal/youtube/player_api_strategy.go: transport errors
// reach us raw (lastErr=err, :534) or %w-wrapped ("read body"/"parse response",
// :543/:564) so errors.As matches the inner net error; only HTTP status
// failures are flattened to the string "<client> API error: HTTP <code>"
// (:552/:556), handled by the string fallback below.
func classifyProbeErr(err error) probeErrClass {
	if err == nil {
		return classServer
	}
	if errors.Is(err, context.Canceled) {
		return classCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return classNetwork
	}

	// url.Error wraps the transport error; recurse on the inner error.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return classifyProbeErr(urlErr.Err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return classNetwork
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return classNetwork
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return classNetwork
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return classNetwork
	}

	// String fallback for lossy wraps where the transport detail was flattened.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "http 429"), // rate limited — transient
		strings.Contains(msg, "http 5"), // 5xx — server transient
		strings.Contains(msg, "tls"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "eof"):
		return classNetwork
	case strings.Contains(msg, "http 4"): // 4xx (non-429) — definitive client error
		return classServer
	}
	return classNetwork // asymmetric default
}

// probeReport tells the wait loop whether/how to feed the passive connectivity
// tracker after a probe error.
type probeReport int

const (
	reportNone    probeReport = iota
	reportFailure             // ReportFailure: the network looks down
	reportSuccess             // ReportSuccess: the request reached the service
)

// probeDecision is the pure decision the wait loops act on for a probe error.
type probeDecision struct {
	count     bool        // increment consecutiveErrors (only definitive failures)
	cancelled bool        // ctx cancelled → return cancelled
	report    probeReport // how to feed the passive tracker
}

// probeErrorDecision maps a probe error to the loop's reaction.
func probeErrorDecision(err error) probeDecision {
	switch classifyProbeErr(err) {
	case classCancelled:
		return probeDecision{cancelled: true, report: reportNone}
	case classNetwork:
		return probeDecision{count: false, report: reportFailure}
	default: // classServer
		return probeDecision{count: true, report: reportSuccess}
	}
}
