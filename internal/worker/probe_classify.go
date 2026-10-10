package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/youtube"
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
//
// A youtube.VideoIDMismatchError (and ErrAllClientsMismatched) falls to the
// asymmetric default, classNetwork: the waiting-room loop keeps waiting rather
// than counting toward its give-up budget. That is the RIGHT answer here —
// while YouTube is serving substitutes we know nothing about the real video,
// and the cost model says a missed classification must only ever delay giving
// up, never wrongly error a waiting stream.
//
// This class decides the give-up counter ONLY. What the connectivity oracle is
// told is probeVote's separate verdict: classNetwork means "do not count", and
// most of what it covers — a 429, a 403 bot wall, a 5xx, a substitution — is
// an answer from the service, which proves the network is up.
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
	// it, so this single errors.AsType covers dial / timeout / DNS / connection
	// failures without needing separate concrete-type checks.
	if _, ok := errors.AsType[net.Error](err); ok {
		return classNetwork
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return classNetwork
	}

	// String fallback for lossy wraps where the transport detail was flattened.
	msg := strings.ToLower(err.Error())

	// Only the "<client> API error: HTTP <code>" head is a classification
	// signal. Since YOUTUBE-7 a non-200 Innertube failure appends YouTube's
	// own error.status/message after " — ", and that text is not ours: a
	// terminal 404 whose message reads "Backend timeout …" would match the
	// transient arm below — which is tested FIRST — and beat the status code
	// this fallback exists to read. The consequences are not symmetric with
	// the asymmetric default: the give-up counter would never advance on a
	// deleted video, and reportProbeResult would cast a false "the network is
	// down" vote into the passive connectivity oracle.
	if i := strings.Index(msg, " — "); i >= 0 {
		msg = msg[:i]
	}
	switch {
	case strings.Contains(msg, "http 429"), // rate limited — transient
		strings.Contains(msg, "http 401"), // auth expired — cookie refresh can remediate; keep waiting
		strings.Contains(msg, "http 403"), // forbidden — often transient (bot / rate-limit); keep waiting
		strings.Contains(msg, "http 5"),   // 5xx — server transient
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

// httpAnswerRe matches the status a platform client flattens an HTTP answer
// into: YouTube's "<client> API error: HTTP 429", Twitch's "gql http 503".
var httpAnswerRe = regexp.MustCompile(`\bhttp [1-5][0-9][0-9]\b`)

// probeVote is what a failed probe or fetch tells the connectivity oracle,
// kept apart from classifyProbeErr's give-up class. The two used to be one
// switch, so everything "transient — do not count" was also reported as "the
// network is down": a 429 rate limit, a 403 bot wall, a 5xx, an expired
// Twitch token, a YouTube substitution. Each of those is an ANSWER from the
// service. Enough waiting rooms answered that way, plus one unrelated
// transport blip, met the passive threshold and took the whole process
// offline — every Twitch live capture cancelled, every monitor paused, an
// Outage Alert sent — with the internet fully reachable.
//
// So only a failure to reach the service at all votes "down": a transport
// error (net.Error in the chain, a request deadline with no answer, a
// connection cut mid-response), or one of their tokens in a wrap that
// flattened the type away. An HTTP status in the error's head, a GQL-level
// error and a substitution vote "up", since the request reached the service.
// Anything else casts no vote: an error that cannot be placed is not evidence
// either way.
func probeVote(err error) probeReport {
	if err == nil {
		return reportSuccess
	}
	if errors.Is(err, context.Canceled) {
		return reportNone
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return reportFailure
	}
	// url.Error is itself a net.Error, so look through it: what it wraps is
	// the verdict.
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr.Err != nil {
		return probeVote(urlErr.Err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return reportFailure
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return reportFailure
	}
	// YouTube answered, with a player response for another video.
	if _, ok := errors.AsType[*youtube.VideoIDMismatchError](err); ok {
		return reportSuccess
	}
	if errors.Is(err, youtube.ErrAllClientsMismatched) {
		return reportSuccess
	}

	// String fallback, over the same head classifyProbeErr reads: the text
	// after " — " is YouTube's own message, and may well say "timeout".
	msg := strings.ToLower(err.Error())
	if i := strings.Index(msg, " — "); i >= 0 {
		msg = msg[:i]
	}
	switch {
	case httpAnswerRe.MatchString(msg),
		strings.Contains(msg, "gql rate limited ("), // 429 (internal/twitch/api.go)
		strings.Contains(msg, "gql auth failure ("), // 401/403
		strings.Contains(msg, "gql error ("),        // an HTTP 200 carrying GQL errors
		strings.Contains(msg, "gql batch error ("):
		return reportSuccess
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "tls"),
		strings.Contains(msg, "eof"):
		return reportFailure
	}
	return reportNone
}

// reportFetchOutcome reports a confirmatory/secondary full-fetch error's
// reachability signal to the passive oracle (probeVote: a transport failure
// → failure; an answer from the service → success) WITHOUT touching the wait
// loop's give-up counter, which the lightweight probe owns. A cancelled
// context, or an error probeVote cannot place, is not reported.
func reportFetchOutcome(err error, tag string) {
	switch probeVote(err) {
	case reportFailure:
		reportProbeResult(tag, true)
	case reportSuccess:
		reportProbeResult(tag, false)
	}
}

// applyProbeError folds a probe error into the wait loop's running error count.
// It returns the new count, whether the loop should give up (definitive
// failures reached maxConsecutiveProbeErrors), how to feed the passive tracker
// (probeVote — not the class: a transient 429 still reached the service), and
// whether the context was cancelled.
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
		return consecutiveErrors, false, probeVote(err), false
	default: // classServer
		count = consecutiveErrors + 1
		return count, count >= maxConsecutiveProbeErrors, probeVote(err), false
	}
}
