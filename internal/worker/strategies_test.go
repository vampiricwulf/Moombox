package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// orderedSolver is a cipher.Solver double whose N() is caller-controlled —
// unlike job.YT (a concrete *youtube.Service with no stub seam), the
// cipher.Solver interface lets a test make the URL half's cipher call
// arbitrarily slow and ctx-oblivious, standing in for "a deliberately slow
// URL-fetch path" without needing real network access.
type orderedSolver struct {
	onN func(ctx context.Context, playerID, encryptedN string) (string, error)
}

func (s *orderedSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	return "", errors.New("orderedSolver: Sig not implemented")
}

func (s *orderedSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	return s.onN(ctx, playerID, encryptedN)
}

func (s *orderedSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	return nil, nil, errors.New("orderedSolver: Batch not implemented")
}

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
	// via SetVisitorData (refreshGvsCredentials invalidates it internally).
	ytSvc := youtube.NewService(nil, &discardLogger{})
	ytSvc.SetVisitorData("vd-123")

	job := &JobContext{
		Job:    &database.Job{ID: "test-job"},
		YT:     ytSvc,
		Logger: &discardLogger{},
	}
	// GvsBinding stamped the way withAttestation would after a watch-page
	// fetch that resolved visitorData binding under yt-dlp's rule.
	videoInfo := &youtube.VideoInfo{
		GvsBinding:     "vd-123",
		GvsBindingKind: youtube.BindingVisitorData,
		Formats: []youtube.Format{
			{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"},
		},
	}

	// Mirrors the real caller (strategy_youtube_manifestless_dash.go):
	// resolved once, before the binding-clearing invalidate403Caches step
	// refreshGvsCredentials runs internally.
	binding, _ := gvsBinding(job, videoInfo)

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
// The caller now resolves the binding ONCE (gvsBinding, at strategy
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
		GvsBinding:     "vd-123",
		GvsBindingKind: youtube.BindingVisitorData,
		Formats:        []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"}},
	}

	binding, _ := gvsBinding(job, videoInfo)
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
		Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"}},
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
		Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"}},
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
		Formats:   []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator"}},
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

// TestRefreshGvsCredentialsSkipsWholeFileFormat pins the corruption-avoidance
// fix: when the itag being refreshed has, by the time this call resolves,
// become a whole-file format (YouTube finished VOD processing mid-job — a
// real event during the post-live tail this refresh exists to survive),
// installing its URL would feed a &sq=N-addressed segment loop a byte-range
// URL. YouTube ignores &sq on a whole-file URL and returns the ENTIRE file
// for every sequence number — the same disk-filling runaway
// HasManifestlessDashFormats' doc comment describes at strategy-selection
// time; this is the mid-download instance of that same check. The fix must
// detect the ContentLength discriminator and skip the URL install entirely
// (never even call the cipher solver) while still returning the freshly
// minted token.
//
// job.YT is left nil so refreshGvsCredentials resolves directly against
// videoInfo.Formats without a network round trip — standing in for "the
// itag's fresh format," matching how TestRefreshGvsCredentialsSkipsURLHalfWithoutSolver
// avoids the same round trip for its own gate.
func TestRefreshGvsCredentialsSkipsWholeFileFormat(t *testing.T) {
	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			return "fresh-token", nil
		},
	}
	routedSolver := &orderedSolver{
		onN: func(ctx context.Context, playerID, encryptedN string) (string, error) {
			t.Fatal("N() must not be called — the whole-file guard must skip resolution before any cipher call")
			return "", nil
		},
	}

	job := &JobContext{
		Job:    &database.Job{ID: "test-job", VideoID: "vid1"},
		YT:     nil,
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
		Formats: []youtube.Format{
			{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: "web_creator", ContentLength: "123456789"},
		},
	}

	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, routedSolver, nil, fake, "vd-123", "test")

	if baseURL != "" {
		t.Errorf("baseURL = %q, want empty — itag 140 now carries ContentLength (whole-file), never &sq-addressable", baseURL)
	}
	if token != "fresh-token" {
		t.Errorf("token = %q, want fresh-token — the token half must still run even when the URL half is skipped", token)
	}
}

