package youtube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// The status probe's refusals that say nothing.
//
// Service.ProbeVideoStatus is one cookieless ANDROID_VR player call. It feeds
// both monitors (the feed walk and DECAPI), the signed-out waiting room, the
// live strategies' CheckStreamStatus and the quality monitor's first hop. When
// ANDROID_VR refuses — YouTube's "Sign in to confirm you're not a bot" wall on
// the probe's IP, or a "made for kids" video, which yt-dlp marks unavailable
// with this client (_base.py:226, and visionos at :295) — the refusal often
// carries no videoDetails at all, and classifyStream, with nothing to go on,
// files it not_a_stream. Everything downstream took that as a verdict: the
// feed walk stored it as a settled not_a_stream it never probes again, DECAPI
// memoised it as terminal, a live-only channel skipped it as non-stream
// content, the waiting room's default arm ended the job "unexpected status:
// not_a_stream", and a live capture's stall check read it as the broadcast
// over. Only that last site guarded against the shape, and only for an
// auth-walled verdict.
//
// yt-dlp never meets this as a verdict. Its live_status is decided by
// get_first over the videoDetails of every player response it holds
// (_list_formats, _video.py:3861-3874 at b77c9304e), and a client that is
// refused simply contributes none: the other default clients (tv_samsung,
// visionos, web — _video.py:143-147; android_vr is in no default tuple)
// classify the video. A response that states none of the flags is
// live_status None, "unknown" — not "not live". And for the made-for-kids
// case it appends web_embedded behind a refused android_vr answer
// (_video.py:3145-3153), with the embed page's encryptedHostFlags on the call
// (f2bd3202c, #16177; _video.py:2948-2954). That fallback's b77c9304e state
// took five commits: web_embedded behind android_vr in bb1c05752 (#15785),
// visionos beside android_vr in 1328586f7 (#17184), ERROR beside UNPLAYABLE in
// 69ea20006 (#17261) — which also swapped web_embedded for tv_downgraded —
// web_embedded back ahead of tv_downgraded in 5d5b634d8 (#17462), and
// tv_downgraded dropped in 897341241 (#17805), leaving web_embedded alone.
//
// Moombox's probe is one client, so the port is the two halves of that
// (owner decision D6, 2026-10-31): such an answer is INCONCLUSIVE, never a
// classification — a members-only refusal aside, a verdict about who may
// watch the video that the monitors and the waiting room act on
// (statusAnswerInconclusive) — and it is escalated ONCE before the probe
// gives up: to the authenticated TV probe when the jar is signed in,
// otherwise to a public web_embedded call carrying the embed page's
// encryptedHostFlags. An escalation that answers with broadcast details is
// returned in the probe's place, tagged with the escalation that gave it
// (VideoInfo.StatusProbeEscalation); one that fails, or refuses the same way,
// leaves the probe inconclusive (*StatusProbeInconclusiveError), which the
// monitors count as an errored probe — nothing stored, probed again next
// cycle for as long as the refusal stands, since the failure tracker's
// give-up only logs unless probe_cooldown is set — and the waiting loop as a
// network-class error (keep waiting, never counted toward its give-up).
//
// Deliberately beyond upstream in one respect: upstream asks web_embedded
// only when the watch page says "made for kids" and a JS runtime is there to
// solve the formats' signatures. The probe fetches no watch page, and what it
// asks — the broadcast's state — is read off videoDetails, which needs no
// player and no solved format, so it escalates on the shape of the refusal
// itself, whatever the refusal says.

// The status probe's two escalations, as StatusProbeInconclusiveError.Escalation
// and VideoInfo.StatusProbeEscalation name them.
const (
	// StatusProbeEscalationTVAuth is the signed-in escalation: the
	// authenticated TV probe, ProbeVideoStatusAuthenticated.
	StatusProbeEscalationTVAuth = "tv_auth"
	// StatusProbeEscalationWebEmbedded is the signed-out one: a probe-only
	// web_embedded call (probeVideoStatusEmbedded).
	StatusProbeEscalationWebEmbedded = "web_embedded"
)

// StatusProbeInconclusiveError is what Service.ProbeVideoStatus returns when
// ANDROID_VR refused with no broadcast details (statusAnswerInconclusive) and
// the one escalation behind it settled nothing either. It is not a statement
// about the video: a consumer must not store, end or skip anything on it.
//
// It unwraps to the escalation's own failure, so a cancelled context still
// reads as one (errors.Is(err, context.Canceled)) and a substituted video as
// a *VideoIDMismatchError.
type StatusProbeInconclusiveError struct {
	// Playability and Reason are ANDROID_VR's refusal.
	Playability PlayabilityError
	Reason      string
	// Escalation names the one client asked next:
	// StatusProbeEscalationTVAuth when the jar is signed in,
	// StatusProbeEscalationWebEmbedded when it is not. "" when none was asked,
	// because the caller's context had ended first.
	Escalation string
	// EscalationErr is why the escalation settled nothing: the call's own
	// failure, or a refusal of the same shape (errRefusedWithoutDetails).
	EscalationErr error
}

