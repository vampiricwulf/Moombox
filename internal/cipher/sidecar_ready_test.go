package cipher

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// healthSidecar is fakeSidecar with the IsHealthy a *sidecar.Sidecar has:
// healthy is what it reports, and dieOnCall flips it false inside the next
// call, as a child that dies with the request in flight does.
type healthSidecar struct {
	fakeSidecar
	healthy   atomic.Bool
	dieOnCall bool
}

func (h *healthSidecar) IsHealthy() bool { return h.healthy.Load() }

func (h *healthSidecar) SolveCipher(ctx context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	if h.dieOnCall {
		h.healthy.Store(false)
		h.calls = append(h.calls, req)
		return sidecar.SolveCipherResult{}, errors.New("sidecar: stdout EOF")
	}
	return h.fakeSidecar.SolveCipher(ctx, req)
}

// TestCompositeSidecarReadyAndItsTypedSigFailure is audit G10's cipher half:
// the health accessor the extraction ranks the JS-less client on, and the
// sig failure the worker's challenge-free fallback classifies on. A sidecar
// that was configured but is down — crashed, or waiting on the supervisor's
// restart — used to fail every sig request with its own untyped "sidecar:
// unhealthy", read like one request's failure; it is ErrSidecarUnavailable
// now, as a sidecar never configured always was, and a child that dies with
// the request in flight types its error the same way, keeping the child's
// own error wrapped. SidecarReady is false for each of those and for no
// sidecar at all, and true only for an installed sidecar that is up.
//
// Mutants: sidecarSolver.ready answering true whatever the client says (the
// down rows read ready, and the sig failure is untyped); Sig's pre-call
// readiness check dropped (the down row asks the client); sidecarDown
// returning err as it came (the in-flight rows are untyped); Batch's
// readiness check dropped (the down Batch row asks the client and succeeds);
// Batch's sig failure left untyped (the in-flight Batch row); SidecarReady
// ignoring readiness (the down rows).
func TestCompositeSidecarReadyAndItsTypedSigFailure(t *testing.T) {
	// In a bubble: a request meeting the down sidecar waits out its
	// readiness grace (sidecarReadyGrace, PR-R3) on the bubble's clock.
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		src := &fakePlayerSource{playerID: "p1", js: "JS"}
		if NewCompositeSolver(nil, nil).SidecarReady() {
			t.Error("SidecarReady with no sidecar = true, want false")
		}

		up := &healthSidecar{fakeSidecar: fakeSidecar{respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{SigResults: map[string]string{"x": "SIG"}}, nil
		}}}
		up.healthy.Store(true)
		c := NewCompositeSolver(NewSidecarSolver(up, src), nil)
		if !c.SidecarReady() {
			t.Error("SidecarReady with a sidecar that is up = false, want true")
		}
		if got, err := c.Sig(ctx, "p1", "x"); err != nil || got != "SIG" {
			t.Errorf("Sig with the sidecar up = %q, %v; want SIG", got, err)
		}

		// Down: never asked, and the failure is typed.
		up.healthy.Store(false)
		asked := len(up.calls)
		if c.SidecarReady() {
			t.Error("SidecarReady with the sidecar down = true, want false")
		}
		if _, err := c.Sig(ctx, "p1", "x"); !errors.Is(err, ErrSidecarUnavailable) {
			t.Errorf("Sig with the sidecar down = %v, want ErrSidecarUnavailable", err)
		}
		if len(up.calls) != asked {
			t.Errorf("Sig with the sidecar down asked it %d time(s), want none", len(up.calls)-asked)
		}
		if _, _, err := c.Batch(ctx, "p1", []string{"x"}, nil); !errors.Is(err, ErrSidecarUnavailable) {
			t.Errorf("Batch of a sig with the sidecar down = %v, want ErrSidecarUnavailable", err)
		}

		// Dies with the request in flight: typed, the child's error kept.
		dying := &healthSidecar{dieOnCall: true}
		dying.healthy.Store(true)
		c = NewCompositeSolver(NewSidecarSolver(dying, src), nil)
		_, err := c.Sig(ctx, "p1", "x")
		if !errors.Is(err, ErrSidecarUnavailable) || !strings.Contains(err.Error(), "stdout EOF") {
			t.Errorf("Sig whose sidecar died in flight = %v, want ErrSidecarUnavailable wrapping the child's error", err)
		}
		dying.healthy.Store(true)
		c = NewCompositeSolver(NewSidecarSolver(dying, src), nil)
		if _, _, err := c.Batch(ctx, "p1", []string{"x"}, nil); !errors.Is(err, ErrSidecarUnavailable) || !strings.Contains(err.Error(), "stdout EOF") {
			t.Errorf("Batch of a sig whose sidecar died in flight = %v, want ErrSidecarUnavailable wrapping the child's error", err)
		}

		// A failure while the sidecar stays up is that request's own: untyped.
		flaky := &healthSidecar{fakeSidecar: fakeSidecar{respond: func(sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{}, errors.New("context deadline exceeded")
		}}}
		flaky.healthy.Store(true)
		c = NewCompositeSolver(NewSidecarSolver(flaky, src), nil)
		if _, err := c.Sig(ctx, "p1", "x"); err == nil || errors.Is(err, ErrSidecarUnavailable) {
			t.Errorf("Sig failing with the sidecar up = %v, want its own error, not ErrSidecarUnavailable", err)
		}
	})
}
