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
	ok, _ := reachabilityProbe(context.Background(), []string{"127.0.0.1:1", ln.Addr().String()}, nil)
	if !ok {
		t.Fatal("expected reachable when at least one target accepts")
	}
}

func TestReachabilityProbe_FailWhenAllDead(t *testing.T) {
	// 127.0.0.1:1 is reserved/closed; the dial fails fast.
	start := time.Now()
	ok, _ := reachabilityProbe(context.Background(), []string{"127.0.0.1:1"}, nil)
	if ok {
		t.Fatal("expected unreachable when no target accepts")
	}
	if time.Since(start) > probeRaceTimeout+time.Second {
		t.Fatalf("probe took too long: %v", time.Since(start))
	}
}

// TestReachabilityProbe_RecoversFromDialPanic: a panicking dial goroutine must
// still deliver its result, or the receive loop waits forever (project rule:
// every goroutine recovers inline). No logger is in scope in this free
// function, so the recover reports "unreachable" silently.
func TestReachabilityProbe_RecoversFromDialPanic(t *testing.T) {
	orig := probeDial
	t.Cleanup(func() { probeDial = orig })
	probeDial = func(context.Context, *net.Dialer, string) (net.Conn, error) { panic("boom") }

	done := make(chan bool, 1)
	go func() {
		ok, _ := reachabilityProbe(context.Background(), []string{"127.0.0.1:1", "127.0.0.1:2"}, []string{"127.0.0.1:3"})
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a panicking dial must not count as reachable")
		}
	case <-time.After(probeRaceTimeout + 2*time.Second):
		t.Fatal("reachabilityProbe hung after a dial panic")
	}
}