func (e *StatusProbeInconclusiveError) Error() string {
	msg := "status probe inconclusive: android_vr " + refusalText(e.Playability, e.Reason)
	switch {
	case e.Escalation != "":
		msg += "; " + e.Escalation + " escalation: " + errText(e.EscalationErr)
	case e.EscalationErr != nil:
		msg += "; not escalated: " + e.EscalationErr.Error()
	}
	return msg
}

func (e *StatusProbeInconclusiveError) Unwrap() error { return e.EscalationErr }

// refusalText is one refused answer as the error text names it.
func refusalText(playability PlayabilityError, reason string) string {
	if reason == "" {
		return fmt.Sprintf("refused with no broadcast details (%s)", playability)
	}
	return fmt.Sprintf("refused with no broadcast details (%s: %q)", playability, reason)
}

// errRefusedWithoutDetails is the escalation's own answer when it refused the
// way ANDROID_VR did.
func errRefusedWithoutDetails(info *VideoInfo) error {
	return fmt.Errorf("%s too", refusalText(info.PlayabilityError, info.PlayabilityReason))
}

// statusLiveFlagsStated reports whether a player response states anything
// yt-dlp's live_status is decided on (_list_formats, _video.py:3861-3874):
// post_live when isPostLiveDvr is true, is_live when isLive is (or, when no
// response states isLive, liveBroadcastDetails.isLiveNow), is_upcoming when
// isUpcoming is, was_live when isLiveContent is, and not_live when isLive or
// isLiveContent is stated false. A response that states none of them is
// live_status None: it says nothing about the broadcast. The microformat
// arm never fires on ANDROID_VR, which sends no microformat, and is kept so
// the rule is upstream's whichever client answered.
func statusLiveFlagsStated(videoDetails, microformat map[string]any) bool {
	for _, key := range []string{"isLive", "isLiveContent"} {
		if _, ok := videoDetails[key].(bool); ok {
			return true
		}
	}
	for _, key := range []string{"isUpcoming", "isPostLiveDvr"} {
		if v, _ := videoDetails[key].(bool); v {
			return true
		}
	}
	lbd, _ := microformat["liveBroadcastDetails"].(map[string]any)
	_, ok := lbd["isLiveNow"].(bool)
	return ok
}

// statusAnswerInconclusive reports whether a status probe's answer is the
// refused shape that says nothing: a refusal (any verdict but OK), classified
// not_a_stream, from a response that stated none of live_status's flags
// (statusLiveFlagsStated). not_a_stream is the one classification
// classifyStream reaches by absence — every other arm reads something the
// response said (the flags, an upcoming playability, the liveStreamability
// renderer, broadcast details) — so a refused answer read upcoming off the
// renderer, or not_a_stream off an explicit isLiveContent false, stays the
// answer it is.
//
// A members-only refusal is never inconclusive, details or none. It says
// nothing about the broadcast, but it is a verdict about the video — who may
// watch it — and every consumer acts on it as one: the feed walk relabels the
// row 'membership' on it and retries with the authenticated probe in the same
// cycle (the refusal IS the sighting, spec §9; walk.go's probeRow), which is
// what routes the row to that probe on every later cycle and lets the
// membership cookie gate park it when cookies lapse; the waiting room
// switches to the cookied probe on it (B1, waitingRoomAuthFlip). Escalated, a
// member's TV answer would replace it and none of that would happen;
// inconclusive, the walk would see an errored probe and nothing to relabel,
// and a signed-out waiting room would wait for good on a stream it can never
// record. The one consumer it says nothing to — the live stall check, which
// asks whether the broadcast is still on air — defers on it itself
// (internal/worker's observeYouTubeStatusProbe), as it did before G8. Not a
// shape yt-dlp's made-for-kids fallback reads either: that keys on
// UNPLAYABLE or ERROR on a page saying "made for kids" (_video.py:3145-3153).
func statusAnswerInconclusive(info *VideoInfo) bool {
	return info != nil && info.StreamStatus == StreamNotAStream &&
		info.PlayabilityError != PlayabilityOK && info.PlayabilityError != PlayabilityMembersOnly &&
		!info.liveFlagsStated
}

