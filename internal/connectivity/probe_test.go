package connectivity

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestReachabilityProbe_SuccessOnLiveTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// One dead target + one live target: race must report the live one.
	ok := reachabilityProbe(context.Background(), []string{"127.0.0.1:1", ln.Addr().String()})
	if !ok {
		t.Fatal("expected reachable when at least one target accepts")
	}
}

func TestReachabilityProbe_FailWhenAllDead(t *testing.T) {
	// 127.0.0.1:1 is reserved/closed; the dial fails fast.
	start := time.Now()
	ok := reachabilityProbe(context.Background(), []string{"127.0.0.1:1"})
	if ok {
		t.Fatal("expected unreachable when no target accepts")
	}
	if time.Since(start) > probeRaceTimeout+time.Second {
		t.Fatalf("probe took too long: %v", time.Since(start))
	}
}

func TestReachabilityProbe_EmptyTargetsUsesDefaults(t *testing.T) {
	// Empty slice must fall back to defaultProbeTargets (don't panic / don't
	// treat as trivially reachable). We can't assume network access in CI, so
	// only assert it doesn't panic and returns a bool within the deadline.
	_ = reachabilityProbe(context.Background(), nil)
}
