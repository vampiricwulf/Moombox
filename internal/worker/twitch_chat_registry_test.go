package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Arc 10 R5, worker half. Nothing here touches a credential: the registry's
// whole surface is "which downloaders are live" and "tell all of them".

// fakeChat counts Reauthenticate calls. The registry takes an interface rather
// than *twitch.ChatDownloader precisely so its own behaviour — add, remove,
// broadcast — is testable without a websocket.
//
// reentrant, when set, runs inside Reauthenticate. It is how
// TestBroadcastDoesNotHoldTheRegistryLock reaches back into the registry from
// the middle of a broadcast, which is the only way to observe a lock held
// across the calls.
type fakeChat struct {
	calls     atomic.Int64
	reentrant func()
}

func (f *fakeChat) Reauthenticate() {
	f.calls.Add(1)
	if f.reentrant != nil {
		f.reentrant()
	}
}

// TestRegistryBroadcastsToEveryLiveDownloader.
//
// The mutation: returning after the first entry, or breaking out of the loop.
// A single-entry broadcast passes any "something was told" assertion and
// leaves every concurrent capture but one anonymous — and concurrent Twitch
// captures are the normal case for this application.
func TestRegistryBroadcastsToEveryLiveDownloader(t *testing.T) {
	reg := newTwitchChatRegistry()
	a, b, c := &fakeChat{}, &fakeChat{}, &fakeChat{}
	reg.add(a)
	reg.add(b)
	reg.add(c)

	if got := reg.reauthenticateAll(); got != 3 {
		t.Errorf("reauthenticateAll() = %d, want 3", got)
	}
	for i, f := range []*fakeChat{a, b, c} {
		if n := f.calls.Load(); n != 1 {
			t.Errorf("downloader %d was told %d times, want exactly 1", i, n)
		}
	}
}

// TestRemovedDownloaderIsNotReauthenticated.
//
// The mutation: a remove closure that deletes the wrong key. A finished job's
// downloader would keep being told to reconnect, and Reauthenticate on a
// stopped downloader clears latches a resumed job then inherits.
func TestRemovedDownloaderIsNotReauthenticated(t *testing.T) {
	reg := newTwitchChatRegistry()
	stays, goes := &fakeChat{}, &fakeChat{}
	reg.add(stays)
	removeGoes := reg.add(goes)

	removeGoes()

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d after one removal, want 1", got)
	}
	if n := goes.calls.Load(); n != 0 {
		t.Errorf("the removed downloader was told %d times, want 0", n)
	}
	if n := stays.calls.Load(); n != 1 {
		t.Errorf("the remaining downloader was told %d times, want 1", n)
	}
}

// TestRemoveIsIdempotent. ExecuteTwitch defers the remove; a future edit that
// also removes on an explicit path must not corrupt the registry or panic.
//
// The mutation: a slice-based registry whose second removal panics on, or
// re-slices around, an index it no longer holds — taking the surviving entry
// with it.
func TestRemoveIsIdempotent(t *testing.T) {
	reg := newTwitchChatRegistry()
	stays := &fakeChat{}
	reg.add(stays)
	remove := reg.add(&fakeChat{})

	remove()
	remove()

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d after a double removal, want 1", got)
	}
}

// TestRegistryIsSafeUnderConcurrentJobs. Jobs start and finish while a
// credential change broadcasts; that is the ordinary case, not an edge one.
//
// Two mutations. Dropping the mutex: caught only under -race, which the step
// below runs explicitly — so this test's value is largely in that run, and it
// is listed separately so nobody deletes the -race step believing it is
// redundant. And a slice-index remove that SHIFTS under a concurrent add: the
// final assertion catches that one, because a shifted index removes the wrong
// entry and leaves a live one behind. Neither is reachable from a sequential
// test, which is why neither belongs on TestRemovedDownloaderIsNotReauthenticated.
func TestRegistryIsSafeUnderConcurrentJobs(t *testing.T) {
	reg := newTwitchChatRegistry()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &fakeChat{}
			remove := reg.add(f)
			reg.reauthenticateAll()
			remove()
		}()
	}
	wg.Wait()

	if got := reg.reauthenticateAll(); got != 0 {
		t.Errorf("reauthenticateAll() = %d after every job removed itself, want 0", got)
	}
}

