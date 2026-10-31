package youtube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The client IDs the status probe and its escalations go out as
// (X-YouTube-Client-Name).
const (
	clientAndroidVR   = "28"
	clientTV          = "7"
	clientWebEmbedded = "56"
)

// botWallBody is ANDROID_VR's "Sign in to confirm you're not a bot" wall as
// Moombox has seen it (internal/worker/interruption.go's former guard): a
// LOGIN_REQUIRED playability and no videoDetails at all.
const botWallBody = `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you’re not a bot",` +
	`"messages":["This helps protect our community. Learn more"]}}`

// upcomingEmbeddedBody is a waiting room as web_embedded answers it: the
// broadcast's own flags, so an answer.
const upcomingEmbeddedBody = `{"playabilityStatus":{"status":"LIVE_STREAM_OFFLINE","reason":"This live event will begin in a few moments."},` +
	`"videoDetails":{"videoId":"test1234567","title":"t","author":"a","isLiveContent":true,"isUpcoming":true}}`

// liveTVBody is a live broadcast as TV answers it.
const liveTVBody = `{"playabilityStatus":{"status":"OK"},` +
	`"videoDetails":{"videoId":"test1234567","title":"t","author":"a","isLive":true,"isLiveContent":true},` +
	`"streamingData":{"hlsManifestUrl":"https://example.com/hls.m3u8"}}`

// embedPageHTML is an embed page naming a player and the encryptedHostFlags
// upstream sends on every web_embedded call.
const embedPageHTML = `<script>ytcfg.set({"WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":` +
	`{"jsUrl":"\/s\/player\/e0e0e0e0\/player_ias.vflset\/en_US\/base.js","encryptedHostFlags":"ehf"}}});</script>`

// stubEmbedPage serves the embed page the signed-out escalation fetches and
// counts the fetches.
func stubEmbedPage(t *testing.T) *atomic.Int32 {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = io.WriteString(w, embedPageHTML)
	}))
	t.Cleanup(srv.Close)
	orig := constants.YouTubeURLs.Embed
	constants.YouTubeURLs.Embed = srv.URL
	t.Cleanup(func() { constants.YouTubeURLs.Embed = orig })
	return &fetches
}

// TestStatusProbeRefusalWithNoBroadcastDetailsIsInconclusive is G8 (owner
// decision D6): an ANDROID_VR refusal that states none of the flags
// yt-dlp's live_status is decided on is no classification. The probe asks
// one more client — signed out, web_embedded with the embed page's
// encryptedHostFlags — and when that refuses the same way it returns a
// *StatusProbeInconclusiveError, never the not_a_stream the feed walk
// settled, DECAPI memoised and the waiting room ended its job on.
//
// Mutants: Service.ProbeVideoStatus returning the ANDROID_VR answer as it
// stands (the pre-fix probe: a nil error and not_a_stream); the escalation
// left out (no web_embedded call, no embed page); the escalation's own
// refusal returned in the probe's place (statusAnswerInconclusive not asked
// of it: a nil error again).
func TestStatusProbeRefusalWithNoBroadcastDetailsIsInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       PlayabilityError
	}{
		{"the bot wall", botWallBody, PlayabilityLoginRequired},
		// The made-for-kids refusal yt-dlp falls back to web_embedded on is
		// UNPLAYABLE or ERROR (_video.py:3145-3153); its exact text from
		// android_vr has not been captured, so these are its two statuses.
		{"an UNPLAYABLE refusal", `{"playabilityStatus":{"status":"UNPLAYABLE","reason":"This video can't be played here"}}`, PlayabilityUnknown},
		{"an ERROR refusal", `{"playabilityStatus":{"status":"ERROR","reason":"This video is unavailable"}}`, PlayabilityUnavailable},
		{"an answer with no playability at all", `{}`, PlayabilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			embeds := stubEmbedPage(t)
			tr := serveBodies(t, map[string]string{clientAndroidVR: tc.body, clientWebEmbedded: botWallBody})
			svc := NewService(cookies.NewCookieJar(), noopLogger{})

			info, err := svc.ProbeVideoStatus(context.Background(), "test1234567")

			inconclusive, ok := errors.AsType[*StatusProbeInconclusiveError](err)
			if !ok {
				t.Fatalf("ProbeVideoStatus = (%+v, %v), want a *StatusProbeInconclusiveError", info, err)
			}
			if info != nil {
				t.Errorf("info = %+v alongside the error, want nil: the refusal is no answer to hand on", info)
			}
			if inconclusive.Playability != tc.want || inconclusive.Escalation != "web_embedded" {
				t.Errorf("error = %+v, want android_vr's %s and the web_embedded escalation", inconclusive, tc.want)
			}
			if inconclusive.EscalationErr == nil || !strings.Contains(inconclusive.EscalationErr.Error(), "too") {
				t.Errorf("EscalationErr = %v, want web_embedded's own refusal", inconclusive.EscalationErr)
			}
			if n, m := len(tr.of(clientAndroidVR)), len(tr.of(clientWebEmbedded)); n != 1 || m != 1 {
				t.Errorf("android_vr asked %d times, web_embedded %d, want once each: one escalation, never a loop", n, m)
			}
			if n := len(tr.of(clientTV)); n != 0 {
				t.Errorf("TV asked %d times on a signed-out jar, want none: anonymous TV is bot-walled", n)
			}
			if n := embeds.Load(); n != 1 {
				t.Errorf("embed page fetched %d times, want once, for its encryptedHostFlags", n)
			}
		})
	}
}

