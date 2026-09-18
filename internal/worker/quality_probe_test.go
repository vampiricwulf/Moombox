package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakeProbeClient counts which of the three entry points the quality probe
// chose. Nothing here touches the network: the row is about REQUEST SHAPE, so
// the assertion has to be on the calls themselves.
type fakeProbeClient struct {
	probe     int
	probeAuth int
	full      int

	probeInfo     *youtube.VideoInfo
	probeAuthInfo *youtube.VideoInfo
	fullInfo      *youtube.VideoInfo
	fullErr       error
}

func (f *fakeProbeClient) ProbeVideoStatus(context.Context, string) (*youtube.VideoInfo, error) {
	f.probe++
	return f.probeInfo, nil
}

func (f *fakeProbeClient) ProbeVideoStatusAuthenticated(context.Context, string) (*youtube.VideoInfo, error) {
	f.probeAuth++
	return f.probeAuthInfo, nil
}

func (f *fakeProbeClient) GetVideoInfo(context.Context, string) (*youtube.VideoInfo, error) {
	f.full++
	return f.fullInfo, f.fullErr
}

// splitAdaptive is a format pool the manifest-free path can address: split
// video + audio, no contentLength.
func splitAdaptive() []youtube.Format {
	w, h := 1920, 1080
	return []youtube.Format{
		{Itag: 137, URL: "https://x/v", MimeType: `video/mp4; codecs="avc1.640028"`, Width: &w, Height: &h},
		{Itag: 140, URL: "https://x/a", MimeType: `audio/mp4; codecs="mp4a.40.2"`},
	}
}

// TestProbeVideoInfoUsesTheCheapAuthenticatedProbe is owner decision O-H. The
// 30 s monitor used to run the FULL authenticated cascade — a 1-5 MB cookied
// watch page plus 3-7 player calls — for every auth-walled stream, which at
// this cadence is ~120 watch pages and >=360 player calls per hour per job.
// ProbeVideoStatusAuthenticated is one TV-with-cookies call and already
// returns the pool the probe selects from.
//
// Mutants this kills:
//   - requiresAuth still routed to GetVideoInfo  → full == 1, probeAuth == 0
//   - requiresAuth routed to the COOKIELESS probe → probe == 1 (android_vr 401s
//     on members-only content, so this must not happen)
func TestProbeVideoInfoUsesTheCheapAuthenticatedProbe(t *testing.T) {
	f := &fakeProbeClient{probeAuthInfo: &youtube.VideoInfo{Formats: splitAdaptive()}}

	info, err := probeVideoInfo(context.Background(), f, "vid", true)
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
}

// TestProbeVideoInfoFallsBackOnceForBothProbeKinds is the other half of O-H
// and the verifier's correction: the DOMINANT waste path is the PUBLIC stream
// whose android_vr probe returns neither DASH nor split-adaptive, which paid
// the probe AND the whole cascade every tick. The fallback stays — it is what
// makes the authenticated probe safe when TV-with-cookies turns out to carry
// no adaptiveFormats — but it fires ONCE per tick, for either probe kind.
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

			info, err := probeVideoInfo(context.Background(), f, "vid", tc.requiresAuth)
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

// TestProbeVideoInfoSurfacesANilInfoAsAnError keeps the defensive arm that
// exists because GetVideoInfo / ProbeVideoStatus can return (nil, nil) during
// a context-cancel shutdown race; the next dereference would panic the
// monitor goroutine.
//
// Mutants this kills: the nil guard removed → panic instead of an error.
func TestProbeVideoInfoSurfacesANilInfoAsAnError(t *testing.T) {
	f := &fakeProbeClient{}
	if _, err := probeVideoInfo(context.Background(), f, "vid", false); err == nil {
		t.Fatal("a nil info with no error was accepted")
	}

	g := &fakeProbeClient{probeInfo: &youtube.VideoInfo{}, fullErr: errors.New("boom")}
	if _, err := probeVideoInfo(context.Background(), g, "vid", false); err == nil {
		t.Fatal("a failing fallback was accepted")
	}
}
