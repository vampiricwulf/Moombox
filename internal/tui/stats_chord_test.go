package tui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/stats"
)

func rtOffered(app *App) bool {
	for _, it := range app.buildMenuItems() {
		if it.Chord == "R T" {
			return true
		}
	}
	return false
}

// statsTickPointer is the code pointer every tea.Tick command shares — the
// closure tea.Tick returns is a single func literal, so a command that came
// from one is recognisable without running it. Running a refresh tick would
// mean waiting out statsRefreshInterval, and counting scheduled chains is the
// whole point: the defect this file pins was invisible while the tests threw
// away the command each arm returned. Nothing else in these batches is a
// tea.Tick — the overlay's spinner command (spinnerTickCmd) returns its
// message immediately.
var statsTickPointer = reflect.ValueOf(statsRefreshTick(0)).Pointer()

func isRefreshTick(cmd tea.Cmd) bool {
	return cmd != nil && reflect.ValueOf(cmd).Pointer() == statsTickPointer
}

// drainStats runs cmd — flattening batches, skipping spinner ticks — and
// returns the real messages it produced plus the number of 60 s refresh
// chains it scheduled. Refresh ticks are counted, never run.
func drainStats(cmd tea.Cmd) (msgs []tea.Msg, ticks int) {
	if cmd == nil {
		return nil, 0
	}
	if isRefreshTick(cmd) {
		return nil, 1
	}
	switch msg := cmd().(type) {
	case nil:
		return nil, 0
	case tea.BatchMsg:
		for _, sub := range msg {
			subMsgs, subTicks := drainStats(sub)
			msgs = append(msgs, subMsgs...)
			ticks += subTicks
		}
		return msgs, ticks
	case spinner.TickMsg:
		return nil, 0
	default:
		return []tea.Msg{msg}, 0
	}
}

// applyStatsCmd drains cmd, feeds every message it produced back through
// app.Update and drains what THOSE arms return, the way the Bubble Tea loop
// would, and returns how many refresh chains the whole cascade scheduled.
// Draining the arms' own commands is what makes the count honest: the chain a
// fetch result used to arm was invisible to a test that stopped at the
// message. It terminates because a refresh tick is counted, never run.
func applyStatsCmd(app *App, cmd tea.Cmd) int {
	msgs, ticks := drainStats(cmd)
	for _, msg := range msgs {
		_, next := app.Update(msg)
		ticks += applyStatsCmd(app, next)
	}
	return ticks
}

// statsBatch returns the commands a batch bundled, unexecuted — calling a
// batch command only yields the slice.
func statsBatch(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a batch, got no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected a batch, got %T", msg)
	}
	return batch
}

// TestStatsChordOpensLoadsAndRefreshes: R T exists only with the callback;
// dispatch opens the overlay loading, the fetch fills it, r re-fetches, the
// 60 s tick re-fetches while open and is ignored once closed; a fetch error
// renders in the overlay.
func TestStatsChordOpensLoadsAndRefreshes(t *testing.T) {
	app := NewApp()
	app.OnGetStats = nil
	if rtOffered(app) {
		t.Fatal("R T offered without OnGetStats")
	}
	calls := 0
	app.OnGetStats = func() (stats.Snapshot, error) {
		calls++
		s := sampleSnapshot()
		s.JobCount = 1000 + calls
		return s, nil
	}
	if !rtOffered(app) {
		t.Fatal("R T missing with OnGetStats wired")
	}
	_, cmd := app.dispatchAction("R T", nil)
	if !app.statsDlg.IsVisible() || !strings.Contains(app.statsDlg.View(), "Loading") {
		t.Fatal("R T must open the overlay loading")
	}
	applyStatsCmd(app, cmd) // drains the batch and feeds the messages back through app.Update
	if calls != 1 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,001") {
		t.Fatalf("after open: calls=%d view=%s", calls, app.statsDlg.View())
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	applyStatsCmd(app, cmd)
	if calls != 2 || !strings.Contains(stripANSI(app.statsDlg.View()), "1,002") {
		t.Fatalf("after r: calls=%d", calls)
	}
	_, cmd = app.Update(statsRefreshTickMsg{Epoch: app.statsEpoch})
	applyStatsCmd(app, cmd)
	if calls != 3 {
		t.Fatalf("the refresh tick must re-fetch while open, calls=%d", calls)
	}
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.statsDlg.IsVisible() {
		t.Fatal("esc must close")
	}
	_, cmd = app.Update(statsRefreshTickMsg{Epoch: app.statsEpoch})
	if cmd != nil {
		t.Fatal("a tick after close must not produce a command")
	}
	if calls != 3 {
		t.Fatal("a tick after close must not fetch")
	}
	app.OnGetStats = func() (stats.Snapshot, error) { return stats.Snapshot{}, errors.New("db closed") }
	_, cmd = app.dispatchAction("R T", nil)
	applyStatsCmd(app, cmd)
	if !strings.Contains(app.statsDlg.View(), "db closed") {
		t.Fatal("fetch error must render in the overlay")
	}
}

