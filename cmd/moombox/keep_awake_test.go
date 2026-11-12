package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

type keepAwakeTestLog struct{}

func (keepAwakeTestLog) Debug(string, ...any) {}
func (keepAwakeTestLog) Info(string, ...any)  {}
func (keepAwakeTestLog) Warn(string, ...any)  {}
func (keepAwakeTestLog) Error(string, ...any) {}

// newTestKeepAwake returns a controller whose requests to the operating system
// are recorded instead of made.
func newTestKeepAwake(enabled bool) (k *keepAwake, calls *[]bool) {
	calls = new([]bool)
	k = &keepAwake{
		enabled: enabled,
		hold:    func(on bool) bool { *calls = append(*calls, on); return true },
		logger:  keepAwakeTestLog{},
	}
	return k, calls
}

func awakeJob(id string, status database.JobStatus) *database.Job {
	return &database.Job{ID: id, Status: status}
}

// TestKeepAwakeFollowsTheJobs pins owner ruling R2-X5: the host is held awake
// only while some job is capturing or muxing, and released as soon as none
// is, once per change and not once per status write.
//
// Mutants: count jobs up and down instead of keeping the set (the repeated
// Downloading write leaves a count of 2 and the hold is never released); hold
// for Upcoming (the first step asks); forget a deleted row (no release after
// the delete).
func TestKeepAwakeFollowsTheJobs(t *testing.T) {
	k, calls := newTestKeepAwake(true)

	k.jobChanged(awakeJob("a", database.StatusUpcoming))
	k.jobChanged(awakeJob("q", database.StatusQueued))
	if len(*calls) != 0 {
		t.Fatalf("asked the OS %v for jobs that are only waiting", *calls)
	}
	k.jobChanged(awakeJob("a", database.StatusLive))
	k.jobChanged(awakeJob("a", database.StatusDownloading))
	k.jobChanged(awakeJob("a", database.StatusDownloading))
	k.jobChanged(awakeJob("b", database.StatusDownloading))
	k.jobChanged(awakeJob("a", database.StatusMuxing))
	if want := []bool{true}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("while capturing and muxing: OS calls %v, want %v", *calls, want)
	}
	k.jobChanged(awakeJob("a", database.StatusFinished))
	if len(*calls) != 1 {
		t.Fatalf("released with job b still downloading: %v", *calls)
	}
	k.jobChanged(awakeJob("b", database.StatusError))
	if want := []bool{true, false}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("after the last job ended: OS calls %v, want %v", *calls, want)
	}

	k.jobChanged(awakeJob("c", database.StatusMuxing))
	k.jobDeleted("c")
	if want := []bool{true, false, true, false}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("after a muxing row was deleted: OS calls %v, want %v", *calls, want)
	}
}

// TestKeepAwakeSettingAndStop: turning the setting off releases a hold in
// place and turning it on takes one that is due, without a restart; the
// shutdown releases for good.
//
// Mutants: setEnabled stores the flag without applying it (no release, no
// take); stop leaves the hold (the last call stays true); a status write after
// stop takes a new hold.
func TestKeepAwakeSettingAndStop(t *testing.T) {
	k, calls := newTestKeepAwake(false)
	k.jobChanged(awakeJob("a", database.StatusDownloading))
	if len(*calls) != 0 {
		t.Fatalf("asked the OS %v with the setting off", *calls)
	}
	k.setEnabled(true)
	k.setEnabled(false)
	k.setEnabled(true)
	if want := []bool{true, false, true}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("setting on, off, on with a capture running: OS calls %v, want %v", *calls, want)
	}
	k.stop()
	k.jobChanged(awakeJob("b", database.StatusDownloading))
	if want := []bool{true, false, true, false}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("after stop: OS calls %v, want %v", *calls, want)
	}
}

// TestKeepAwakeIsRebuiltFromTheDatabase: the hold is derived from the rows as
// they stand. The boot seed takes one for a row a previous run left
// Downloading; a hold whose job ended without an event is dropped by the
// reconcile; a read that fails changes nothing; and nothing is read while
// nothing is held.
//
// Mutants: seed without reading (no hold at boot); reconcile that keeps the
// old set (the hold outlives its job); reconcile that reads while nothing is
// held (the read count moves).
func TestKeepAwakeIsRebuiltFromTheDatabase(t *testing.T) {
	k, calls := newTestKeepAwake(true)
	rows := []*database.Job{awakeJob("a", database.StatusDownloading), awakeJob("f", database.StatusFinished)}
	var readErr error
	reads := 0
	k.allJobs = func() ([]*database.Job, error) { reads++; return rows, readErr }

	k.seed()
	if want := []bool{true}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("boot seed over a Downloading row: OS calls %v, want %v", *calls, want)
	}

	rows = []*database.Job{awakeJob("a", database.StatusFinished)} // no event reached the controller
	readErr = errors.New("database is locked")
	k.reconcile()
	if len(*calls) != 1 {
		t.Fatalf("a failed read changed the hold: %v", *calls)
	}
	readErr = nil
	k.reconcile()
	if want := []bool{true, false}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("reconcile after the job ended unreported: OS calls %v, want %v", *calls, want)
	}

	before := reads
	k.reconcile()
	if reads != before {
		t.Errorf("reconcile read the jobs %d time(s) with nothing held", reads-before)
	}

	k.rebuild([]*database.Job{awakeJob("m", database.StatusMuxing)})
	k.rebuild(nil)
	if want := []bool{true, false, true, false}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("bulk change lists: OS calls %v, want %v", *calls, want)
	}
}

// TestKeepAwakeIsWiredToTheJobEvents drives the real subscriptions: a status
// write through the database reaches the controller, the setting is applied
// through applyKeepAwake, and a deleted row releases.
//
// Mutants: startKeepAwake without the OnJobChange subscription (no hold when
// the row goes Downloading), or without OnJobDeleted (no release at the end).
func TestKeepAwakeIsWiredToTheJobEvents(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AddJob(&database.Job{ID: "j1", VideoID: "v1", Status: database.StatusUpcoming}); err != nil {
		t.Fatal(err)
	}

	s := &runState{db: db}
	s.startKeepAwake(true)
	var calls []bool
	s.keepAwake.mu.Lock()
	s.keepAwake.hold = func(on bool) bool { calls = append(calls, on); return true }
	s.keepAwake.logger = keepAwakeTestLog{}
	s.keepAwake.mu.Unlock()

	db.UpdateJobFields("j1", map[string]any{"status": database.StatusDownloading})
	db.UpdateJobFields("j1", map[string]any{"progress": "(V: 1/2)"})
	if want := []bool{true}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("after the row went Downloading: OS calls %v, want %v", calls, want)
	}
	s.applyKeepAwake(false)
	s.applyKeepAwake(true)
	if err := db.DeleteJob("j1"); err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, false, true, false}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("after the setting was toggled and the row deleted: OS calls %v, want %v", calls, want)
	}
}
