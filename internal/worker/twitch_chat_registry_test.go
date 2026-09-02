package worker

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
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
// The mutation: a remove closure that deletes the wrong key. The map would then
// pin a finished job's downloader — its dedup set, pending message buffer and
// cached emote data — for the life of the process, and this daemon runs 24/7.
// Not a latch problem: stream_processor_twitch.go builds a FRESH downloader per
// job execution, so no later job can inherit a stale one's state.
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
// Two mutations, one per half of add's guard, each caught by its own block
// below: dropping `r == nil` (or reauthenticateAll's) panics on the nil
// receiver; dropping `|| cd == nil` stores the nil and panics later, inside the
// broadcast loop.
func TestNilRegistryIsInert(t *testing.T) {
	var reg *twitchChatRegistry
	reg.add(&fakeChat{})() // add returns a no-op remove; calling it must not panic
	if got := reg.reauthenticateAll(); got != 0 {
		t.Errorf("a nil registry broadcast to %d downloaders", got)
	}

	// The other half of add's guard: an unregistered nil must not be broadcast
	// to. Deleting `|| cd == nil` panics in reauthenticateAll and nowhere else.
	live := newTwitchChatRegistry()
	live.add(nil)
	if got := live.reauthenticateAll(); got != 0 {
		t.Errorf("a nil downloader was registered and broadcast to (%d told)", got)
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

// registryLen reports how many downloaders the registry currently holds. Only
// the tests need this number; production asks the registry to act, never to
// describe itself.
func registryLen(r *twitchChatRegistry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// TestExecuteTwitchRegistersTheLiveChatDownloaderForItsWholeRun is the only
// thing that can see the registration site. Deleting the `if irc != nil` block,
// inverting it, or dropping `defer unregisterChat()` are each invisible to
// every other test in this file and to the whole package — the registry would
// stay empty (or leak) forever while every unit test stayed green.
//
// The probe is a synchronous OnJobUpdate subscriber on the Muxing flip, which
// ExecuteTwitch performs after registration and before any defer runs — no
// polling, no timing. A pre-cancelled ctx walks the function end to end without
// touching the network: the notifier is nil, the thumbnail URL is empty, a nil
// FetchVariantsFn skips the quality monitor, the download loop never runs, and
// startChat's goroutine dials an already-dead context.
func TestExecuteTwitchRegistersTheLiveChatDownloaderForItsWholeRun(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator

	job := &database.Job{
		ID: "tw_reg", VideoID: "reg", URL: "https://twitch.tv/x",
		Platform: "twitch", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	var duringRun atomic.Int64
	duringRun.Store(-1)
	unsub := db.OnJobUpdate(func(j *database.Job) {
		if j.ID == job.ID && j.Status == database.StatusMuxing {
			duringRun.Store(int64(registryLen(o.twitchChats)))
		}
	})
	defer unsub()

	// The real concrete type ExecuteTwitch asserts on. The dead ctx means Start
	// never dials. No credential: the options carry no Credentials getter, so
	// the downloader is anonymous by construction.
	cd := twitch.NewChatDownloader(twitch.ChatDownloaderOptions{
		ChannelLogin: "somechannel",
		OutputPath:   filepath.Join(t.TempDir(), "chat.json"),
	}, &discardLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: &discardLogger{}}
	_ = o.ExecuteTwitch(ctx, jobCtx, &TwitchVariantInfo{URL: "http://127.0.0.1:1/x.m3u8", Name: "720p"}, false, cd)

	if got := duringRun.Load(); got != 1 {
		t.Errorf("the live chat downloader was registered on %d entries while ExecuteTwitch ran, want 1", got)
	}
	if got := registryLen(o.twitchChats); got != 0 {
		t.Errorf("the registry still holds %d entries after ExecuteTwitch returned, want 0", got)
	}
}
