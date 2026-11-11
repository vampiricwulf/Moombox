package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestRefreshGvsCredentialsRunsTheURLHalfAfterAMintThatWaited pins the call
// site's half of the mint grace (owner ruling R-F1): a mint that meets a
// restarting sidecar waits for at most half of what the refresh's context has
// left and then fails with the context still live
// (bgutils.PotProvider.awaitSidecar), and the 403 recovery then re-fetches
// the player response under that same context and installs the fresh URL.
// The minter here is a fake that behaves that way; the wait itself is pinned
// in internal/bgutils/pot_sidecar_grace_test.go.
//
// Mutants: a mint given a context of its own that the URL half does not
// share, or a refresh that gives up when the mint fails (the re-fetch never
// runs); a re-fetch under a context the mint has used up (it sees a dead
// context).
func TestRefreshGvsCredentialsRunsTheURLHalfAfterAMintThatWaited(t *testing.T) {
	var mintDeadline time.Time
	fake := &fakePotProvider{generate: func(ctx context.Context, _ string, _ bool) (string, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the mint got a context with no deadline; the refresh bounds it")
			return "", errors.New("sidecar down")
		}
		mintDeadline = dl
		select {
		case <-time.After(time.Until(dl) / 2):
		case <-ctx.Done():
		}
		return "", errors.New("sidecar down")
	}}

	refetched := false
	orig := refreshVideoInfo
	refreshVideoInfo = func(_ *youtube.Service, ctx context.Context, _ string) (*youtube.VideoInfo, error) {
		refetched = true
		if err := ctx.Err(); err != nil {
			t.Errorf("the player response re-fetch got a dead context (%v) after the mint waited", err)
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

	// The parent's deadline is far shorter than the refresh's own 10 s floor,
	// so the fake's half-wait is a fraction of a second.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	baseURL, token := refreshGvsCredentials(ctx, job, videoInfo, 140, solver, nil, fake, "vd-123", "test")

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
