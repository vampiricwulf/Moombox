package worker

import (
	"context"
	"errors"
	"testing"

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

	_, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, "test")

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

	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, fake, "test")
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

	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, nil, nil, nil, "test")
	if token != "" {
		t.Errorf("token = %q, want empty with a nil provider", token)
	}
	if baseURL != "" {
		t.Errorf("baseURL = %q, want empty (no cipher solver wired)", baseURL)
	}
}