// TestRefreshGvsCredentialsMintsBeforeSlowURLFetch pins the ordering fix:
// the token re-mint must run BEFORE the URL half (player-response re-fetch
// + cipher resolve) on their shared refreshCtx deadline, so a slow URL
// half can never starve the mint of budget. Without this ordering, a tight
// clamped window (credentialRefreshTimeoutFor can go as low as ~10s at the
// 30s MaxTimeout floor) plus an honestly-slow player-response fetch could
// exhaust the whole window before the mint even started, handing
// GeneratePoTokenString an already-expired context — killing the re-mint,
// which is the credential that actually went stale (a stale URL still
// serves until its own expiry; a stale/cached token is the exact thing
// that just earned the 403).
//
// job.YT is deliberately left nil so the (un-fakeable, concrete-typed)
// player-response re-fetch is skipped, isolating the URL half to its one
// controllable piece: cipher resolution via a cipher.Solver double
// (orderedSolver) whose N() is made deliberately slow and ctx-oblivious —
// the worst case for a URL half that would run after the mint. The outer
// ctx is given a short deadline to stand in for a tight clamped budget,
// without re-testing credentialRefreshTimeoutFor's own arithmetic (that's
// TestCredentialRefreshTimeoutFor's job) — context.WithTimeout composes,
// so a short outer deadline is exactly as binding on refreshCtx as a low
// clamp would be.
func TestRefreshGvsCredentialsMintsBeforeSlowURLFetch(t *testing.T) {
	var trace []string
	var mintCtxErr error

	fake := &fakePotProvider{
		generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
			trace = append(trace, "mint")
			mintCtxErr = ctx.Err()
			return "fresh-token", nil
		},
	}
	slowSolver := &orderedSolver{
		onN: func(ctx context.Context, playerID, encryptedN string) (string, error) {
			trace = append(trace, "urlresolve")
			// Ignores ctx entirely and blocks well past the short outer
			// deadline below — the worst-case "honestly slow, doesn't even
			// respect cancellation" URL fetch the ordering fix protects
			// against. If mint ran first, it already returned before this
			// call even started, so this can't have starved it.
			time.Sleep(300 * time.Millisecond)
			return "decrypted-n", nil
		},
	}

	job := &JobContext{
		Job: &database.Job{ID: "test-job", VideoID: "vid1"},
		// job.YT stays nil — see the doc comment above.
		Logger: &discardLogger{},
	}
	videoInfo := &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
		Formats:   []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1&n=abc123", Source: "web_creator"}},
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, token := refreshGvsCredentials(shortCtx, job, videoInfo, 140, slowSolver, nil, fake, "vd-123", "test")

	if token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token — the mint must succeed despite the slow URL half that runs after it", token)
	}
	if mintCtxErr != nil {
		t.Errorf("mint's context was already expired (%v) at call time — the URL half must not run before the mint and starve its budget", mintCtxErr)
	}
	if len(trace) != 2 || trace[0] != "mint" || trace[1] != "urlresolve" {
		t.Errorf("call order = %v, want [mint urlresolve] — the mint must run before the URL half", trace)
	}
}

// TestRefreshGvsCredentialsSkipsTokenFreeStream is the 403-recovery half of
// the live-path policy. The live strategies leave a stream bare at setup
// when its client needs no GVS token upstream (youtube.GvsTokenRequired:
// tv_auth, tv_public, web_embedded, visionos, android_vr*); without this
// gate the first 403 on that stream would re-mint a token here and the
// engine would SetPoToken it onto the very URL the setup gate kept clean.
// The refresh reads the refreshed itag's own client from the setup formats
// — the same format the strategy chose — so its policy cannot drift from
// the setup's. An itag the pool does not name has no recorded client and is
// not minted for either.
//
// Mutant: dropping the gate fails every row (the fake is called and the
// token comes back); gating on the old WebPO-client list instead fails the
// tv_auth, tv_public and web_embedded rows.
func TestRefreshGvsCredentialsSkipsTokenFreeStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		itag   int
	}{
		{name: "tv_auth", source: "tv_auth", itag: 140},
		{name: "tv_public", source: "tv_public", itag: 140},
		{name: "web_embedded", source: "web_embedded", itag: 140},
		{name: "visionos", source: "visionos", itag: 140},
		{name: "android_vr", source: "android_vr", itag: 140},
		{name: "android_vr_dash_fallback", source: "android_vr_dash_fallback", itag: 140},
		{name: "itag not in the pool", source: "web_creator", itag: 251},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var minted int
			fake := &fakePotProvider{
				generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
					minted++
					return "fresh-token", nil
				},
			}
			logs := &captureLogger{}
			job := &JobContext{Job: &database.Job{ID: "test-job"}, Logger: logs}
			videoInfo := &youtube.VideoInfo{
				Formats: []youtube.Format{{Itag: 140, URL: "https://example.invalid/videoplayback?id=1", Source: tc.source}},
			}

			_, token := refreshGvsCredentials(context.Background(), job, videoInfo, tc.itag, nil, nil, fake, "vd-123", "manifestless DASH audio")

			if minted != 0 || token != "" {
				t.Errorf("re-mint ran %d times and returned %q, want no mint and no token for a %s stream", minted, token, tc.name)
			}
			wantSource := tc.source
			if tc.itag != 140 {
				wantSource = ""
			}
			assertSkipLines(t, logs, skipLine(job, wantSource, "tag", "manifestless DASH audio"))
		})
	}
}

