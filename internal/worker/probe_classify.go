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

	// net.Error is an interface; *net.OpError and *net.DNSError both implement
	// it, so this single errors.As covers dial / timeout / DNS / connection
	// failures without needing separate concrete-type checks.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classNetwork
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return classNetwork
	}

	// String fallback for lossy wraps where the transport detail was flattened.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "http 429"), // rate limited — transient
		strings.Contains(msg, "http 401"), // auth expired — cookie refresh can remediate; keep waiting
		strings.Contains(msg, "http 403"), // forbidden — often transient (bot / rate-limit); keep waiting
		strings.Contains(msg, "http 5"), // 5xx — server transient
		strings.Contains(msg, "tls"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "eof"):
		return classNetwork
	case strings.Contains(msg, "http 4"): // other 4xx (404/410/...) — definitive, terminal
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

// applyProbeError folds a probe error into the wait loop's running error count.
// It returns the new count, whether the loop should give up (definitive
// failures reached maxConsecutiveProbeErrors), how to feed the passive tracker,
// and whether the context was cancelled.
//
// Network-class failures NEVER advance the count — they are transient, so the
// loop keeps waiting through an outage; only definitive (server-class) failures
// count toward give-up. Centralising this keeps the YouTube and Twitch wait
// loops identical and makes the give-up threshold unit-testable without driving
// the full polling loop (timers + live API).
func applyProbeError(err error, consecutiveErrors int) (count int, giveUp bool, report probeReport, cancelled bool) {
	switch classifyProbeErr(err) {
	case classCancelled:
		return consecutiveErrors, false, reportNone, true
	case classNetwork:
		return consecutiveErrors, false, reportFailure, false
	default: // classServer
		count = consecutiveErrors + 1
		return count, count >= maxConsecutiveProbeErrors, reportSuccess, false
	}
}