// TestBroadcastDoesNotHoldTheRegistryLock is the claim reauthenticateAll's
// comment makes and that no other test here can make.
//
// A fakeChat that returns immediately is invisible to the mutation that
// matters: holding the registry mutex across the Reauthenticate calls passes
// every other test in this file, with or without -race, and only shows up in
// production as a Twitch job that cannot START while a credential change is
// being broadcast to the jobs already running.
//
// So the fake RE-ENTERS the registry, which is exactly what a starting job
// does. The re-entrant call runs on its own goroutine and is waited for with a
// bounded timeout: under the mutation it blocks on a mutex the broadcast still
// holds, and the wait below reports that rather than hanging until the test
// binary's own timeout.
func TestBroadcastDoesNotHoldTheRegistryLock(t *testing.T) {
	reg := newTwitchChatRegistry()
	registered := make(chan struct{})

	reg.add(&fakeChat{reentrant: func() {
		go func() {
			remove := reg.add(&fakeChat{})
			remove()
			close(registered)
		}()
		select {
		case <-registered:
		case <-time.After(2 * time.Second):
			t.Error("a job registering while a broadcast was in flight blocked — reauthenticateAll is holding the registry lock across Reauthenticate")
		}
	}})

	if got := reg.reauthenticateAll(); got != 1 {
		t.Errorf("reauthenticateAll() = %d, want 1", got)
	}
}

// TestNilRegistryIsInert. DownloadWorker may be constructed in a test or a
// partially initialised process without one; a nil deref at the moment an
// operator repairs their cookies is the worst possible time for one.
//
// The mutation: dropping either nil guard.
func TestNilRegistryIsInert(t *testing.T) {
	var reg *twitchChatRegistry
	reg.add(&fakeChat{})() // add returns a no-op remove; calling it must not panic
	if got := reg.reauthenticateAll(); got != 0 {
		t.Errorf("a nil registry broadcast to %d downloaders", got)
	}
}

// TestReauthenticateTwitchChatsReachesTheRegistry pins the worker's exported
// accessor to the registry rather than to a second mechanism.
//
// The mutation: an accessor that returns 0 unconditionally, or one wired to a
// registry the orchestrator does not share — either leaves cmd/moombox's
// broadcast reaching nothing while every test above still passes.
func TestReauthenticateTwitchChatsReachesTheRegistry(t *testing.T) {
	reg := newTwitchChatRegistry()
	f := &fakeChat{}
	reg.add(f)

	w := &DownloadWorker{twitchChats: reg}
	if got := w.ReauthenticateTwitchChats(); got != 1 {
		t.Errorf("ReauthenticateTwitchChats() = %d, want 1", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("the registered downloader was told %d times, want 1", n)
	}

	var nilWorker *DownloadWorker
	if got := nilWorker.ReauthenticateTwitchChats(); got != 0 {
		t.Errorf("a nil worker broadcast to %d downloaders", got)
	}
	if got := (&DownloadWorker{}).ReauthenticateTwitchChats(); got != 0 {
		t.Errorf("a worker with no registry broadcast to %d downloaders", got)
	}
}

// TestOrchestratorAndWorkerShareOneRegistry is the wiring pin.
//
// ExecuteTwitch registers into the ORCHESTRATOR's registry and cmd/moombox
// broadcasts through the WORKER's. If NewDownloadWorker built two, every test
// above would still pass and the feature would be dead in production — the
// broadcast would reach an always-empty map.
//
// The mutation: `orchestrator: NewDownloadOrchestrator(...)` left with its own
// freshly constructed registry, or a second newTwitchChatRegistry() call.
//
// So this goes through the REAL NewDownloadWorker. Assembling a
// DownloadWorker and a DownloadOrchestrator by hand around one registry and
// then asserting they share it asserts only what the test itself wired, and
// the constructor — the only place the two-registries mistake can be made —
// would never run.
func TestOrchestratorAndWorkerShareOneRegistry(t *testing.T) {
	w, _ := testWorkerSetup(t)

	if w.twitchChats == nil {
		t.Fatal("NewDownloadWorker left the worker with no Twitch chat registry")
	}
	if w.orchestrator == nil || w.orchestrator.twitchChats == nil {
		t.Fatal("NewDownloadWorker left the orchestrator with no Twitch chat registry")
	}
	if w.twitchChats != w.orchestrator.twitchChats {
		t.Fatal("the worker and the orchestrator hold different registries")
	}

	// Registering the way ExecuteTwitch does must be reachable the way
	// cmd/moombox broadcasts.
	f := &fakeChat{}
	remove := w.orchestrator.twitchChats.add(f)
	defer remove()

	if got := w.ReauthenticateTwitchChats(); got != 1 {
		t.Fatalf("a downloader registered through the orchestrator was not reached through the worker (%d told)", got)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("the registered downloader was told %d times, want 1", n)
	}
}
