package worker

import "sync"

// twitchChatReauthenticator is the slice of *twitch.ChatDownloader the
// registry needs.
//
// An interface rather than the concrete type, so the registry's own
// behaviour — add, remove, broadcast under concurrency — is testable without a
// websocket server. It is deliberately one method: a registry that could also
// Stop() or MessageCount() would invite callers to reach a running job's
// downloader for things that belong to the job goroutine.
type twitchChatReauthenticator interface {
	Reauthenticate()
}

// twitchChatRegistry holds every live Twitch IRC chat downloader so a
// credential change can reach all of them at once.
//
// It exists because nothing else can reach them. A downloader is built per job
// in processTwitchLive, handed to ExecuteTwitch, and from there lives only
// inside one job goroutine's call stack — so an operator repairing cookies.txt
// during three concurrent Twitch captures had no path to any of them, and each
// job stayed anonymous until it ended.
//
// Keyed by an opaque counter rather than by job ID. The only question this type
// answers is "which downloaders are live"; the entry is removed by the closure
// add returns, so no caller needs a key, and a job ID would invite a second
// question this type has no business answering.
//
// VOD chat is deliberately absent. VodChatDownloader polls GQL and re-reads its
// bearer token per page, so a repaired credential reaches it on its own.
type twitchChatRegistry struct {
	mu      sync.Mutex
	next    uint64
	entries map[uint64]twitchChatReauthenticator
}

func newTwitchChatRegistry() *twitchChatRegistry {
	return &twitchChatRegistry{entries: make(map[uint64]twitchChatReauthenticator)}
}

// add registers one downloader and returns the function that removes it.
//
// The remove-closure shape, rather than a Remove(id) method, is the same one
// database.OnJobUpdate uses and is chosen for the same reason: the caller can
// `defer` it beside the registration and cannot hold the wrong key.
// ExecuteTwitch defers it, so every job exit path — finish, error, user cancel,
// shutdown, connectivity finalize, panic — unregisters, and no exit path has
// to remember to.
//
// Calling the returned function more than once is safe.
func (r *twitchChatRegistry) add(cd twitchChatReauthenticator) (remove func()) {
	if r == nil || cd == nil {
		return func() {}
	}
	r.mu.Lock()
	id := r.next
	r.next++
	r.entries[id] = cd
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.entries, id)
		r.mu.Unlock()
	}
}

// reauthenticateAll tells every live downloader to re-read its credentials and
// reconnect, and returns how many were told.
//
// "Told" is the whole claim. Reauthenticate clears a downloader's latches and,
// only when a session is live, asks it to reconnect — so the count never means
// "re-authenticated", and no caller may report it as such.
//
// The count is a NUMBER and is the only thing that leaves this function.
// Nothing here may surface which channel, which job or which account — the
// caller logs the count and stops.
//
// The snapshot is taken under the lock and the calls are made outside it,
// following StreamProcessor.Stop's convention: Reauthenticate takes the
// downloader's own mutex and cancels a context, and holding the registry lock
// across N of those would put an unrelated job's registration behind them.
func (r *twitchChatRegistry) reauthenticateAll() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	live := make([]twitchChatReauthenticator, 0, len(r.entries))
	for _, cd := range r.entries {
		live = append(live, cd)
	}
	r.mu.Unlock()

	for _, cd := range live {
		cd.Reauthenticate()
	}
	return len(live)
}