// escalateStatusProbe is the one escalation behind a status probe whose
// ANDROID_VR answer was inconclusive (refused is that answer): the
// authenticated TV probe when the jar is signed in, a public web_embedded
// call otherwise. It returns the escalation's answer when that answer says
// something about the broadcast, and a *StatusProbeInconclusiveError
// otherwise — never refused itself, which would hand the caller the guess this
// exists to keep from it.
//
// The jar is synced first, as GetVideoInfo syncs it before choosing its
// cascade: which escalation runs is a question about the credentials, and a
// cookies.txt replaced by hand must reach it. The authenticated probe syncs
// again itself; an unchanged file costs a stat.
func (s *Service) escalateStatusProbe(ctx context.Context, videoID, visitorData string, refused *VideoInfo) (*VideoInfo, error) {
	inconclusive := &StatusProbeInconclusiveError{Playability: refused.PlayabilityError, Reason: refused.PlayabilityReason}
	if err := ctx.Err(); err != nil {
		inconclusive.EscalationErr = err
		return nil, inconclusive
	}
	if err := s.Auth.SyncCookies(); err != nil {
		s.logger.Warn("[YouTube] SyncCookies failed before the status probe's escalation", "error", err)
	}
	var info *VideoInfo
	var err error
	if s.Auth.HasAuthCookies() {
		inconclusive.Escalation = StatusProbeEscalationTVAuth
		info, err = s.ProbeVideoStatusAuthenticated(ctx, videoID)
	} else {
		inconclusive.Escalation = StatusProbeEscalationWebEmbedded
		info, err = s.PlayerAPI.probeVideoStatusEmbedded(ctx, videoID, visitorData)
	}
	if inconclusive.EscalationErr = escalationSettledNothing(info, err); inconclusive.EscalationErr != nil {
		s.logger.Debug("[YouTube] Status probe inconclusive", slog.String("videoID", videoID), slog.String("error", inconclusive.Error()))
		return nil, inconclusive
	}
	s.logger.Debug("[YouTube] Status probe refused with no broadcast details; the escalation answered",
		slog.String("videoID", videoID), slog.String("escalation", inconclusive.Escalation),
		slog.String("refusal", string(refused.PlayabilityError)), slog.String("status", string(info.StreamStatus)))
	info.StatusProbeEscalation = inconclusive.Escalation
	return info, nil
}

// escalationSettledNothing is why the escalation's call settled nothing —
// its own failure, no answer at all, or a refusal as blind as ANDROID_VR's —
// or nil when its answer says something about the broadcast and stands in
// the probe's place.
func escalationSettledNothing(info *VideoInfo, err error) error {
	switch {
	case err != nil:
		return err
	case info == nil:
		return errNilEscalationAnswer
	case statusAnswerInconclusive(info):
		return errRefusedWithoutDetails(info)
	}
	return nil
}

// errRateLimitedSession is why a rate-limit refusal is not escalated
// (Service.ProbeVideoStatus): the limit is the session's, and another
// client would only be another call into it.
var errRateLimitedSession = errors.New("the session is rate-limited; another client would only be another call into the limit")

// errNilEscalationAnswer is the escalation's answer when its call returned
// neither an answer nor an error — the (nil, nil) a shutdown race can leave.
var errNilEscalationAnswer = errors.New("no answer and no error")

// probeVideoStatusEmbedded is the signed-out escalation: one WEB_EMBEDDED
// player call through fetchWithEmbedded, with the embed page fetched for its
// encryptedHostFlags as upstream sends them on every web_embedded call
// (f2bd3202c), marked probe-only — so, as on every probe, no PLAYER PO token
// is minted (owner decision O-R), and no player is adopted for a signature
// timestamp, which would cost a player fetch on every escalation. The answer
// is asked for the broadcast's state, read off videoDetails; its formats are
// still parsed — ciphered ones kept for a later solve, as on every call — and
// they are another client's, under that client's rules (no PO token, and
// whatever SABR does to web clients), which is why the answer comes back
// tagged (VideoInfo.StatusProbeEscalation): the stall check does not read
// the interruption signature off it, though the quality probe's first hop
// may still select from it. The visitor data and session are the probes' own
// (probeYtcfg).
func (p *PlayerAPI) probeVideoStatusEmbedded(ctx context.Context, videoID, visitorData string) (*VideoInfo, error) {
	return p.fetchWithEmbedded(withProbeOnlyCall(ctx), videoID, p.probeYtcfg(visitorData), 0, true)
}
