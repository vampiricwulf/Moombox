package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// fakePotProvider is a minimal gvsTokenMinter double — it does NOT satisfy
// *bgutils.PotProvider, so refreshGvsCredentials's asPotProvider() helper
// resolves it to nil (nothing to invalidate) and minterUsable() reports it
// usable for the mint step. That split is exactly what makes it useful
// here: the test can pin the mint call's arguments without pulling in a
// real BotGuard sidecar.
type fakePotProvider struct {
	generate func(ctx context.Context, binding string, bypassCache bool) (string, error)
}

func (f *fakePotProvider) GeneratePoTokenString(ctx context.Context, binding string, bypassCache bool) (string, error) {
	return f.generate(ctx, binding, bypassCache)
}

// TestRefreshGvsCredentialsBypassesTokenCache pins the property that makes
// recovery work at all: the re-mint must bypass the session cache. The token
// that just earned a 403 is the cached one, so handing it back unchanged
// would make the refresh a no-op — which is precisely the difference between
// the failing download and the manual cancel-and-resume that fixes it.
func TestRefreshGvsCredentialsBypassesTokenCache(t *testing.T) {
	var gotBypass bool
	var gotBinding string
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			gotBinding = binding
			gotBypass = bypassCache
			return "fresh-token", nil
		},
	}

	// job.YT is a real *youtube.Service (JobContext.YT is concrete, not an
	// interface — there is no stub seam), constructed with a nil cookie jar
	// and populated with visitor data the same way a watch-page fetch would
	// via SetVisitorData. poTokenBinding reads it through GetVisitorData.
	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")

	job := &JobContext{
		Job:    &database.Job{ID: "test-job"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		Formats: []youtube.Format{
			{Itag: 140, URL: "https://example.invalid/videoplayback?id=1"},
		},
	}

	// Mirrors the real caller (strategy_youtube_manifestless_dash.go):
	// resolved once, before the binding-clearing invalidate403Caches step
	// refreshGvsCredentials runs internally.
	binding := poTokenBinding(job, videoInfo)

	_, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, binding, "test")

	if token != "fresh-token" {
		t.Errorf("token = %q, want fresh-token", token)
	}
	if !gotBypass {
		t.Error("re-mint must pass bypassCache=true; a cached token is the one that just failed")
	}
	if gotBinding != "vd-123" {
		t.Errorf("binding = %q, want the visitor data", gotBinding)
	}
}

// TestRefreshGvsCredentialsBindingStableAcrossRefreshes pins the fix for
// cross-call binding drift: invalidate403Caches wipes job.YT's visitor data
// on every call, and nothing repopulates it mid-download. If the binding
// were recomputed from job.YT inside refreshGvsCredentials on every call
// (as an earlier version of this function did), the SECOND refresh —
// whichever downloader's cooldown fires next, video or audio, since both
// share the same job.YT — would silently fall back to the degraded
// channelID/videoID binding while the first refresh minted under the real
// visitorData binding: two live downloaders on the same stream, two
// different binding schemes.
//
// The caller now resolves the binding ONCE (poTokenBinding, at strategy
// setup) and passes it into every refreshGvsCredentials call for the
// lifetime of the download. This test simulates that: it calls
// refreshGvsCredentials twice with one caller-supplied binding — standing
// in for the video downloader's refresh, then the audio downloader's,
// firing seconds later on its own cooldown — and asserts both mints
// observed the identical binding, even though the first call's
// invalidate403Caches has by then cleared the underlying visitor data.
func TestRefreshGvsCredentialsBindingStableAcrossRefreshes(t *testing.T) {
	var gotBindings []string
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			gotBindings = append(gotBindings, binding)
			return "fresh-token", nil
		},
	}

	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")
	job := &JobContext{
		Job:    &database.Job{ID: "test-job"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1"}},
	}

	binding := poTokenBinding(job, videoInfo)
	if binding != "vd-123" {
		t.Fatalf("test setup: binding = %q, want vd-123", binding)
	}

	if _, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, binding, "video"); token != "fresh-token" {
		t.Fatalf("first (video) refresh: token = %q, want fresh-token", token)
	}
	if got := ytSvc.GetVisitorData(); got != "" {
		t.Fatalf("test setup: expected the first refresh's invalidate403Caches to have cleared visitor data, got %q", got)
	}

	if _, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, binding, "audio"); token != "fresh-token" {
		t.Fatalf("second (audio) refresh: token = %q, want fresh-token", token)
	}

	if len(gotBindings) != 2 {
		t.Fatalf("expected 2 mint calls, got %d: %v", len(gotBindings), gotBindings)
	}
	if gotBindings[0] != gotBindings[1] {
		t.Errorf("binding drifted across refreshes: first=%q second=%q", gotBindings[0], gotBindings[1])
	}
	if gotBindings[0] != "vd-123" {
		t.Errorf("binding = %q, want the stable vd-123 resolved before either refresh", gotBindings[0])
	}
}

