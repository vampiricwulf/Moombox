package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeProbeClient counts which of the three entry points the quality probe
// chose. Nothing here touches the network: the row is about REQUEST SHAPE, so
// the assertion has to be on the calls themselves.
//
// Every entry point returns a NON-NIL info by default in the routing tests
// (see splitAdaptive): a fixture whose unchosen entry points answer (nil, nil)
// makes a mis-routed call die on the nil-info arm instead of on the call
// counts, which is the assertion the routing mutants are written against.
type fakeProbeClient struct {
	probe     int
	probeAuth int
	full      int

	hasCookies bool

	probeInfo     *youtube.VideoInfo
	probeErr      error
	probeAuthInfo *youtube.VideoInfo
	probeAuthErr  error
	fullInfo      *youtube.VideoInfo
	fullErr       error
}

func (f *fakeProbeClient) ProbeVideoStatus(context.Context, string) (*youtube.VideoInfo, error) {
	f.probe++
	return f.probeInfo, f.probeErr
}

func (f *fakeProbeClient) ProbeVideoStatusAuthenticated(context.Context, string) (*youtube.VideoInfo, error) {
	f.probeAuth++
	return f.probeAuthInfo, f.probeAuthErr
}

func (f *fakeProbeClient) GetVideoInfo(context.Context, string) (*youtube.VideoInfo, error) {
	f.full++
	return f.fullInfo, f.fullErr
}

func (f *fakeProbeClient) HasAuthCookies() bool { return f.hasCookies }

// splitAdaptive is a format pool the manifest-free path can address: split
// video + audio, no contentLength.
func splitAdaptive() []youtube.Format {
	w, h := 1920, 1080
	return []youtube.Format{
		{Itag: 137, URL: "https://x/v", MimeType: `video/mp4; codecs="avc1.640028"`, Width: &w, Height: &h},
		{Itag: 140, URL: "https://x/a", MimeType: `audio/mp4; codecs="mp4a.40.2"`},
	}
}

// selectable builds a probe answer the monitor can select from, tagged so a
// test can tell WHICH entry point produced the info it got back.
func selectable(title string) *youtube.VideoInfo {
	return &youtube.VideoInfo{Title: title, Formats: splitAdaptive()}
}

// TestProbeVideoInfoUsesTheCheapAuthenticatedProbe is owner decision O-H. The
// 30 s monitor used to run the FULL authenticated cascade — a 1-5 MB cookied
// watch page plus 3-7 player calls — for every auth-walled stream, which at
// this cadence is ~120 watch pages and >=360 player calls per hour per job.
// ProbeVideoStatusAuthenticated is one TV-with-cookies call and already
// returns the pool the probe selects from.
//
// Every entry point here returns a selectable answer, so a mis-route is caught
// by the CALL COUNTS rather than by an incidental nil dereference downstream
// (fix round 1, m4).
//
// Mutants this kills:
//   - requiresAuth still routed to GetVideoInfo  → full == 1, probeAuth == 0
//   - requiresAuth routed to the COOKIELESS probe → probe == 1 (android_vr 401s
//     on members-only content, so this must not happen)
func TestProbeVideoInfoUsesTheCheapAuthenticatedProbe(t *testing.T) {
	f := &fakeProbeClient{
		hasCookies:    true,
		probeInfo:     selectable("cookieless probe"),
		probeAuthInfo: selectable("cookied probe"),
		fullInfo:      selectable("cascade"),
	}

	info, err := probeVideoInfo(context.Background(), f, "vid", true, nil)
	if err != nil {
		t.Fatalf("probeVideoInfo: %v", err)
	}
	if info == nil || len(info.Formats) != 2 {
		t.Fatalf("info = %+v, want the probe's two formats", info)
	}
	if f.probeAuth != 1 || f.full != 0 || f.probe != 0 {
		t.Errorf("calls: probeAuth=%d probe=%d full=%d; want exactly one authenticated probe",
			f.probeAuth, f.probe, f.full)
	}
	if info.Title != "cookied probe" {
		t.Errorf("info came from %q, want the cookied probe's answer", info.Title)
	}
}