// TestStatsRefreshChainIsSingular: the 60 s refresh is the Web's setInterval —
// exactly one chain per open, started at R T and re-armed only by its own
// tick. A fetch result never schedules one, r never schedules one, and a tick
// or a snapshot from a retired open is dropped, so r presses and reopens
// cannot leave parallel chains polling for the rest of the session.
func TestStatsRefreshChainIsSingular(t *testing.T) {
	app := NewApp()
	calls := 0
	app.OnGetStats = func() (stats.Snapshot, error) {
		calls++
		s := sampleSnapshot()
		s.JobCount = 1000 + calls
		return s, nil
	}

	_, cmd := app.dispatchAction("R T", nil)
	subs := statsBatch(t, cmd)
	if len(subs) < 2 || !isRefreshTick(subs[len(subs)-1]) {
		t.Fatalf("the refresh tick must be LAST in the R T batch (%d commands) — a consumer that stops at the first real message would otherwise sit out the 60 s timer", len(subs))
	}
	if ticks := applyStatsCmd(app, cmd); ticks != 1 {
		t.Fatalf("R T must start exactly one refresh chain, got %d", ticks)
	}
	openEpoch := app.statsEpoch

	// A fetch result never arms a tick — one per result is what multiplied
	// the chains.
	if _, cmd = app.Update(statsSnapshotMsg{Epoch: openEpoch, Snap: sampleSnapshot()}); cmd != nil {
		t.Fatal("a fetch result must not schedule anything")
	}

	// Three r presses: three fetches, no second chain.
	for i := 1; i <= 3; i++ {
		_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
		if ticks := applyStatsCmd(app, cmd); ticks != 0 {
			t.Fatalf("r press %d started %d refresh chains; the chain belongs to the open", i, ticks)
		}
	}
	if calls != 4 {
		t.Fatalf("r must re-fetch: calls=%d, want 4", calls)
	}

	// The chain's own tick fetches and re-arms itself — exactly once.
	_, cmd = app.Update(statsRefreshTickMsg{Epoch: openEpoch})
	if ticks := applyStatsCmd(app, cmd); ticks != 1 {
		t.Fatalf("the tick must re-arm exactly one chain, got %d", ticks)
	}
	if calls != 5 {
		t.Fatalf("the tick must re-fetch: calls=%d, want 5", calls)
	}

	// Close, then reopen: the previous open's tick and its in-flight fetch
	// are both dead, so neither restarts the polling nor repaints the view.
	app.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.statsEpoch == openEpoch {
		t.Fatal("closing must retire the open's epoch")
	}
	_, cmd = app.dispatchAction("R T", nil)
	applyStatsCmd(app, cmd)
	reopened, view := calls, stripANSI(app.statsDlg.View())

	if _, cmd = app.Update(statsRefreshTickMsg{Epoch: openEpoch}); cmd != nil {
		t.Fatal("a tick from the previous open must be dropped, not re-armed")
	}
	if calls != reopened {
		t.Fatalf("a tick from the previous open must not fetch: calls=%d, want %d", calls, reopened)
	}
	stale := sampleSnapshot()
	stale.JobCount = 424242
	app.Update(statsSnapshotMsg{Epoch: openEpoch, Snap: stale})
	if got := stripANSI(app.statsDlg.View()); got != view {
		t.Fatalf("a fetch result from the previous open must not repaint the overlay:\n%s", got)
	}
}
