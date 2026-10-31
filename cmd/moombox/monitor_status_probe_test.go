package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeYouTube answers the status probe's player calls by client
// (X-YouTube-Client-Name) and serves an embed page, at the URLs
// constants.YouTubeURLs names for the length of the test. It records the
// clients asked, in order.
type fakeYouTube struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeYouTube) asked() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}

func serveFakeYouTube(t *testing.T, bodies map[string]string) *fakeYouTube {
	t.Helper()
	f := &fakeYouTube{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			_, _ = io.WriteString(w, `<script>ytcfg.set({"WEB_PLAYER_CONTEXT_CONFIGS":{"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER":{"encryptedHostFlags":"ehf"}}});</script>`)
		case r.URL.Path == "/youtubei/v1/player":
			client := r.Header.Get("X-YouTube-Client-Name")
			f.mu.Lock()
			f.calls = append(f.calls, client)
			f.mu.Unlock()
			body, ok := bodies[client]
			if !ok {
				http.Error(w, `{"error":{"message":"not served here"}}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	orig := constants.YouTubeURLs
	constants.YouTubeURLs.Base = srv.URL
	constants.YouTubeURLs.API = srv.URL + "/youtubei/v1"
	constants.YouTubeURLs.Embed = srv.URL + "/embed"
	t.Cleanup(func() { constants.YouTubeURLs = orig })
	return f
}

// TestABotWalledStatusProbeReachesTheMonitorsAsAnErroredProbe is G8 end to
// end, on the wiring both monitors run (monitorProbe over
// youtube.Service.ProbeVideoStatus) and the classification DECAPI and the
// feed walk share: ANDROID_VR's bot wall — LOGIN_REQUIRED, no videoDetails —
// on a signed-out install, on a channel that archives live content only.
// Before, the probe answered not_a_stream: the monitor skipped the video as
// non-stream content, DECAPI memoised that as terminal and the feed walk
// stored it settled, never to probe it again. Now the probe escalates once to
// web_embedded, and:
//
//   - when that refuses the same way, the monitors see a failed probe — no
//     classification, nothing stored, the failure counted by the failure
//     tracker — marked monitor.ErrProbeInconclusive, so DECAPI probes the
//     video again next cycle and the feed walk holds the row on its ladder;
//   - when it answers, its answer is the classification, so an upcoming
//     stream the wall hid becomes a job.
//
// Mutants: youtube.Service.ProbeVideoStatus returning ANDROID_VR's answer as
// it stands (the first row classifies not_a_stream); the escalation left out
// (the second row's stream is never found); monitorProbe not marking the
// refusal inconclusive.
func TestABotWalledStatusProbeReachesTheMonitorsAsAnErroredProbe(t *testing.T) {
	const botWall = `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you’re not a bot"}}`
	const upcoming = `{"playabilityStatus":{"status":"LIVE_STREAM_OFFLINE","reason":"This live event will begin in a few moments."},` +
		`"videoDetails":{"videoId":"test1234567","title":"Stream","author":"a","isLiveContent":true,"isUpcoming":true}}`
	for _, tc := range []struct {
		name, embedded, wantStatus string
		wantProcess, wantFailure   bool
	}{
		{"the escalation refuses too", botWall, "", false, true},
		{"the escalation answers", upcoming, "upcoming", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yt := serveFakeYouTube(t, map[string]string{"28": botWall, "56": tc.embedded})
			svc := youtube.NewService(cookies.NewCookieJar(), &nopLogger{})
			tracker := monitor.NewMetadataFailureTracker()

			res := monitor.ProcessYouTubeVideo(monitor.ProcessYouTubeVideoParams{
				Ctx:        context.Background(),
				VideoID:    "test1234567",
				Title:      "Stream",
				Channel:    &config.ChannelConfig{Name: "c", IncludeNonLiveContent: false},
				ProbeVideo: monitorProbe(svc.ProbeVideoStatus),
				Tracker:    tracker,
				Logger:     &nopLogger{},
			})

			if res.StreamStatus != tc.wantStatus || res.ShouldProcess != tc.wantProcess {
				t.Errorf("classified %q, process %v; want %q, %v", res.StreamStatus, res.ShouldProcess, tc.wantStatus, tc.wantProcess)
			}
			// The tracker's count after one more failure says whether the
			// probe itself was counted as one: 2 if it was, 1 if it succeeded.
			if n, _ := tracker.RecordFailure("test1234567"); (n == 2) != tc.wantFailure {
				t.Errorf("tracker at %d after one more failure; the probe counted as a failure: %v, want %v", n, n == 2, tc.wantFailure)
			}
			if got := yt.asked(); got != "28,56" {
				t.Errorf("clients asked %q, want android_vr then its one web_embedded escalation", got)
			}

			// The failure the monitors see is marked inconclusive, its chain
			// and text kept: the feed walk holds the row on its backoff
			// ladder for it, where a transient failure is retried next cycle.
			_, err := monitorProbe(svc.ProbeVideoStatus)(context.Background(), "test1234567")
			if got := errors.Is(err, monitor.ErrProbeInconclusive); got != tc.wantFailure {
				t.Errorf("the probe's error %v is marked inconclusive: %v, want %v", err, got, tc.wantFailure)
			}
			if _, ok := errors.AsType[*youtube.StatusProbeInconclusiveError](err); ok != tc.wantFailure {
				t.Errorf("the probe's error %v keeps the youtube error in its chain: %v, want %v", err, ok, tc.wantFailure)
			}
		})
	}
}

// TestMonitorProbeMarksOnlyAnInconclusiveProbe: monitorProbe marks a probe
// that could not tell (*youtube.StatusProbeInconclusiveError) with
// monitor.ErrProbeInconclusive, which is what the feed walk holds a row on
// (an hour, then six, then daily — FeedMonitor.noteProbeOutcome), and marks
// nothing else: a transport failure is transient, and a held freshly live
// stream would go unseen for an hour. The mark keeps the error's text, so
// the walk's failure line still names the refusal, and its chain, so a
// cancelled context still reads as one.
//
// Mutants: monitorProbe passing every error through unmarked (the
// inconclusive one is not marked); marking every error (the transport
// failure is).
func TestMonitorProbeMarksOnlyAnInconclusiveProbe(t *testing.T) {
	inconclusive := &youtube.StatusProbeInconclusiveError{
		Playability: youtube.PlayabilityLoginRequired, Reason: "Sign in to confirm you’re not a bot",
		Escalation: youtube.StatusProbeEscalationWebEmbedded, EscalationErr: context.Canceled,
	}
	transport := errors.New("dial tcp: connection reset by peer")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"an inconclusive probe", inconclusive, true},
		{"a transport failure", transport, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := monitorProbe(func(context.Context, string) (*youtube.VideoInfo, error) {
				return nil, tc.err
			})(context.Background(), "test1234567")
			if got := errors.Is(err, monitor.ErrProbeInconclusive); got != tc.want {
				t.Errorf("marked inconclusive: %v, want %v", got, tc.want)
			}
			if err == nil || err.Error() != tc.err.Error() {
				t.Errorf("error text %v, want the probe's own, %q", err, tc.err.Error())
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("the probe's error %v is not in the chain", tc.err)
			}
		})
	}
	_, err := monitorProbe(func(context.Context, string) (*youtube.VideoInfo, error) { return nil, inconclusive })(context.Background(), "test1234567")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the mark hides what the youtube error wraps: errors.Is(%v, context.Canceled) is false", err)
	}
}