// TestProbeVideoInfoFallsBackOnceForBothProbeKinds is the other half of O-H
// and the verifier's correction: the DOMINANT waste path is the PUBLIC stream
// whose android_vr probe returns neither DASH nor split-adaptive, which paid
// the probe AND the whole cascade every tick. The fallback stays — it is what
// makes the authenticated probe safe when TV-with-cookies turns out to carry
// no adaptiveFormats — but it fires ONCE per tick, for either probe kind.
//
// No cookies are configured on this fixture, so the cookied recovery hop
// (below) is not in play here: this is the cookieless install's shape.
//
// Mutants this kills:
//   - the fallback gated on !requiresAuth again  → the auth subtest sees full == 0
//   - the fallback looping or retrying           → full > 1
//   - the fallback firing when the probe already
//     had split-adaptive formats                 → covered by the test above
func TestProbeVideoInfoFallsBackOnceForBothProbeKinds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		requiresAuth bool
	}{
		{"public recovery path", false},
		{"authenticated probe with no adaptive formats", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			empty := &youtube.VideoInfo{} // no DASH URL, no formats
			f := &fakeProbeClient{
				probeInfo:     empty,
				probeAuthInfo: empty,
				fullInfo:      &youtube.VideoInfo{Formats: splitAdaptive()},
			}

			info, err := probeVideoInfo(context.Background(), f, "vid", tc.requiresAuth, nil)
			if err != nil {
				t.Fatalf("probeVideoInfo: %v", err)
			}
			if len(info.Formats) != 2 {
				t.Fatalf("info = %+v, want the fallback's formats", info)
			}
			if f.full != 1 {
				t.Errorf("GetVideoInfo called %d times, want exactly 1", f.full)
			}
		})
	}
}

// TestProbeVideoInfoTakesTheCookiedHopBeforeTheCascade is O-H's second clause —
// "auth-walled/RECOVERY streams" — and the branch the verifier called dominant.
// A public stream whose cookieless android_vr probe comes back with nothing
// selectable used to pay the whole cascade (a 1-5 MB cookied watch page plus
// three to seven player calls) on EVERY 30 s tick. When the install has
// cookies, one TV-with-cookies call is tried first, and a stream that answers
// there costs two player calls instead of the cascade.
//
// Mutants this kills:
//   - the recovery branch goes straight to the cascade → probeAuth == 0, full == 1
//   - the hop's answer discarded and the cascade run anyway → full == 1
func TestProbeVideoInfoTakesTheCookiedHopBeforeTheCascade(t *testing.T) {
	f := &fakeProbeClient{
		hasCookies:    true,
		probeInfo:     &youtube.VideoInfo{}, // android_vr: nothing selectable
		probeAuthInfo: selectable("cookied hop"),
		fullInfo:      selectable("cascade"),
	}

	info, err := probeVideoInfo(context.Background(), f, "vid", false, nil)
	if err != nil {
		t.Fatalf("probeVideoInfo: %v", err)
	}
	if f.probe != 1 || f.probeAuth != 1 || f.full != 0 {
		t.Errorf("calls: probe=%d probeAuth=%d full=%d; want the cookieless probe then the cookied hop, no cascade",
			f.probe, f.probeAuth, f.full)
	}
	if info.Title != "cookied hop" {
		t.Errorf("info came from %q, want the cookied hop's answer", info.Title)
	}
}

// TestProbeVideoInfoSkipsTheCookiedHopWithoutCookies keeps the hop from
// becoming a wasted call per tick on a cookieless install: Step 0's live
// evidence is that TV_DOWNGRADED without cookies answers login_required
// ("Sign in to confirm you're not a bot") and carries no formats at all.
//
// Mutants this kills:
//   - the hop taken unconditionally → probeAuth == 1
func TestProbeVideoInfoSkipsTheCookiedHopWithoutCookies(t *testing.T) {
	f := &fakeProbeClient{
		hasCookies:    false,
		probeInfo:     &youtube.VideoInfo{},
		probeAuthInfo: selectable("cookied hop"),
		fullInfo:      selectable("cascade"),
	}

	info, err := probeVideoInfo(context.Background(), f, "vid", false, nil)
	if err != nil {
		t.Fatalf("probeVideoInfo: %v", err)
	}
	if f.probe != 1 || f.probeAuth != 0 || f.full != 1 {
		t.Errorf("calls: probe=%d probeAuth=%d full=%d; want the cookieless probe then the cascade",
			f.probe, f.probeAuth, f.full)
	}
	if info.Title != "cascade" {
		t.Errorf("info came from %q, want the cascade's answer", info.Title)
	}
}

