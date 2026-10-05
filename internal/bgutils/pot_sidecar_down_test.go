package bgutils

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// In sidecar mode a mint while the sidecar is down used to fall through to
// the in-process BotGuard pass — which mints no PO token in practice — and
// every request of an outage paid for it: seconds to minutes and three
// Google round trips each, serialised behind one lock. A mint now fails at
// once. An attached handle that was never started is exactly what boot
// leaves when the first start fails.
//
// Mutants: either entry point's unhealthy check removed — the call reaches
// the goja path, which this test's refusing transport turns into a different
// error.
func TestASidecarThatIsDownFailsAMintAtOnce(t *testing.T) {
	prev := bgHTTPClient
	bgHTTPClient = &http.Client{Transport: refusingTransport{}}
	t.Cleanup(func() { bgHTTPClient = prev })

	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	pp.SetSidecar(sidecar.New(sidecar.Config{CacheDir: t.TempDir(), Logger: bypassTestLogger{}}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pp.GeneratePoToken(ctx, "VIDEOID", false); !errors.Is(err, errSidecarDown) {
		t.Errorf("GeneratePoToken = %v, want errSidecarDown", err)
	}
	if _, err := pp.GenerateGvsPoToken(ctx, "VIDEOID", ""); !errors.Is(err, errSidecarDown) {
		t.Errorf("GenerateGvsPoToken = %v, want errSidecarDown", err)
	}
}

// With no sidecar (use_sidecar = false) minter creation is serialised behind
// one lock, and a caller behind it used to wait out the holder's whole
// BotGuard run whatever its own deadline said. It now gives up on time.
//
// Mutant: lockMinterCreation ignoring ctx (a plain blocking send).
func TestAMinterCreationWaiterGivesUpOnItsDeadline(t *testing.T) {
	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	release, err := pp.lockMinterCreation(context.Background()) // another goroutine is creating a minter
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pp.GeneratePoToken(ctx, "VIDEOID", false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the caller's deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the caller waited past its deadline for the creation lock")
	}
}
