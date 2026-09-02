package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Arc 10 R4's signalling half. Every credential here is synthetic.
//
// These reuse writeTwitchPair and twitchMarkFixture from
// refresh_twitch_mark_test.go — same package, same fixtures, one definition.

// credentialFires records every OnCredentialsChanged call as "platform" and,
// separately, whether the identity handed over was non-empty. The IDENTITY
// ITSELF is never compared against a literal here and never printed: it is an
// opaque equality token, and a test that hardcoded a digest would also be
// pinning the hash input, which is jar_twitch_identity_test.go's job.
type credentialFires struct {
	platforms  []string
	identities []string
}

func (c *credentialFires) record(platform, identity string) {
	c.platforms = append(c.platforms, platform)
	c.identities = append(c.identities, identity)
}

// TestTwitchCredentialChangeFiresOnCredentialsChanged is the claim.
//
// The mutation: leaving the fire YouTube-only (the shipped state). Under it
// nothing downstream ever learns a Twitch credential changed, so Task 5's
// registry is never broadcast to and a live capture stays anonymous for the
// rest of the job — the exact defect this arc exists to close.
func TestTwitchCredentialChangeFiresOnCredentialsChanged(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background()) // baseline == "" fires once, on purpose
	if len(fires.platforms) != 1 || fires.platforms[0] != "twitch" {
		t.Fatalf("first pass fired %v, want exactly [twitch] — the baseline == \"\" case fires once per process so an offline cookie swap is noticed at all", fires.platforms)
	}
	if fires.identities[0] == "" {
		t.Error("the fire carried an empty identity — the subscriber cannot tell accounts apart")
	}

	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 2 {
		t.Fatalf("fires = %v, want a second one after the auth-token changed", fires.platforms)
	}
	if fires.identities[1] == fires.identities[0] {
		t.Error("the second fire carried the SAME identity as the first — the fingerprint is not being re-read from the reloaded jar")
	}
}

// TestTwitchCredentialsUnchangedFireOnce: the edge filter.
//
// The mutation: dropping advanceIdentityBaseline for Twitch, so the baseline
// stays "" and every 30-minute pass fires. Downstream that drops and
// re-establishes every live IRC session twice an hour, forever, on installs
// where nothing changed.
func TestTwitchCredentialsUnchangedFireOnce(t *testing.T) {
	rs, _ := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())

	if len(fires.platforms) != 1 {
		t.Errorf("fires = %v across three unchanged passes, want exactly one", fires.platforms)
	}
}

// TestADeadTwitchTokenDoesNotConsumeTheChangeEdge is the property
// advanceIdentityBaseline exists for, restated for Twitch.
//
// The sequence is routine: an operator drops in an export that is already
// stale, sees it fail, and re-exports properly. If the failed export moved the
// baseline, the working one compares equal to it and NOTHING fires — the live
// chat session never learns the credentials it has been waiting for arrived.
//
// The mutation: `rs.prevTwitchIdentity = twIdentity` unconditionally.
func TestADeadTwitchTokenDoesNotConsumeTheChangeEdge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchPair(t, path, "test-token-aaaa", "archiveraccount")
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	// A validate server whose answer the test controls between passes. The
	// status is atomic because the handler runs on net/http's connection
	// goroutine while the test writes it from its own — a plain int here is a
	// data race that only -race reports, and the twitch and worker packages in
	// this arc are gated under -race.
	var code atomic.Int32
	code.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(code.Load()))
	}))
	t.Cleanup(srv.Close)
	pointTwitchValidateAt(t, srv)
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(jar, 0, nopLogger{})
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	// Pass 1: the stale export. Conclusive, NOT authenticated.
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 0 {
		t.Fatalf("fires = %v for credentials that do not authenticate, want none", fires.platforms)
	}

	// Pass 2: the same file re-exported properly. Same fingerprint is not the
	// point — what matters is that the edge survived the failed pass.
	code.Store(http.StatusOK)
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 1 || fires.platforms[0] != "twitch" {
		t.Errorf("fires = %v, want [twitch] — the failed pass consumed the edge", fires.platforms)
	}
}

