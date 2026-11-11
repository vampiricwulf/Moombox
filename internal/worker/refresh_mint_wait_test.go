package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestRefreshGvsCredentialsRunsTheURLHalfAfterAMintThatFailed pins the call
// site's half of the mint grace's short-deadline rule (owner ruling: "don't
// wait when time is short"). The 403 recovery's mint runs under the
// refresh's own deadline, which is at most 45 s
// (credentialRefreshTimeoutFor) and so nearer than the 60 s mint grace: a
// mint that meets a restarting sidecar fails at once
// (bgutils.PotProvider.awaitSidecar), and the recovery then re-fetches the
// player response under that same context and installs the fresh URL. The
// minter here is a fake that fails at once; that the real one does so under
// such a deadline is pinned in internal/bgutils/pot_sidecar_grace_test.go.
//
// Mutants: a mint given no deadline, or one past the mint grace (it would
// wait for the sidecar); a mint given a context of its own that the URL half
// does not share, or a refresh that gives up when the mint fails (the
// re-fetch never runs).
func TestRefreshGvsCredentialsRunsTheURLHalfAfterAMintThatFailed(t *testing.T) {
	const mintGrace = 60 * time.Second // bgutils' sidecarMintGrace
	var mintDeadline time.Time
	fake := &fakePotProvider{generate: func(ctx context.Context, _ string, _ bool) (string, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the mint got a context with no deadline; it would wait for a restarting sidecar")
			return "", errors.New("sidecar down")
		}
		if left := time.Until(dl); left >= mintGrace {
			t.Errorf("the mint's deadline is %v away, not nearer than the %v mint grace; it would wait for a restarting sidecar", left, mintGrace)
		}
		mintDeadline = dl
		return "", errors.New("sidecar down")
	}}

	refetched := false
	orig := refreshVideoInfo
	refreshVideoInfo = func(_ *youtube.Service, ctx context.Context, _ string) (*youtube.VideoInfo, error) {
		refetched = true
		if err := ctx.Err(); err != nil {
			t.Errorf("the player response re-fetch got a dead context (%v) after the mint failed", err)
		}
		if dl, ok := ctx.Deadline(); !ok || !dl.Equal(mintDeadline) {
			t.Errorf("the re-fetch's deadline = %v (set: %v), want the mint's own %v: the two share one budget", dl, ok, mintDeadline)
		}
		return &youtube.VideoInfo{
			PlayerURL:      "https://www.youtube.com/s/player/deadbeef/player.js",
			GvsBinding:     "vd-123",
			GvsBindingKind: youtube.BindingVisitorData,
			Formats:        []youtube.Format{{Itag: 140, URL: "https://fresh.invalid/videoplayback?id=2&n=abc123", Source: "web_creator"}},
		}, nil
	}
	t.Cleanup(func() { refreshVideoInfo = orig })

	solver := &orderedSolver{onN: func(context.Context, string, string) (string, error) { return "decrypted-n", nil }}
	job := &JobContext{Job: &database.Job{ID: "test-job", VideoID: "vid1"}, YT: youtube.NewService(nil, &discardLogger{}), Logger: &discardLogger{}}
	videoInfo := &youtube.VideoInfo{
		PlayerURL: "https://www.youtube.com/s/player/deadbeef/player.js",
		Formats:   []youtube.Format{{Itag: 140, URL: "https://setup.invalid/videoplayback?id=1&n=abc123", Source: "web_creator"}},
	}

	// A parent with no deadline: the mint's deadline is the refresh's own.
	baseURL, token := refreshGvsCredentials(context.Background(), job, videoInfo, 140, solver, nil, fake, "vd-123", "test")

	if !refetched {
		t.Fatal("the player response was not re-fetched after the mint failed; the URL half must still run")
	}
	if token != "" {
		t.Errorf("token = %q, want none: the mint failed", token)
	}
	if baseURL == "" {
		t.Error("no fresh URL was installed; the URL half ran on a live context and should have resolved it")
	}
}