// TestProbeVideoInfoCascadesOnceWhenTheCookiedHopAlsoFails is the ceiling on
// the hop: it buys at most one extra player call, never a second cascade, and
// the tick still resolves exactly as it did before O-H.
//
// Mutants this kills:
//   - the cascade dropped once the hop has run → full == 0
//   - the hop retried/looped                   → probeAuth > 1
func TestProbeVideoInfoCascadesOnceWhenTheCookiedHopAlsoFails(t *testing.T) {
	f := &fakeProbeClient{
		hasCookies:    true,
		probeInfo:     &youtube.VideoInfo{},
		probeAuthInfo: &youtube.VideoInfo{},
		fullInfo:      selectable("cascade"),
	}

	info, err := probeVideoInfo(context.Background(), f, "vid", false, nil)
	if err != nil {
		t.Fatalf("probeVideoInfo: %v", err)
	}
	if f.probe != 1 || f.probeAuth != 1 || f.full != 1 {
		t.Errorf("calls: probe=%d probeAuth=%d full=%d; want one of each, cascade last",
			f.probe, f.probeAuth, f.full)
	}
	if info.Title != "cascade" {
		t.Errorf("info came from %q, want the cascade's answer", info.Title)
	}
}

// TestProbeVideoInfoFallsBackToTheCascadeOnAProbeError is the "never make
// recovery worse" ruling. One erroring player call must not end the tick: the
// cascade pools three to seven clients and routinely answers when one of them
// 403s, so a TV (or android_vr) client that starts failing would otherwise
// silently kill quality monitoring for the rest of the broadcast — visible
// only at Debug, since QualityMonitor.Run logs a probe error and skips.
//
// The one probe error that does NOT take this route is a substitution — see
// TestProbeVideoInfoEndsTheTickOnASubstitutedVideo.
//
// Mutants this kills:
//   - a probe error returned immediately → full == 0
//   - the cascade's error returned bare   → errors.As can no longer see the
//     mismatch through the wrap
func TestProbeVideoInfoFallsBackToTheCascadeOnAProbeError(t *testing.T) {
	for _, tc := range []struct {
		name         string
		requiresAuth bool
		probeErr     error
		authErr      error
	}{
		{"cookied probe 403s", true, nil, errors.New("403")},
		{"cookieless probe fails", false, errors.New("dial tcp: timeout"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProbeClient{
				probeErr:     tc.probeErr,
				probeAuthErr: tc.authErr,
				fullInfo:     selectable("cascade"),
			}

			info, err := probeVideoInfo(context.Background(), f, "vid", tc.requiresAuth, nil)
			if err != nil {
				t.Fatalf("probeVideoInfo: %v", err)
			}
			if f.full != 1 {
				t.Errorf("GetVideoInfo called %d times after a probe error, want exactly 1", f.full)
			}
			if info.Title != "cascade" {
				t.Errorf("info came from %q, want the cascade's answer", info.Title)
			}
		})
	}

	t.Run("the cascade's own mismatch still unwraps", func(t *testing.T) {
		f := &fakeProbeClient{
			probeErr: errors.New("dial tcp: timeout"),
			fullErr:  &youtube.VideoIDMismatchError{Requested: "vid", Got: "other"},
		}
		_, err := probeVideoInfo(context.Background(), f, "vid", false, nil)
		var mismatch *youtube.VideoIDMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("err = %v; want a wrapped *youtube.VideoIDMismatchError", err)
		}
	})
}

