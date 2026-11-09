package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// drained reports whether the scheduler's holds-changed signal is pending,
// and takes it.
func drained(s *Scheduler) bool {
	select {
	case <-s.holdsChanged:
		return true
	default:
		return false
	}
}

// TestSchedulerHoldsReportsEachGate pins Scheduler.Holds (owner ruling R-D3):
// it reports offline, the disk gate, a standing rate-limit hold and each
// row's own hold, as sweep sees them, and each change of them signals
// holdsChanged so the UIs are pushed a new snapshot.
//
// Mutants: Holds reading diskHeld before sweep set it (never true); a hold
// placed without noteHoldsChanged; an expired own hold reported.
func TestSchedulerHoldsReportsEachGate(t *testing.T) {
	s, db, _ := testSchedulerSetup(t, 1)
	now := time.Now()
	if h := s.Holds(now); h.Offline || h.DiskHeld || !h.RateLimitedUntil.IsZero() || len(h.Jobs) != 0 {
		t.Fatalf("a fresh scheduler holds %+v, want nothing", h)
	}

	conn := &fakeConn{}
	s.conn = conn
	if !s.Holds(now).Offline {
		t.Error("offline not reported")
	}
	conn.online.Store(true)

	d := &fakeDisk{used: 99}
	s.readDisk = d.read
	queueBacklog(t, db, "UC_h", "h_a")
	drained(s)
	s.sweep()
	if !s.Holds(now).DiskHeld || !drained(s) {
		t.Error("the disk gate's close was not reported, or not signalled")
	}
	d.used = 50
	s.sweep()
	if s.Holds(now).DiskHeld || !drained(s) {
		t.Error("the disk gate's reopen was not reported, or not signalled")
	}

	s.holdForRateLimit(now, time.Hour)
	if got := s.Holds(now).RateLimitedUntil; !got.Equal(now.Add(time.Hour)) || !drained(s) {
		t.Errorf("rate-limit hold = %v (signalled?), want %v", got, now.Add(time.Hour))
	}
	if got := s.Holds(now.Add(2 * time.Hour)).RateLimitedUntil; !got.IsZero() {
		t.Errorf("a run-out rate-limit hold is reported until %v", got)
	}

	s.holdUntil("h_a", now.Add(10*time.Minute))
	if got := s.Holds(now).Jobs["h_a"]; !got.Until.Equal(now.Add(10*time.Minute)) || !drained(s) {
		t.Errorf("own hold = %+v, want until +10m and a signal", got)
	}
	if _, ok := s.Holds(now.Add(time.Hour)).Jobs["h_a"]; ok {
		t.Error("a run-out own hold is reported")
	}
	s.unhold("h_a")
	if _, ok := s.Holds(now).Jobs["h_a"]; ok || !drained(s) {
		t.Error("unhold did not drop the hold, or did not signal")
	}
}

// TestBacklogRetryStateCountsTheStreak pins the per-row half (owner ruling
// R-D3, K09): a backlog VOD a transient failure sent back to Queued reports
// its place in the retry streak and when it is admitted again, both through
// BacklogRetryState and in AdmissionHolds; the streak's end drops it.
//
// Mutants: AdmissionHolds not filling in the attempt (0 for a counted retry);
// endBacklogStreak not unholding.
func TestBacklogRetryStateCountsTheStreak(t *testing.T) {
	w, db := testWorkerSetup(t)
	ch := "UC_r"
	addSchedJob(t, db, &ch, "r_a", database.StatusUpcoming, 1)
	job, _ := db.GetJob("r_a")
	transient := errors.New("dial tcp: connection refused")
	for range 2 {
		if ok, err := w.requeueBacklog(job, transient, "test requeue"); !ok {
			t.Fatalf("not requeued: %v", err)
		}
	}
	now := time.Now()
	st, ok := w.BacklogRetryState("r_a", now)
	if !ok || st.Attempt != 2 || st.Of != backlogRetryLimit || !st.Until.After(now) {
		t.Fatalf("BacklogRetryState = %+v %v, want attempt 2 of %d, held", st, ok, backlogRetryLimit)
	}
	h := w.AdmissionHolds()
	if got := h.Jobs["r_a"]; got.Attempt != 2 || got.Of != backlogRetryLimit {
		t.Errorf("AdmissionHolds job = %+v, want attempt 2", got)
	}
	row, _ := db.GetJob("r_a")
	if q := h.For(row, now); q.Reason != jobstate.HoldRetry || q.Attempt != 2 {
		t.Errorf("the Queued row's hold = %+v, want retry 2", q)
	}
	w.endBacklogStreak("r_a")
	if _, ok := w.BacklogRetryState("r_a", now); ok {
		t.Error("the streak's end left the row held")
	}
}