// TestStatusProbeAnswersThatSaySomethingAreNotEscalated: only the refused
// not_a_stream that states none of live_status's flags is inconclusive.
// Everything else ANDROID_VR answers is returned as it was, on one call: a
// refusal that says the video is not live content (upstream's not_live), an
// OK answer, and the refusals classifyStream reads upcoming or live off
// something the response did say.
//
// Mutants: statusAnswerInconclusive without its liveFlagsStated conjunct
// (the stated-false rows escalate), without its refusal conjunct (the OK
// answer with no videoDetails escalates), or without its not_a_stream
// conjunct (the liveStreamability row, a refusal stating no flag,
// escalates); parsePlayerResponse not stamping liveFlagsStated (the
// stated-false rows escalate).
func TestStatusProbeAnswersThatSaySomethingAreNotEscalated(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       StreamStatus
	}{
		{"a refusal stating isLiveContent false", `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in"},` +
			`"videoDetails":{"videoId":"test1234567","isLiveContent":false}}`, StreamNotAStream},
		{"a refusal stating isLive false", `{"playabilityStatus":{"status":"ERROR","reason":"This video is unavailable"},` +
			`"videoDetails":{"videoId":"test1234567","isLive":false}}`, StreamNotAStream},
		{"a plain upload", `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"test1234567","isLiveContent":false}}`, StreamNotAStream},
		{"an OK answer with no videoDetails", `{"playabilityStatus":{"status":"OK"}}`, StreamNotAStream},
		{"an offline waiting room with no videoDetails", `{"playabilityStatus":{"status":"LIVE_STREAM_OFFLINE","reason":"This live event will begin in a few moments."}}`, StreamUpcoming},
		{"a refused waiting room with only the liveStreamability renderer", `{"playabilityStatus":{"status":"UNPLAYABLE","reason":"x",` +
			`"liveStreamability":{"liveStreamabilityRenderer":{"videoId":"test1234567"}}}}`, StreamUpcoming},
		{"a members-only live stream", `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Join this channel to get access to members-only content"},` +
			`"videoDetails":{"videoId":"test1234567","isLive":true,"isLiveContent":true}}`, StreamLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			embeds := stubEmbedPage(t)
			tr := serveBodies(t, map[string]string{clientAndroidVR: tc.body, clientWebEmbedded: upcomingEmbeddedBody})
			svc := NewService(cookies.NewCookieJar(), noopLogger{})

			info, err := svc.ProbeVideoStatus(context.Background(), "test1234567")

			if err != nil {
				t.Fatalf("ProbeVideoStatus: %v, want android_vr's answer", err)
			}
			if info.StreamStatus != tc.want {
				t.Errorf("StreamStatus = %s, want %s", info.StreamStatus, tc.want)
			}
			if n := len(tr.of(clientWebEmbedded)) + int(embeds.Load()); n != 0 {
				t.Errorf("escalated (%d web_embedded calls and embed fetches) on an answer that said something", n)
			}
		})
	}
}