// TestProbeVideoInfoEndsTheTickOnASubstitutedVideo is the close-review ruling
// on Finding 1. A *youtube.VideoIDMismatchError is not the one-client-403 case
// the cascade fallback was ruled for: a substitute is served at the IP level,
// every client is substituted alike, and the cascade cannot answer differently
// — it only re-asks the same blocked IP seven more times. Left as an ordinary
// probe error, one blocked live job paid probe + cookied hop + a seven-client
// cascade EVERY 30 s tick (~1,080 player calls an hour) toward a service that
// was already rate-limiting it, which is exactly the condition where extra
// load prolongs the block.
//
// So a substitution ends the tick at the hop that saw it: no hop 2 after a
// hop-1 mismatch, no cascade after either. QualityMonitor.Run logs the error
// at Debug and keeps the previous quality, exactly as it does for the
// cancelled-context arm, and the next tick retries from scratch.
//
// Mutants this kill:
//   - the hop-1 guard removed → the auth row cascades (full == 1); the public
//     row also pays the cookied hop (probeAuth == 1)
//   - the hop-2 guard removed → the hop-2 row cascades (full == 1)
//   - the guard matching on == instead of errors.As → the wrapped row cascades
func TestProbeVideoInfoEndsTheTickOnASubstitutedVideo(t *testing.T) {
	mismatch := &youtube.VideoIDMismatchError{Requested: "vid", Got: "OTHERvideo1"}

	for _, tc := range []struct {
		name          string
		requiresAuth  bool
		probeInfo     *youtube.VideoInfo
		probeErr      error
		authErr       error
		wantProbe     int
		wantProbeAuth int
	}{
		{"auth-walled hop 1 is substituted", true, nil, nil, mismatch, 0, 1},
		{"cookieless hop 1 is substituted", false, nil, mismatch, nil, 1, 0},
		// Hop 1 answers, but with nothing selectable, so the cookied hop runs
		// and IT is the one served a substitute.
		{"the cookied hop 2 is substituted", false, &youtube.VideoInfo{}, nil, mismatch, 1, 1},
		{"a wrapped mismatch still ends the tick", true, nil, nil, fmt.Errorf("tv client: %w", mismatch), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProbeClient{
				hasCookies:   true,
				probeInfo:    tc.probeInfo,
				probeErr:     tc.probeErr,
				probeAuthErr: tc.authErr,
				fullInfo:     selectable("cascade"),
			}

			_, err := probeVideoInfo(context.Background(), f, "vid", tc.requiresAuth, nil)
			var mm *youtube.VideoIDMismatchError
			if !errors.As(err, &mm) {
				t.Fatalf("err = %v; want the substitution to be reported", err)
			}
			if f.full != 0 {
				t.Errorf("GetVideoInfo called %d times after a substitution, want 0 — the cascade only re-asks the blocked IP", f.full)
			}
			if f.probe != tc.wantProbe || f.probeAuth != tc.wantProbeAuth {
				t.Errorf("calls: probe=%d probeAuth=%d; want probe=%d probeAuth=%d",
					f.probe, f.probeAuth, tc.wantProbe, tc.wantProbeAuth)
			}
		})
	}
}

// TestProbeVideoInfoBuysNoCascadeOnACancelledContext keeps the shutdown race
// cheap: when the tick's context is already done, the cascade cannot succeed
// and every call it makes is waste on the way out.
//
// Mutants this kills:
//   - the ctx guard dropped → full == 1 during shutdown
func TestProbeVideoInfoBuysNoCascadeOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &fakeProbeClient{
		hasCookies: true,
		probeErr:   context.Canceled,
		fullInfo:   selectable("cascade"),
	}

	if _, err := probeVideoInfo(ctx, f, "vid", false, nil); err == nil {
		t.Fatal("a cancelled tick returned no error")
	}
	if f.probeAuth != 0 || f.full != 0 {
		t.Errorf("calls: probeAuth=%d full=%d; want neither the hop nor the cascade on a cancelled context",
			f.probeAuth, f.full)
	}
}

// TestProbeVideoInfoSurfacesANilInfoAsAnError keeps the defensive arm that
// exists because GetVideoInfo / ProbeVideoStatus can return (nil, nil) during
// a context-cancel shutdown race; the next dereference would panic the
// monitor goroutine. A nil probe answer is treated as "nothing selectable" and
// takes the cascade; only a nil answer from the cascade itself is terminal.
//
// Mutants this kills: the nil guard removed → panic instead of an error.
func TestProbeVideoInfoSurfacesANilInfoAsAnError(t *testing.T) {
	f := &fakeProbeClient{}
	if _, err := probeVideoInfo(context.Background(), f, "vid", false, nil); err == nil {
		t.Fatal("a nil info with no error was accepted")
	}

	g := &fakeProbeClient{probeInfo: &youtube.VideoInfo{}, fullErr: errors.New("boom")}
	if _, err := probeVideoInfo(context.Background(), g, "vid", false, nil); err == nil {
		t.Fatal("a failing fallback was accepted")
	}
}