// TestRefreshGvsCredentialsDegradesOnMintFailure pins the "never returns an
// error" contract: a failing mint must not propagate an error or panic —
// it degrades to an empty token so the engine keeps its existing
// credentials and the download fails the same way it would have without
// this callback.
func TestRefreshGvsCredentialsDegradesOnMintFailure(t *testing.T) {
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			return "", errors.New("mint failed")
		},
	}

	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")
	job := &JobContext{
		Job:    &database.Job{ID: "test-job"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1"}},
	}

	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, "vd-123", "test")
	if token != "" {
		t.Errorf("token = %q, want empty on mint failure", token)
	}
	// No cipher solver is wired (routedSolver and cipherSolver both nil), so
	// the URL half is also expected to come back empty here — this asserts
	// the degrade path never panics and never fabricates a value, not that
	// URL resolution specifically failed for this reason.
	if baseURL != "" {
		t.Errorf("baseURL = %q, want empty (no cipher solver wired)", baseURL)
	}
}

// TestRefreshGvsCredentialsNilProviderSkipsMint exercises the typed-nil
// interface case directly: a genuinely nil *bgutils.PotProvider passed as
// the gvsTokenMinter parameter must not panic when refreshGvsCredentials
// calls minterUsable/asPotProvider on it — boxing a nil concrete pointer
// into an interface produces a non-nil interface value, which is exactly
// the footgun asPotProvider/minterUsable exist to guard against.
func TestRefreshGvsCredentialsNilProviderSkipsMint(t *testing.T) {
	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")
	job := &JobContext{
		Job:    &database.Job{ID: "test-job"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1"}},
	}

	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, nil, "vd-123", "test")
	if token != "" {
		t.Errorf("token = %q, want empty with a nil provider", token)
	}
	if baseURL != "" {
		t.Errorf("baseURL = %q, want empty (no cipher solver wired)", baseURL)
	}
}

// TestCredentialRefreshTimeoutFor pins the clamp: at most 1/3 of the job's
// configured MaxTimeout, never above the credentialRefreshTimeout ceiling.
// Without this clamp, a job running near config.MaximumTimeout's validated
// 30s floor could have a single refresh attempt consume its entire stall
// budget — worse, since the engine doesn't reset lastSegTime around the
// callback, the very next 403 would force-finalize the recording at the
// moment working credentials arrived.
func TestCredentialRefreshTimeoutFor(t *testing.T) {
	cases := []struct {
		name string
		cfg  *JobConfig
		want time.Duration
	}{
		{"nil config falls back to the ceiling", nil, credentialRefreshTimeout},
		{"zero MaximumTimeout falls back to the ceiling", &JobConfig{MaximumTimeout: 0}, credentialRefreshTimeout},
		{"validated floor (30s) clamps to 10s", &JobConfig{MaximumTimeout: 30}, 10 * time.Second},
		{"default (600s) stays at the 45s ceiling — 200s third is above it", &JobConfig{MaximumTimeout: 600}, credentialRefreshTimeout},
		{"90s budget clamps to exactly 30s", &JobConfig{MaximumTimeout: 90}, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialRefreshTimeoutFor(tc.cfg); got != tc.want {
				t.Errorf("credentialRefreshTimeoutFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefreshGvsCredentialsSkipsURLHalfWithoutSolver pins the gating fix:
// with no cipher solver wired, resolveFormatURLByItag can only fail
// (RoutedResolveURL requires a solver even for solver-less passthrough), so
// refreshGvsCredentials must skip the whole URL half — including the
// player-response re-fetch that exists solely to feed it — rather than
// attempting and warning about work that cannot succeed on every single
// refresh. This is asserted indirectly but concretely: job.YT.GetVideoInfo
// against a fabricated PlayerURL would otherwise attempt a real network
// request with no test server behind it; if the gate were missing this
// test would hang or fail slowly instead of returning immediately.
func TestRefreshGvsCredentialsSkipsURLHalfWithoutSolver(t *testing.T) {
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			return "fresh-token", nil
		},
	}

	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")
	job := &JobContext{
		Job:    &database.Job{ID: "test-job", VideoID: "vid1"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
		Formats:   []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1"}},
	}

	start := time.Now()
	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, "vd-123", "test")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("took %v — the URL half (player-response re-fetch) must be skipped without a solver, not attempted over the network", elapsed)
	}
	if baseURL != "" {
		t.Errorf("baseURL = %q, want empty — no solver was wired to resolve it", baseURL)
	}
	if token != "fresh-token" {
		t.Errorf("token = %q, want fresh-token — the mint half must still run without a solver", token)
	}
}