// TestStatusProbeEscalationAnswersInTheProbesPlace: an escalation that
// answers with the broadcast's flags is the probe's answer — so a bot-walled
// or made-for-kids upcoming stream is found, waited for and recorded. Signed
// out it is web_embedded, sent with the embed page's encryptedHostFlags
// (f2bd3202c), and as a probe: no PLAYER PO token although a minter and
// visitor data are there (owner decision O-R), and no player adopted for an
// STS. Signed in it is the authenticated TV probe, and web_embedded is not
// asked.
//
// Mutants: the escalation always web_embedded (the signed-in row asks it and
// not TV) or always TV (the signed-out row asks TV); fetchWithEmbedded's PO
// token mint not skipped for a probe (a token minted and sent); its player
// adoption not skipped (the answer carries the embed page's player); the
// embed page not fetched (no encryptedHostFlags).
func TestStatusProbeEscalationAnswersInTheProbesPlace(t *testing.T) {
	t.Run("signed out: web_embedded with encryptedHostFlags, as a probe", func(t *testing.T) {
		embeds := stubEmbedPage(t)
		tr := serveBodies(t, map[string]string{clientAndroidVR: botWallBody, clientWebEmbedded: upcomingEmbeddedBody})
		svc := NewService(cookies.NewCookieJar(), noopLogger{})
		svc.SetVisitorData("vd")
		pot := &countingPotProvider{token: "pot"}
		svc.PlayerAPI.SetPotProvider(pot)

		info, err := svc.ProbeVideoStatus(context.Background(), "test1234567")

		if err != nil {
			t.Fatalf("ProbeVideoStatus: %v, want web_embedded's answer", err)
		}
		if info.StreamStatus != StreamUpcoming || !info.IsUpcoming {
			t.Errorf("StreamStatus = %s (IsUpcoming %v), want web_embedded's upcoming", info.StreamStatus, info.IsUpcoming)
		}
		emb := tr.of(clientWebEmbedded)
		if len(emb) != 1 || embeds.Load() != 1 {
			t.Fatalf("web_embedded asked %d times, embed page fetched %d, want once each", len(emb), embeds.Load())
		}
		if !strings.Contains(emb[0], `"encryptedHostFlags":"ehf"`) {
			t.Errorf("web_embedded body %s, want the embed page's encryptedHostFlags", emb[0])
		}
		if pot.calls != 0 || strings.Contains(emb[0], "serviceIntegrityDimensions") {
			t.Errorf("a PLAYER PO token was minted (%d) or sent (%s) on a probe", pot.calls, emb[0])
		}
		if strings.Contains(emb[0], "signatureTimestamp") || info.PlayerURL != "" {
			t.Errorf("the probe adopted a player (PlayerURL %q, body %s); its answer is read for videoDetails alone", info.PlayerURL, emb[0])
		}
		if n := len(tr.of(clientTV)); n != 0 {
			t.Errorf("TV asked %d times on a signed-out jar, want none", n)
		}
	})

	t.Run("signed in: the authenticated TV probe", func(t *testing.T) {
		embeds := stubEmbedPage(t)
		stubHomepage(t, nil)
		tr := serveBodies(t, map[string]string{clientAndroidVR: botWallBody, clientTV: liveTVBody, clientWebEmbedded: upcomingEmbeddedBody})
		jar, _ := loadAccountJar(t, accountCookies("a"))
		svc := NewService(jar, noopLogger{})

		info, err := svc.ProbeVideoStatus(context.Background(), "test1234567")

		if err != nil {
			t.Fatalf("ProbeVideoStatus: %v, want TV's answer", err)
		}
		if info.StreamStatus != StreamLive {
			t.Errorf("StreamStatus = %s, want TV's live", info.StreamStatus)
		}
		if info.HlsManifestSource != "tv_auth" {
			t.Errorf("HlsManifestSource = %q, want the authenticated TV probe's tv_auth", info.HlsManifestSource)
		}
		if n, m := len(tr.of(clientAndroidVR)), len(tr.of(clientTV)); n != 1 || m != 1 {
			t.Errorf("android_vr asked %d times, TV %d, want once each", n, m)
		}
		if n := len(tr.of(clientWebEmbedded)) + int(embeds.Load()); n != 0 {
			t.Errorf("web_embedded asked (%d calls and embed fetches) on a signed-in jar, want only TV", n)
		}
	})
}