// TestProbeVideoInfoLogsTheBranchItTook is the controller's Debug ruling, and
// the label honesty fix (round 1, m2): the TV probe is only "+cookies" when
// cookies were actually configured — Service.ProbeVideoStatusAuthenticated
// sends whatever the jar holds and never checks, so an unconditional label
// claims credentials a cookieless install never sent.
//
// Mutants this kills:
//   - the probe label hard-coded to "+cookies" → the cookieless case logs it
//   - a branch that logs nothing               → no line for that branch
func TestProbeVideoInfoLogsTheBranchItTook(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hasCookies bool
		wantProbe  string
	}{
		{"cookied install", true, "tv_downgraded+cookies"},
		{"cookieless install", false, "tv_downgraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg := &recordingProbeLogger{}
			f := &fakeProbeClient{hasCookies: tc.hasCookies, probeAuthInfo: selectable("cookied probe")}

			if _, err := probeVideoInfo(context.Background(), f, "vid", true, lg); err != nil {
				t.Fatalf("probeVideoInfo: %v", err)
			}
			if len(lg.lines) != 1 {
				t.Fatalf("logged %d Debug lines, want exactly 1: %+v", len(lg.lines), lg.lines)
			}
			if got := lg.lines[0].value("probe"); got != tc.wantProbe {
				t.Errorf("probe label = %q, want %q", got, tc.wantProbe)
			}
		})
	}

	t.Run("the cascade branch says so", func(t *testing.T) {
		lg := &recordingProbeLogger{}
		f := &fakeProbeClient{probeInfo: &youtube.VideoInfo{}, fullInfo: selectable("cascade")}

		if _, err := probeVideoInfo(context.Background(), f, "vid", false, lg); err != nil {
			t.Fatalf("probeVideoInfo: %v", err)
		}
		if len(lg.lines) != 1 || lg.lines[0].value("probe") != "android_vr" {
			t.Fatalf("lines = %+v, want one android_vr line", lg.lines)
		}
		if got := lg.lines[0].value("hop"); got != "android_vr" {
			t.Errorf("hop = %q, want the probe's own kind when the reason came from hop 1", got)
		}
	})

	// Close-review Finding 8: the fallback line read
	// `probe android_vr reason "hop 403"` when the reason came from the
	// COOKIED hop, attributing the TV client's failure to android_vr. The
	// `hop` field names where the reason actually came from.
	//
	// Mutant this kills: the `probeHop = tvKind` assignment removed → hop
	// reads "android_vr" while the reason is the TV hop's error.
	t.Run("the fallback names the hop the reason came from", func(t *testing.T) {
		lg := &recordingProbeLogger{}
		f := &fakeProbeClient{
			hasCookies:   true,
			probeInfo:    &youtube.VideoInfo{}, // hop 1: nothing selectable, no error
			probeAuthErr: errors.New("hop 403"),
			fullInfo:     selectable("cascade"),
		}

		if _, err := probeVideoInfo(context.Background(), f, "vid", false, lg); err != nil {
			t.Fatalf("probeVideoInfo: %v", err)
		}
		if len(lg.lines) != 1 {
			t.Fatalf("logged %d Debug lines, want exactly 1: %+v", len(lg.lines), lg.lines)
		}
		if got := lg.lines[0].value("hop"); got != "tv_downgraded+cookies" {
			t.Errorf("hop = %q, want the cookied hop that produced the reason", got)
		}
		if got := lg.lines[0].value("reason"); got != "hop 403" {
			t.Errorf("reason = %q, want the cookied hop's error", got)
		}
		if got := lg.lines[0].value("probe"); got != "android_vr" {
			t.Errorf("probe = %q, want the probe the tick started with", got)
		}
	})
}

// recordingProbeLogger captures Debug lines. It satisfies the worker's
// anonymous logger interface (the four methods); only Debug is asserted on.
type recordingProbeLogger struct{ lines []probeLogLine }

type probeLogLine struct {
	msg  string
	args []any
}

// value returns the string value logged for key, or "" when absent.
func (l probeLogLine) value(key string) string {
	for i := 0; i+1 < len(l.args); i += 2 {
		if k, ok := l.args[i].(string); ok && k == key {
			s, _ := l.args[i+1].(string)
			return s
		}
	}
	return ""
}

func (r *recordingProbeLogger) Debug(msg string, args ...any) {
	r.lines = append(r.lines, probeLogLine{msg: msg, args: args})
}
func (r *recordingProbeLogger) Info(string, ...any)  {}
func (r *recordingProbeLogger) Warn(string, ...any)  {}
func (r *recordingProbeLogger) Error(string, ...any) {}
