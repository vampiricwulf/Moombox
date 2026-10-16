package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/connectivity"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// outageHarness drives an outageAlerts with a clock the test sets and the
// sidecar tests' fakeClock for the coalescing window, so "the link stayed up
// for two minutes" is one fire() rather than two minutes.
type outageHarness struct {
	a   *outageAlerts
	rec *notificationtest.Recorder
	clk *fakeClock
	now time.Time
}

func newOutageHarness(t *testing.T) *outageHarness {
	t.Helper()
	h := &outageHarness{rec: notificationtest.New(), clk: &fakeClock{}, now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	h.a = newOutageAlerts(h.rec, &nopLogger{}, h.clk.after, func() time.Time { return h.now })
	return h
}

// down and up advance the clock by d and then report the transition.
func (h *outageHarness) down(d time.Duration) { h.now = h.now.Add(d); h.a.onState(false) }
func (h *outageHarness) up(d time.Duration)   { h.now = h.now.Add(d); h.a.onState(true) }

func (h *outageHarness) alerts() int { return len(h.rec.ByEvent("connectivity_restored")) }

// TestShortOutageSendsNoAlert: an outage under connectivity.MinOutage is a
// blip — the engines rode it out and the monitors re-checked on the restore —
// and sends nothing, however long the link then stays up.
//
// Mutant: report sending whatever the time offline.
func TestShortOutageSendsNoAlert(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	h.up(connectivity.MinOutage - time.Second)
	h.clk.last(t).fire()
	if n := h.alerts(); n != 0 {
		t.Fatalf("%d Outage Alerts for a %v outage, want none under the %v minimum", n, connectivity.MinOutage-time.Second, connectivity.MinOutage)
	}
}

// TestOutageReportedOnceTheLinkStaysUp: an outage past the minimum is
// reported, once, when the link has stayed up for the coalescing window — not
// at the restore — with its start, end and time offline.
//
// Mutants: sending at the restore (no window); arming the window with the
// wrong delay; a single outage carrying a Flaps field.
func TestOutageReportedOnceTheLinkStaysUp(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	start := h.now
	h.up(90 * time.Second)
	end := h.now
	if n := h.alerts(); n != 0 {
		t.Fatalf("%d Outage Alerts at the restore itself, want none until the link has stayed up", n)
	}
	if d := h.clk.last(t).d; d != outageCoalesceWindow {
		t.Errorf("coalescing window = %v, want %v", d, outageCoalesceWindow)
	}
	h.clk.last(t).fire()
	calls := h.rec.ByEvent("connectivity_restored")
	if len(calls) != 1 {
		t.Fatalf("%d Outage Alerts, want 1", len(calls))
	}
	c := calls[0]
	for name, want := range map[string]string{
		"Started":  "<t:" + strconv.FormatInt(start.Unix(), 10) + ":f>",
		"Ended":    "<t:" + strconv.FormatInt(end.Unix(), 10) + ":f>",
		"Duration": "1m30s",
	} {
		if got, _ := c.Field(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if v, ok := c.Field("Flaps"); ok {
		t.Errorf("a single outage carries Flaps = %q", v)
	}
}

// TestFlappingLinkCoalescesIntoOneAlert is W27-11: a link that drops two
// polls in three cycled the monitor every ~15 s, and every restore sent an
// Outage Alert. Restores each followed by a loss inside the window are one
// episode, reported once the link settles, with the flap count and the time
// offline summed — and a window armed by an earlier restore that runs out
// while the link is down again reports nothing mid-outage.
//
// Mutants: a restore reporting its own outage (twelve alerts, or a report
// mid-episode); a loss not disarming the window an earlier restore armed (the
// stale window reports the episode while the link is down); the flap count
// or the summed duration wrong.
func TestFlappingLinkCoalescesIntoOneAlert(t *testing.T) {
	h := newOutageHarness(t)
	start := h.now
	for i := range 12 {
		if i > 0 {
			h.down(10 * time.Second)
		} else {
			h.down(0)
		}
		h.up(5 * time.Second)
	}
	// The link drops again and stays down past every window armed so far.
	h.down(10 * time.Second)
	for _, tm := range h.clk.timers {
		tm.fire()
	}
	if n := h.alerts(); n != 0 {
		t.Fatalf("%d Outage Alerts while the link is still flapping, want none until it settles", n)
	}
	h.up(5 * time.Second)
	end := h.now
	h.clk.last(t).fire()

	calls := h.rec.ByEvent("connectivity_restored")
	if len(calls) != 1 {
		t.Fatalf("%d Outage Alerts for one flapping episode, want 1", len(calls))
	}
	c := calls[0]
	for name, want := range map[string]string{
		"Started":  "<t:" + strconv.FormatInt(start.Unix(), 10) + ":f>",
		"Ended":    "<t:" + strconv.FormatInt(end.Unix(), 10) + ":f>",
		"Duration": "1m5s",
		"Flaps":    "13",
	} {
		if got, _ := c.Field(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestFlappingUnderTheMinimumSendsNoAlert: the minimum is on the time offline
// summed over the episode, so a burst of blips that adds up to less than a
// minute is no outage either.
//
// Mutant: report comparing the episode's span (Ended minus Started, 130 s
// here) instead of the time offline summed over it (50 s).
func TestFlappingUnderTheMinimumSendsNoAlert(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	h.up(10 * time.Second)
	for range 4 {
		h.down(20 * time.Second)
		h.up(10 * time.Second)
	}
	h.clk.last(t).fire()
	if n := h.alerts(); n != 0 {
		t.Fatalf("%d Outage Alerts for 50 s offline in all, want none", n)
	}
}

// TestLinkThatNeverSettlesIsStillReported: the window starts again at every
// restore, so without a bound a link flapping all afternoon would never be
// reported; an episode open for outageReportMaxWait is reported at its next
// restore, and the next loss starts a new one.
//
// Mutant: the outageReportMaxWait arm removed (nothing is ever sent).
func TestLinkThatNeverSettlesIsStillReported(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	h.up(10 * time.Second)
	for h.alerts() == 0 && h.now.Sub(time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)) < 2*outageReportMaxWait {
		h.down(5 * time.Second)
		h.up(10 * time.Second)
	}
	if n := h.alerts(); n != 1 {
		t.Fatalf("%d Outage Alerts for a link flapping %v, want 1 once the episode passed %v",
			n, h.now.Sub(time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)), outageReportMaxWait)
	}
	if el := h.now.Sub(time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)); el > outageReportMaxWait+time.Minute {
		t.Errorf("reported after %v, want at the first restore past %v", el, outageReportMaxWait)
	}
	// The next loss opens a fresh episode rather than extending the reported one.
	h.down(5 * time.Second)
	h.up(2 * time.Minute)
	h.clk.last(t).fire()
	if n := h.alerts(); n != 2 {
		t.Errorf("%d Outage Alerts after a fresh two-minute outage, want 2", n)
	}
}

// TestWindowThatEscapedItsStopReportsNothing: time.AfterFunc can enter its
// callback before the loss that stops it, so a window armed by one restore
// can run after the link dropped and came back again — with the next window
// armed. That stale callback must not report the episode early; only the
// current window does, once.
//
// Mutant: fire not checking the epoch it was armed under (the stale callback
// reports the episode the moment it runs, a window before it settles).
func TestWindowThatEscapedItsStopReportsNothing(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	h.up(2 * time.Minute)
	stale := h.clk.last(t)
	h.down(30 * time.Second)
	h.up(time.Minute)
	stale.f() // entered its callback before the loss stopped it
	if n := h.alerts(); n != 0 {
		t.Fatalf("%d Outage Alerts from a window the next loss had disarmed, want none until the current one runs", n)
	}
	h.clk.last(t).fire()
	if n := h.alerts(); n != 1 {
		t.Errorf("%d Outage Alerts once the current window ran, want 1", n)
	}
}

// TestStartupOnlineReportsNothing: the monitor's first word, and a repeat of
// online, end no outage.
func TestStartupOnlineReportsNothing(t *testing.T) {
	h := newOutageHarness(t)
	h.up(0)
	h.up(time.Minute)
	if len(h.clk.timers) != 0 || h.alerts() != 0 {
		t.Fatalf("online with no outage before it armed %d windows and sent %d alerts", len(h.clk.timers), h.alerts())
	}
}

// TestStopReportsAnOutageWaitingOutItsWindow: an outage that ended just before
// a shutdown is reported by the shutdown rather than never; one still running
// is not, and nothing is sent after stop — not even for an outage that, had
// the alerter been running, its restore would have reported on the spot.
//
// Mutants: stop dropping the pending episode instead of reporting it; onState
// not refusing a transition after stop (the post-stop outage, past
// outageReportMaxWait at its restore, is reported).
func TestStopReportsAnOutageWaitingOutItsWindow(t *testing.T) {
	h := newOutageHarness(t)
	h.down(0)
	h.up(2 * time.Minute)
	window := h.clk.last(t)
	h.a.stop()
	if n := h.alerts(); n != 1 {
		t.Fatalf("%d Outage Alerts at shutdown with an ended outage pending, want 1", n)
	}
	window.fire()
	h.down(time.Minute)
	h.up(outageReportMaxWait + time.Minute)
	for _, tm := range h.clk.timers {
		tm.fire()
	}
	if n := h.alerts(); n != 1 {
		t.Errorf("%d Outage Alerts — the alerter sent after it was stopped", n)
	}

	running := newOutageHarness(t)
	running.down(0)
	running.now = running.now.Add(5 * time.Minute)
	running.a.stop()
	if n := running.alerts(); n != 0 {
		t.Errorf("%d Outage Alerts at shutdown during an outage, want none — it has no end yet", n)
	}
}

// TestOutageAlertIsTheDebouncedOne pins the wiring: the connectivity
// subscriber hands every transition to outageAlerts, monitor_callbacks.go no
// longer sends connectivity_restored itself (the per-restore send this
// finding was about), and the shutdown stops the alerter before the
// notification drain, so the episode it reports rides it.
//
// Mutants killed: the closure sending the alert on the restore again; the
// onState call removed; the shutdown stop removed, or moved after Wait.
func TestOutageAlertIsTheDebouncedOne(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "monitor_callbacks.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	handsOver := false
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"connectivity_restored"` {
			t.Errorf("monitor_callbacks.go sends connectivity_restored itself at %s — the Outage Alert is outageAlerts'", fset.Position(lit.Pos()))
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "OnStateChange" && len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.FuncLit); ok {
				ast.Inspect(lit.Body, func(m ast.Node) bool {
					if c, ok := m.(*ast.CallExpr); ok {
						if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "onState" {
							handsOver = true
						}
					}
					return true
				})
			}
		}
		return true
	})
	if !handsOver {
		t.Error("the connectivity subscriber in monitor_callbacks.go no longer hands its transitions to outageAlerts.onState")
	}

	sf, err := parser.ParseFile(fset, "shutdown.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	stopAt, waitAt := token.NoPos, token.NoPos
	ast.Inspect(sf, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.SelectorExpr); ok {
			switch {
			case x.Sel.Name == "outageAlerts" && sel.Sel.Name == "stop" && stopAt == token.NoPos:
				stopAt = sel.Pos()
			case x.Sel.Name == "notifyMgr" && sel.Sel.Name == "Wait" && waitAt == token.NoPos:
				waitAt = sel.Pos()
			}
		}
		return true
	})
	if stopAt == token.NoPos {
		t.Fatal("shutdown.go no longer stops the outage alerter — an outage that ended just before a restart is never reported")
	}
	if waitAt == token.NoPos || stopAt > waitAt {
		t.Errorf("the outage alerter is stopped at %s, after the notification drain at %s — its report would be dropped",
			fset.Position(stopAt), fset.Position(waitAt))
	}
}