// TestStatusProbeEscalationThatFailsLeavesItInconclusive: an escalation that
// fails settles nothing either, and its failure rides the error — unwrapped,
// so a consumer can still tell a cancelled context from a refusal. A context
// that ended with ANDROID_VR's answer asks nothing more.
//
// Mutants: the escalation's failure returned bare (no
// *StatusProbeInconclusiveError: the waiting room counted a web_embedded 400
// toward its give-up, internal/worker's classifyProbeErr reading "http 4");
// Unwrap dropped (errors.Is cannot see the cancellation); the ended-context
// check dropped (the escalation is still attempted: Escalation names it).
func TestStatusProbeEscalationThatFailsLeavesItInconclusive(t *testing.T) {
	t.Run("an HTTP failure", func(t *testing.T) {
		stubEmbedPage(t)
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			clientAndroidVR:   {http.StatusOK, botWallBody},
			clientWebEmbedded: {http.StatusBadRequest, `{"error":{"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`},
		}}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })
		svc := NewService(cookies.NewCookieJar(), noopLogger{})

		_, err := svc.ProbeVideoStatus(context.Background(), "test1234567")

		inconclusive, ok := errors.AsType[*StatusProbeInconclusiveError](err)
		if !ok {
			t.Fatalf("err = %v, want a *StatusProbeInconclusiveError", err)
		}
		if inconclusive.EscalationErr == nil || !strings.Contains(inconclusive.EscalationErr.Error(), "HTTP 400") {
			t.Errorf("EscalationErr = %v, want web_embedded's HTTP 400", inconclusive.EscalationErr)
		}
		if got := strings.Join(tr.calls, ","); got != clientAndroidVR+","+clientWebEmbedded {
			t.Errorf("calls %s, want android_vr then one web_embedded", got)
		}
	})

	t.Run("a context that ended with android_vr's answer", func(t *testing.T) {
		embeds := stubEmbedPage(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tr := &cancellingTransport{answer: botWallBody, cancel: cancel}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })
		svc := NewService(cookies.NewCookieJar(), noopLogger{})

		_, err := svc.ProbeVideoStatus(ctx, "test1234567")

		inconclusive, ok := errors.AsType[*StatusProbeInconclusiveError](err)
		if !ok {
			t.Fatalf("err = %v, want a *StatusProbeInconclusiveError", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to read as the cancellation", err)
		}
		if inconclusive.Escalation != "" || tr.calls.Load() != 1 || embeds.Load() != 0 {
			t.Errorf("escalated to %q (%d player calls, %d embed fetches) on an ended context, want nothing asked",
				inconclusive.Escalation, tr.calls.Load(), embeds.Load())
		}
	})
}

// cancellingTransport answers every player call with answer, cancelling the
// caller's context as it does — a shutdown landing between the probe and its
// escalation.
type cancellingTransport struct {
	answer string
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (tr *cancellingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	tr.cancel()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tr.answer)), Header: make(http.Header), Request: req}, nil
}

