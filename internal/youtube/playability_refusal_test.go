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
	if status, _, upcoming, _ := classified(classifyStream(map[string]any{}, ps, nil, false)); status != StreamUpcoming || !upcoming {
		t.Errorf("classifyStream = %q (upcoming %v), want upcoming", status, upcoming)
	}
}

// The three answers YouTube gave anonymously for one private video
// (yZIXLfi8CZQ), verbatim: the player's on ANDROID_VR and WEB, and the watch
// page's. None states videoDetails. What a signed-in session is told has not
// been captured.
const (
	privateVideoAndroidVR = `{"status":"LOGIN_REQUIRED","reason":"Please sign in","messages":["This is a private video. Please sign in to verify that you may see it."]}`
	privateVideoWeb       = `{"status":"LOGIN_REQUIRED","messages":["This is a private video. Please sign in to verify that you may see it."],"errorScreen":{"playerErrorMessageRenderer":{"reason":{"simpleText":"Private video"},"subreason":{"simpleText":"Sign in if you've been granted access to this video"}}}}`
	privateVideoWatch     = `{"status":"LOGIN_REQUIRED","reason":"Private video","messages":["This is a private video. Please sign in to verify that you may see it."]}`
)

// TestParsePlayabilityStatusPlacesAPrivateLoginRequired: YouTube refuses a
// private video with LOGIN_REQUIRED, and the arm for that status looked only
// for a membership, so it read login_required. The waiting loop does not give
// up on that, and a stream made private while it waited was fetched every 32 s
// for 31 hours until the operator cancelled it. The word is matched across the
// whole text, since ANDROID_VR says it only in messages[]; the sign-in walls
// and the membership refusal keep their own verdicts.
//
// Mutants: drop the private arm (the three measured rows read login_required);
// read only the displayed reason (the ANDROID_VR row does); put it above the
// member check (the membership row that says private reads private).
// TestParsePlayabilityStatusPlacesWhatASignedInSessionIsTold: the refusals a
// healthy signed-in session was given on 2026-11-07, for a private YouTube Rewind
// video and for DrxyGDJCuc8 once its uploader had removed it. Every cookied client
// answered ERROR, so these read unavailable, or unknown for the one reason with
// neither keyword, and the cascade's answer for both is the watch page's
// unavailable, which ends a wait (isTerminalPlayability).
//
// Mutant: the ERROR arm not reading the reason for private.
func TestParsePlayabilityStatusPlacesWhatASignedInSessionIsTold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		want       PlayabilityError
		wantReason string
	}{
		{"private, tv and web_creator", `{"status":"ERROR","reason":"This video is private"}`, PlayabilityUnavailable, "This video is private"},
		{"private, web and web_embedded", `{"status":"ERROR","reason":"Video unavailable","errorScreen":{"playerErrorMessageRenderer":{"reason":{"simpleText":"Video unavailable"},"subreason":{"simpleText":"This video is private"}}}}`,
			PlayabilityUnavailable, "Video unavailable. This video is private"},
		{"removed, tv and web_creator", `{"status":"ERROR","reason":"This video has been removed by the uploader"}`, PlayabilityUnknown, "This video has been removed by the uploader"},
		{"removed, web and web_embedded", `{"status":"ERROR","reason":"Video unavailable","errorScreen":{"playerErrorMessageRenderer":{"reason":{"simpleText":"Video unavailable"},"subreason":{"simpleText":"This video has been removed by the uploader"}}}}`,
			PlayabilityUnavailable, "Video unavailable. This video has been removed by the uploader"},
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

func TestParsePlayabilityStatusPlacesAPrivateLoginRequired(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		want       PlayabilityError
		wantReason string
	}{
		{"android_vr's private video", privateVideoAndroidVR, PlayabilityPrivate, "Please sign in"},
		{"web's private video", privateVideoWeb, PlayabilityPrivate, "Private video. Sign in if you've been granted access to this video"},
		{"the watch page's private video", privateVideoWatch, PlayabilityPrivate, "Private video"},
		{"the bot wall", `{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you’re not a bot","messages":["Sign in to confirm you’re not a bot"]}`,
			PlayabilityLoginRequired, "Sign in to confirm you’re not a bot"},
		{"a plain sign-in", `{"status":"LOGIN_REQUIRED","reason":"Please sign in"}`, PlayabilityLoginRequired, "Please sign in"},
		{"a membership", `{"status":"LOGIN_REQUIRED","reason":"Join this channel to get access to members-only content"}`,
			PlayabilityMembersOnly, "Join this channel to get access to members-only content"},
		{"a membership that also says private", `{"status":"LOGIN_REQUIRED","reason":"Join this channel","messages":["This is a private video."]}`,
			PlayabilityMembersOnly, "Join this channel"},
		{"a word that only starts the same", `{"status":"LOGIN_REQUIRED","reason":"Please sign in to review our privacy policy"}`,
			PlayabilityLoginRequired, "Please sign in to review our privacy policy"},
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