// TestAMembersOnlyStatusProbeRefusalReachesTheMonitorsAsTheVerdict: the one
// refusal with no broadcast details the status probe does not call
// inconclusive. A members-only refusal is the access verdict the feed walk
// acts on — probeRow relabels the row 'membership' on its members_only and
// retries with the authenticated probe the same cycle (spec §9) — so it
// reaches the monitors as a probe result carrying that verdict, on the one
// ANDROID_VR call, signed in or out: never an errored probe with nothing to
// relabel, and never replaced by a member's TV answer, which would leave the
// row unlabelled and out of the membership cookie gate's reach.
//
// Mutant: statusAnswerInconclusive without its members-only exclusion (an
// errored probe signed out; TV's answer, with no members_only, signed in).
func TestAMembersOnlyStatusProbeRefusalReachesTheMonitorsAsTheVerdict(t *testing.T) {
	const membersOnly = `{"playabilityStatus":{"status":"LOGIN_REQUIRED",` +
		`"reason":"Join this channel to get access to members-only content like this video, and other exclusive perks."}}`
	const memberTV = `{"playabilityStatus":{"status":"LIVE_STREAM_OFFLINE","reason":"This live event will begin in a few moments."},` +
		`"videoDetails":{"videoId":"test1234567","title":"Stream","author":"a","isLiveContent":true,"isUpcoming":true}}`
	for _, tc := range []struct {
		name string
		jar  func(t *testing.T) *cookies.CookieJar
	}{
		{"signed out", func(*testing.T) *cookies.CookieJar { return cookies.NewCookieJar() }},
		{"signed in", func(t *testing.T) *cookies.CookieJar {
			return mustLoadJarFixture(t, "# Netscape HTTP Cookie File\n"+
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tsapisid-a\n"+
				".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tlogininfo-a\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yt := serveFakeYouTube(t, map[string]string{"28": membersOnly, "7": memberTV, "56": memberTV})
			svc := youtube.NewService(tc.jar(t), &nopLogger{})

			res, err := monitorProbe(svc.ProbeVideoStatus)(context.Background(), "test1234567")

			if err != nil {
				t.Fatalf("probe: %v, want android_vr's members-only verdict", err)
			}
			if res.PlayabilityError != "members_only" {
				t.Errorf("PlayabilityError = %q, want members_only: the verdict the walk relabels the row on", res.PlayabilityError)
			}
			if got := yt.asked(); got != "28" {
				t.Errorf("clients asked %q, want android_vr alone: the walk owns a members-only row's authenticated probe", got)
			}
		})
	}
}