// TestRefreshGvsCredentialsKeepsURLOnTokenClassChange pins the refresh's
// install guard (first added by review T3-m1): the refresh decides its
// re-mint by the SETUP format's client but re-runs the full cascade for the
// URL, and the itag's dedup winner can change between the two. When the
// fresh client's GVS token requirement (youtube.GvsTokenRequired) differs
// from the setup client's, installing the fresh URL would break the (URL,
// token) pair the engine holds, so the install is skipped with a Warn whose
// reason names which side requires the token. The rule is symmetric:
//
//   - setup needs no token → fresh requires one (visionos or tv_auth →
//     web_creator): the stream was left bare at setup and the refresh does
//     not mint for it, so the fresh URL would 403 until
//     forbiddenRefreshAttempts runs out.
//   - setup requires one → fresh needs none (web_creator → visionos or
//     tv_auth): the engine keeps the token it was given (the callback cannot
//     clear one) on a URL upstream fetches bare. For a tv URL that is
//     probably harmless, but the class changed, and the guard stays
//     symmetric rather than betting on which bare clients tolerate a stray
//     token — android_vr did not (the 2026-08-15 403s).
//
// A move within one class installs as before: web_creator → web (both
// require), visionos ↔ tv_auth (neither does).
//
// Mutants (each run): making the guard one-directional (skip only when the
// fresh client requires a token the setup lacks) fails the "web_creator to
// tv_auth" and "web_creator to visionos" rows; restoring the old
// WebPO-client two-predicate rule fails "tv_auth to web_creator",
// "web_creator to tv_auth" and "tv_auth to visionos".
func TestRefreshGvsCredentialsKeepsURLOnTokenClassChange(t *testing.T) {
	const warnMsg = "[POT] credential refresh: itag now served by a client of a different token class — keeping the current URL"
	const (
		freshRequires = "fresh client requires the GVS token, setup client does not"
		setupRequires = "setup client requires the GVS token, fresh client does not"
	)
	for _, tc := range []struct {
		name        string
		setup       string
		fresh       string
		wantInstall bool
		wantToken   string
		wantReason  string
	}{
		{name: "visionos to web_creator", setup: "visionos", fresh: "web_creator", wantToken: "", wantReason: freshRequires},
		{name: "tv_auth to web_creator", setup: "tv_auth", fresh: "web_creator", wantToken: "", wantReason: freshRequires},
		{name: "web_creator to visionos", setup: "web_creator", fresh: "visionos", wantToken: "fresh-token", wantReason: setupRequires},
		{name: "web_creator to tv_auth", setup: "web_creator", fresh: "tv_auth", wantToken: "fresh-token", wantReason: setupRequires},
		{name: "web_creator to web", setup: "web_creator", fresh: "web", wantInstall: true, wantToken: "fresh-token"},
		{name: "visionos to tv_auth", setup: "visionos", fresh: "tv_auth", wantInstall: true, wantToken: ""},
		{name: "tv_auth to visionos", setup: "tv_auth", fresh: "visionos", wantInstall: true, wantToken: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := refreshVideoInfo
			refreshVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
				return &youtube.VideoInfo{
					PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
					Formats:   []youtube.Format{{Itag: 140, URL: "https://fresh.invalid/videoplayback?id=2&n=abc123", Source: tc.fresh}},
				}, nil
			}
			t.Cleanup(func() { refreshVideoInfo = orig })

			var minted int
			fake := &fakePotProvider{
				generate: func(ctx context.Context, binding string, bypassCache bool) (string, error) {
					minted++
					return "fresh-token", nil
				},
			}
			solver := &orderedSolver{
				onN: func(ctx context.Context, playerID, encryptedN string) (string, error) {
					return "decrypted-n", nil
				},
			}
			logs := &captureLogger{}
			job := &JobContext{
				Job:    &database.Job{ID: "test-job", VideoID: "vid1"},
				YT:     youtube.NewService(nil, &discardLogger{}),
				Logger: logs,
			}
			videoInfo := &youtube.VideoInfo{
				PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
				Formats:   []youtube.Format{{Itag: 140, URL: "https://setup.invalid/videoplayback?id=1&n=abc123", Source: tc.setup}},
			}

			baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, solver, nil, fake, "vd-123", "test")

			// The token half is decided by the setup source alone, exactly as
			// before the guard.
			if token != tc.wantToken {
				t.Errorf("token = %q, want %q — the token decision follows the setup source", token, tc.wantToken)
			}
			if wantMints := map[bool]int{true: 1, false: 0}[tc.wantToken != ""]; minted != wantMints {
				t.Errorf("minted %d times, want %d", minted, wantMints)
			}

			warns := potLines(logs, warnMsg)
			if tc.wantInstall {
				if !strings.Contains(baseURL, "fresh.invalid") {
					t.Errorf("baseURL = %q, want the fresh %s URL installed", baseURL, tc.fresh)
				}
				if len(warns) != 0 {
					t.Errorf("token-class Warn logged %v, want none for %s → %s", warns, tc.setup, tc.fresh)
				}
				return
			}
			if baseURL != "" {
				t.Errorf("baseURL = %q, want empty — a %s URL must not replace a %s one", baseURL, tc.fresh, tc.setup)
			}
			if len(warns) != 1 {
				t.Fatalf("token-class Warn lines = %v, want exactly one", warns)
			}
			want := map[string]any{"jobID": "test-job", "tag": "test", "itag": 140, "setupSource": tc.setup, "freshSource": tc.fresh, "reason": tc.wantReason}
			for k, v := range want {
				if warns[0][k] != v {
					t.Errorf("Warn %s = %v, want %v (line %v)", k, warns[0][k], v, warns[0])
				}
			}
		})
	}
}
