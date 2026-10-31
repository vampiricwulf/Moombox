package youtube

import "testing"

// The refusal shapes below are built from the paths yt-dlp reads them at
// (_video.py:4048-4081 at b77c9304e: errorScreen.playerErrorMessageRenderer's
// reason and subreason, the interstitialViewModel's title and description
// from a51ef56c0, errorScreen.playerCaptchaViewModel from f484c5159, and the
// "This content isn't available, try again later" reason from 26feac3dd).
// No captured response carries them yet: the status code that comes with the
// rate-limit reason, and the captcha's whole shape, are live-only facts, so
// every row that pins a verdict sets a status the code must not depend on.

// TestParsePlayabilityStatusRecognisesTransientRefusals is G7: a rate-limit
// reason and a captcha challenge read rate_limited, the verdict the worker
// waits out, where they read unknown (or, behind a "Video unavailable",
// unavailable) and ended the job.
//
// Mutants: drop the captcha arm — the captcha rows read unavailable/unknown;
// drop the "try again later" arm — the rate-limit rows read unknown; move the
// transient arms below the status switch — the "Video unavailable" row reads
// unavailable; drop the "sign in" exclusion — the bot-check rows read
// rate_limited; drop the OK exclusion — the OK row reads rate_limited; read
// only messages[0] into the match — the second-message row reads unknown;
// treat an empty captcha object as one — the empty-object row reads
// rate_limited.
func TestParsePlayabilityStatusRecognisesTransientRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		want       PlayabilityError
		wantReason string
	}{
		{"the rate limit, ERROR", `{"status": "ERROR", "reason": "This content isn't available, try again later."}`,
			PlayabilityRateLimited, "This content isn't available, try again later."},
		{"the rate limit, typographic apostrophe", `{"status": "UNPLAYABLE", "reason": "This content isn’t available, try again later."}`,
			PlayabilityRateLimited, "This content isn’t available, try again later."},
		{"the rate limit in a later message", `{"status": "UNPLAYABLE", "messages": ["Something went wrong", "Please try again later"]}`,
			PlayabilityRateLimited, "Something went wrong"},
		{"the rate limit behind a Video unavailable reason",
			`{"status": "ERROR", "reason": "Video unavailable", "errorScreen": {"playerErrorMessageRenderer": {
				"reason": {"simpleText": "Video unavailable"},
				"subreason": {"runs": [{"text": "This content isn't available, try again later"}]}}}}`,
			PlayabilityRateLimited, "Video unavailable. This content isn't available, try again later"},
		{"a captcha challenge",
			`{"status": "UNPLAYABLE", "reason": "Video unavailable", "errorScreen": {"playerCaptchaViewModel": {"captchaId": "x"}}}`,
			PlayabilityRateLimited, "Video unavailable. " + CaptchaChallengeNote},
		{"a captcha challenge with no reason", `{"status": "UNPLAYABLE", "errorScreen": {"playerCaptchaViewModel": {"captchaId": "x"}}}`,
			PlayabilityRateLimited, CaptchaChallengeNote},
		{"an empty captcha object is not one",
			`{"status": "UNPLAYABLE", "reason": "Video unavailable", "errorScreen": {"playerCaptchaViewModel": {}}}`,
			PlayabilityUnavailable, "Video unavailable"},
		{"the bot check is a sign-in, not a captcha",
			`{"status": "LOGIN_REQUIRED", "errorScreen": {
				"playerErrorMessageRenderer": {"reason": {"simpleText": "Sign in to confirm you’re not a bot"},
					"subreason": {"simpleText": "This helps protect our community. Learn more"}},
				"playerCaptchaViewModel": {"captchaId": "x"}}}`,
			PlayabilityLoginRequired, "Sign in to confirm you’re not a bot"},
		{"a sign-in reason that says try again later is a sign-in",
			`{"status": "LOGIN_REQUIRED", "reason": "Sign in to continue, or try again later"}`,
			PlayabilityLoginRequired, "Sign in to continue, or try again later"},
		{"a playable response is not refused", `{"status": "OK", "errorScreen": {"playerCaptchaViewModel": {"captchaId": "x"}}}`,
			PlayabilityOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := parsePlayabilityStatus(decodePlayerJSON(t, tc.status))
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestParsePlayabilityStatusReadsTheErrorScreen is G47 and the ledger's
// a51ef56c0: the error screen's reason comes ahead of the status's own, its
// subreason is appended, a "your country" subreason is a region block
// whatever the status, and an interstitial-only refusal is read through its
// title and description.
//
// Mutants: drop the geo arm — the geo row reads unavailable; read the
// status's reason ahead of the renderer's — the removed-video row quotes
// "Video unavailable" alone; drop the "subreason != reason" test — the
// duplicate row repeats itself; drop the interstitial fallback — the
// interstitial row reads unknown with an empty reason; drop cleanHTMLText —
// the interstitial row keeps its markup and entity.
func TestParsePlayabilityStatusReadsTheErrorScreen(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		want       PlayabilityError
		wantReason string
	}{
		{"a geo block whose status says ERROR",
			`{"status": "ERROR", "reason": "Video unavailable", "errorScreen": {"playerErrorMessageRenderer": {
				"reason": {"simpleText": "Video unavailable"},
				"subreason": {"runs": [{"text": "The uploader has not made this video "}, {"text": "available in your country"}]}}}}`,
			PlayabilityRegionBlocked, "Video unavailable. The uploader has not made this video available in your country"},
		{"the renderer's reason ahead of the status's",
			`{"status": "ERROR", "reason": "Video unavailable", "errorScreen": {"playerErrorMessageRenderer": {
				"reason": {"simpleText": "This video has been removed by the uploader"}}}}`,
			PlayabilityUnavailable, "This video has been removed by the uploader"},
		{"a subreason that repeats the reason is not appended",
			`{"status": "UNPLAYABLE", "errorScreen": {"playerErrorMessageRenderer": {
				"reason": {"simpleText": "Video unavailable"}, "subreason": {"simpleText": "Video unavailable"}}}}`,
			PlayabilityUnavailable, "Video unavailable"},
		{"an interstitial-only refusal",
			`{"status": "UNPLAYABLE", "errorScreen": {"playerInterstitialRenderer": {"content": {"interstitialViewModel": {
				"title": {"content": "This video is private"},
				"description": {"content": "<b>Only</b> people the owner shares it with   can see it &amp; play it"}}}}}}`,
			PlayabilityPrivate, "This video is private. Only people the owner shares it with can see it & play it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := parsePlayabilityStatus(decodePlayerJSON(t, tc.status))
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestPlayabilityMatchKeepsTheTextTheRendererDisplaced: the renderer's
// reason replaces the status's own in what the job quotes, never in what the
// keyword rules read. A waiting room whose "live event will begin" sits in
// the status's `reason` stays upcoming — to the verdict and to the stream
// classification alike — under a renderer reason that says something else.
// The renderer text here is synthetic: no capture shows a waiting room with
// one, and this pins that a future one cannot turn a waiting room into an
// error.
//
// Mutant: build match from the display reason alone — the verdict reads
// unknown and the classification not_a_stream.
func TestPlayabilityMatchKeepsTheTextTheRendererDisplaced(t *testing.T) {
	ps := decodePlayerJSON(t, `{"status": "UNPLAYABLE", "reason": "This live event will begin in a few moments.",
		"errorScreen": {"playerErrorMessageRenderer": {"reason": {"simpleText": "Waiting for the host"}}}}`)
	if got, _ := parsePlayabilityStatus(ps); got != PlayabilityOK {
		t.Errorf("verdict = %q, want ok (a waiting room)", got)
	}
	if status, _, upcoming, _ := classifyStream(map[string]any{}, ps, nil, false); status != StreamUpcoming || !upcoming {
		t.Errorf("classifyStream = %q (upcoming %v), want upcoming", status, upcoming)
	}
}