// TestNoTwitchCredentialsNeverFire: an install with no Twitch cookies has a ""
// fingerprint, and "" compares unequal to every real one.
//
// The mutation: firing for "twitch" without consulting shouldObserveCredentials
// at all (or with its !nowAuth guard dropped), which would broadcast a
// credential change to every live chat session on every cycle of a YouTube-only
// install. shouldObserveCredentials' `nowIdentity == ""` guard cannot be
// mutated INTO a failure here on its own — checkTwitchAuth short-circuits to
// (false, nil) with no auth-token, so nowAuth already blocks the fire — and
// that guard is pinned as a pure function by refresh_identity_test.go's table.
func TestNoTwitchCredentialsNeverFire(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.doRefresh(context.Background())
	rs.doRefresh(context.Background())

	if len(fires.platforms) != 0 {
		t.Errorf("fires = %v on a jar with no Twitch credentials, want none", fires.platforms)
	}
}

// TestAStandingTwitchMarkFiresNoCredentialChange is the twEffective half of
// the two call sites this task adds.
//
// A marked platform has NOT authenticated, whatever oauth2/validate answers,
// so neither the fire nor the baseline advance may treat a marked pass as an
// authenticated observation of a working pair.
//
// Two mutations, one per call site:
//   - the fire reading twAuth instead of twEffective: it announces "these
//     credentials work" for the exact pair a chat session just proved does
//     not, and Task 7's broadcast reconnects every live IRC session into the
//     same downgrade.
//   - advanceIdentityBaseline reading twAuth instead of twEffective: the
//     baseline moves onto the marked pair, so the repair that clears the mark
//     is compared against the broken pair rather than the last one that
//     genuinely worked.
func TestAStandingTwitchMarkFiresNoCredentialChange(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	rs.NoteTwitchAuthLoss(twitchLossLoginRefused)
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 0 {
		t.Fatalf("fires = %v while a mark stands — validate's 200 is not an authenticated observation here", fires.platforms)
	}
	if rs.prevTwitchIdentity != "" {
		t.Error("the baseline advanced onto a marked pair — the repair that clears the mark now compares against the broken credentials")
	}

	// The repair. The mark clears on the fingerprint move and validate decides
	// again, so this is the first authenticated observation of the process.
	writeTwitchPair(t, path, "test-token-bbbb", "archiveraccount")
	rs.doRefresh(context.Background())
	if len(fires.platforms) != 1 || fires.platforms[0] != "twitch" {
		t.Errorf("fires = %v after the repair, want [twitch]", fires.platforms)
	}
}

// TestCheckNowObservesATwitchCredentialChange pins the comparison at the
// PUBLIC entry point every reload site has to reach.
//
// Those sites are enumerated in this task's table: POST /api/cookies/recheck
// and the dashboard/Settings browser refresh (both internal/web/routes/
// cookies.go), the TUI's R C and R F (cmd/moombox/tui_wiring.go), the
// automatic recovery re-check (cmd/moombox/monitor_callbacks.go), and — once
// Task 7a lands — the worker's auth-failure refresh, both setup-wizard finish
// paths and the auto-cookie periodic timer. All of them end in CheckNow, which
// is refresh with allowFallback=false, so driving CheckNow drives all of them;
// what the two routes-package sites cannot have is a test that they CALL it
// (see the residual in the table).
//
// The mutation: sampling the Twitch fingerprint BEFORE jar.Reload() rather
// than inside the status block, which would make every one of those four
// gestures report the pre-edit file.
func TestCheckNowObservesATwitchCredentialChange(t *testing.T) {
	rs, path := twitchMarkFixture(t, "test-token-aaaa", "archiveraccount", http.StatusOK)
	var fires credentialFires
	rs.OnCredentialsChanged = fires.record

	if !rs.CheckNow(context.Background()) {
		t.Fatal("the first CheckNow reported that no pass ran")
	}
	writeTwitchPair(t, path, "test-token-bbbb", "otheraccount")
	if !rs.CheckNow(context.Background()) {
		t.Fatal("the second CheckNow reported that no pass ran")
	}

	if len(fires.platforms) != 2 {
		t.Errorf("fires = %v, want two — CheckNow must reload the jar and compare the fingerprint within the same pass", fires.platforms)
	}
}
