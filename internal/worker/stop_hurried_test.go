package worker

import (
	"testing"
	"time"
)

// TestStopHurriedCancelsMuxesWhenTheHurryCloses: Windows ends a console
// process about 5 seconds after a console close, a logoff or a system
// shutdown, and Stop spent 10 of them waiting before it cancelled a mux, so
// the mux was killed with the process and nothing after the worker's stop ran.
// A stop that is told to hurry cancels the mux root at once; one that is not
// keeps waiting for the job.
//
// Mutants: drop the hurry case from StopHurried's wait (the stop does not
// return until the 10-second wait is over); have Stop pass a closed channel
// (the unhurried stop returns before its job has finished).
func TestStopHurriedCancelsMuxesWhenTheHurryCloses(t *testing.T) {
	w, _ := testWorkerSetup(t)

	// An in-flight job that ends only when the mux root is cancelled, as a
	// job draining a mux does.
	inFlight := func() (ended chan struct{}) {
		ended = make(chan struct{})
		muxCtx := w.orchestrator.muxRoot()
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			<-muxCtx.Done()
			close(ended)
		}()
		return ended
	}

	ended := inFlight()
	hurry := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		w.StopHurried(hurry)
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("the stop returned with its job still running and no hurry")
	case <-ended:
		t.Fatal("the muxes were cancelled before the hurry and before the in-flight wait was over")
	case <-time.After(300 * time.Millisecond):
	}

	close(hurry)
	select {
	case <-stopped:
	case <-time.After(inFlightWait / 2):
		t.Fatal("the stop was still waiting for its in-flight job long after it was told to hurry")
	}
	select {
	case <-ended:
	default:
		t.Error("the hurried stop returned without cancelling the mux root")
	}
}
