package cipher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

type firstSendClient struct {
	mu      sync.Mutex
	calls   int
	withJS  int
	first   chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (g *firstSendClient) SolveCipher(ctx context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	g.mu.Lock()
	g.calls++
	if req.PlayerJS != "" {
		g.withJS++
	}
	g.mu.Unlock()
	g.once.Do(func() { close(g.first) })
	<-g.proceed
	res := sidecar.SolveCipherResult{SigResults: map[string]string{}, NResults: map[string]string{}}
	for _, n := range req.NChallenges {
		res.NResults[n] = "dec-" + n
	}
	return res, nil
}

type firstSendSrc struct{ js string }

func (f firstSendSrc) PlayerJS(string) (string, error) { return f.js, nil }
func (f firstSendSrc) RemovePlayerJS(string) error     { return nil }

// Concurrent first solves for one player each attached its ~3 MB JS, and
// each made the sidecar load it — a synchronous pass on its event loop that
// stalls every PO-token request behind it. The first send is gated now: one
// solve ships the JS, the rest wait for it and go without.
//
// Mutant: the claimFirstSend gate removed — all five attach the JS.
func TestConcurrentFirstSolvesShipThePlayerOnce(t *testing.T) {
	g := &firstSendClient{first: make(chan struct{}), proceed: make(chan struct{})}
	s := newSidecarSolverWith(g, firstSendSrc{js: strings.Repeat("x", 3<<20)})
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := range 5 {
		wg.Go(func() {
			got, err := s.N(context.Background(), "p1", fmt.Sprintf("n%d", i))
			if err == nil && got != fmt.Sprintf("dec-n%d", i) {
				err = fmt.Errorf("solve %d returned %q", i, got)
			}
			errs <- err
		})
	}
	<-g.first
	time.Sleep(100 * time.Millisecond) // let the other four reach the gate
	close(g.proceed)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if g.withJS != 1 || g.calls != 5 {
		t.Errorf("%d calls, %d with the player JS — want 5 and 1", g.calls, g.withJS)
	}
}

// A solve waiting on another's first send gives up on its own deadline.
func TestAFirstSendWaiterGivesUpOnItsDeadline(t *testing.T) {
	s := newSidecarSolverWith(&firstSendClient{first: make(chan struct{}), proceed: make(chan struct{})}, firstSendSrc{})
	release, err := s.claimFirstSend(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.N(ctx, "p1", "n"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("N = %v, want the caller's deadline", err)
	}
}