// TestStatusLiveFlagsStated pins the rule against yt-dlp's live_status
// (_list_formats, _video.py:3861-3874): any of the values it decides on,
// stated, says something — a true isUpcoming or isPostLiveDvr, an isLive or
// isLiveContent stated either way, the microformat's isLiveNow — and only a
// response stating none of them is live_status None.
//
// Mutants: either key list cut short; isUpcoming or isPostLiveDvr counted
// when false; the isLiveNow arm dropped.
func TestStatusLiveFlagsStated(t *testing.T) {
	lbd := func(m map[string]any) map[string]any { return map[string]any{"liveBroadcastDetails": m} }
	for _, tc := range []struct {
		name         string
		videoDetails map[string]any
		microformat  map[string]any
		want         bool
	}{
		{"nothing at all", nil, nil, false},
		{"videoDetails with no flag", map[string]any{"videoId": "v", "title": "t"}, nil, false},
		{"isLive true", map[string]any{"isLive": true}, nil, true},
		{"isLive false", map[string]any{"isLive": false}, nil, true},
		{"isLiveContent true", map[string]any{"isLiveContent": true}, nil, true},
		{"isLiveContent false", map[string]any{"isLiveContent": false}, nil, true},
		{"isUpcoming true", map[string]any{"isUpcoming": true}, nil, true},
		{"isUpcoming false alone", map[string]any{"isUpcoming": false}, nil, false},
		{"isPostLiveDvr true", map[string]any{"isPostLiveDvr": true}, nil, true},
		{"isPostLiveDvr false alone", map[string]any{"isPostLiveDvr": false}, nil, false},
		{"isLiveNow stated", nil, lbd(map[string]any{"isLiveNow": false}), true},
		{"broadcast details without isLiveNow", nil, lbd(map[string]any{"startTimestamp": "2026-10-31T00:00:00Z"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusLiveFlagsStated(tc.videoDetails, tc.microformat); got != tc.want {
				t.Errorf("statusLiveFlagsStated = %v, want %v", got, tc.want)
			}
		})
	}
}

// sameBodyTransport answers every player call with one body.
type sameBodyTransport struct{ body string }

func (tr sameBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tr.body)), Header: make(http.Header), Request: req}, nil
}

// TestAnExtractionKeepsTheRefusalsPlayabilityStatus: the answer to a video
// YouTube removed for a reason the keyword rules have no word for carries
// the ERROR it was refused with (VideoInfo.PlayabilityStatus) beside the
// unknown verdict — out of both cascades, as the waiting loop's full fetch
// reads them (fullFetchSaysRemoved, internal/worker), and out of the status
// probe's own parse, which the escalation hands on as its inconclusive error.
//
// Mutant: parsePlayerResponse no longer stamping PlayabilityStatus (every
// row reads "").
func TestAnExtractionKeepsTheRefusalsPlayabilityStatus(t *testing.T) {
	const removed = `{"playabilityStatus":{"status":"ERROR","reason":"This video has been removed for violating YouTube's Terms of Service"}}`
	for name, run := range map[string]func(*PlayerAPI) (*VideoInfo, error){
		"public cascade": func(p *PlayerAPI) (*VideoInfo, error) {
			return p.GetVideoInfoPublic(context.Background(), "test1234567")
		},
		"authenticated cascade": func(p *PlayerAPI) (*VideoInfo, error) {
			return p.GetVideoInfoAuthenticated(context.Background(), "test1234567")
		},
		"status probe's parse": func(p *PlayerAPI) (*VideoInfo, error) {
			return p.ProbeVideoStatus(context.Background(), "test1234567", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubWatchPageWithPlayerResponse(t, removed)
			orig := apiClient
			apiClient = &http.Client{Transport: sameBodyTransport{removed}}
			t.Cleanup(func() { apiClient = orig })

			info, err := run(newRetryTestAPI())
			if err != nil {
				t.Fatalf("extraction: %v", err)
			}
			if info.PlayabilityStatus != "ERROR" || info.PlayabilityError != PlayabilityUnknown || info.StreamStatus != StreamNotAStream {
				t.Errorf("answer = status %q, verdict %q, stream %q; want ERROR, unknown and not_a_stream",
					info.PlayabilityStatus, info.PlayabilityError, info.StreamStatus)
			}
		})
	}
}
