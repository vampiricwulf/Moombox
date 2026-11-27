package cipher

import (
	"sync"
	"time"
)

// tvKeySuffix is what the tv body's player key carries behind the player id
// in a sidecar request: "<id>-tv". The sidecar keys its loaded players, its
// prepared-player store and its idle eviction by that key alone, so the two
// bodies of one player never stand in for each other there. The suffixed key
// exists only in sidecar requests and in sidecarSolver's sent record: it is
// never handed to a PlayerSource, to CacheKey or to the sig spec store, which
// all know a player by its id. The main body's key is the player id itself.
const tvKeySuffix = "-tv"

// tvPlayerKey is the player key a sidecar request for the player's tv body
// carries.
func tvPlayerKey(playerID string) string { return playerID + tvKeySuffix }

// playerMemorySize bounds playerMemory. Only a player the sidecar failed on
// gets a record, and YouTube serves a handful of players at a time, so the
// bound is never met in practice; the oldest record goes first.
const playerMemorySize = 32

// staleVerdictWindow is how long a failed player is remembered as failed
// (owner ruling RV-11): within it the main body's stale-JS recovery — evict
// every cache layer, refetch about 3 MB, have the sidecar prepare it again —
// runs once per player, not once per solve, and a body ejs threw on while
// preparing it is not sent again. Ten minutes: long enough that a broken
// player costs six recoveries an hour whatever the number of solves, short
// enough that a player whose cached copy really had gone bad (the case the
// recovery exists for) is not kept from a second one for long. A variable so
// a test can read it; production never writes it.
var staleVerdictWindow = 10 * time.Minute

// playerMemory is what the solvers remember about the players ejs could not
// solve with on the main variant. It is keyed by player id, lives in Go
// memory only, and a player's record is never rewritten for another: a new
// player id starts with none, on main.
//
// compositeSolver owns the one that outlives a sidecar restart and hands it
// to each sidecar half it is given (adoptMemory): what is remembered is how
// ejs fares on a player's code, which a new child does not change.
type playerMemory struct {
	mu      sync.Mutex
	players map[string]*playerRecord
	order   []string // insertion order, oldest first
	// now is the clock the verdict window is measured on; a test replaces it.
	now func() time.Time
}

// playerRecord is one player's entry in playerMemory.
type playerRecord struct {
	// useTV is set once ejs has failed on the main body and solved with the
	// tv one: every later solve of the player asks tv alone.
	useTV bool

	// evictedAt is when the main body's stale-JS recovery last ran for this
	// player; no other runs within staleVerdictWindow of it. mainDead is set
	// when that recovery's retry failed while ejs prepared the body, a
	// failure no challenge has a part in: within the window main is not
	// asked at all, and this is the error a solve gets instead.
	evictedAt time.Time
	mainDead  error
	// recoveries counts the recoveries that have finished, so a solve that
	// waited for another's can tell that one ran meanwhile.
	recoveries uint64

	// tvDead is the tv body's counterpart, from tvDeadAt: ejs threw while
	// preparing it, or its fetch got a definite answer that it is not there.
	tvDeadAt time.Time
	tvDead   error

	// warnedGoja is set once the Warn for a solve falling to goja has been
	// logged for this player.
	warnedGoja bool
}

func newPlayerMemory() *playerMemory {
	return &playerMemory{players: make(map[string]*playerRecord), now: time.Now}
}

// record returns playerID's record, making one when create is set. Caller
// holds m.mu.
func (m *playerMemory) record(playerID string, create bool) *playerRecord {
	if r, ok := m.players[playerID]; ok || !create {
		return r
	}
	if len(m.order) >= playerMemorySize {
		delete(m.players, m.order[0])
		m.order = m.order[1:]
	}
	r := &playerRecord{}
	m.players[playerID] = r
	m.order = append(m.order, playerID)
	return r
}

// usesTV reports whether the player's solves go to the tv body.
func (m *playerMemory) usesTV(playerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, false)
	return r != nil && r.useTV
}

// setTV records that the player is solved with the tv body from now on, and
// reports whether this call is the one that recorded it.
func (m *playerMemory) setTV(playerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, true)
	if r.useTV {
		return false
	}
	r.useTV = true
	return true
}

// within reports whether at is inside the verdict window that ends now.
// Caller holds m.mu.
func (m *playerMemory) within(at time.Time) bool {
	return !at.IsZero() && m.now().Sub(at) < staleVerdictWindow
}

// recoveryState is how many stale-JS recoveries of the player's main body
// have finished, and whether the last one to start is within the window
// (held): no other is started while it is.
func (m *playerMemory) recoveryState(playerID string) (finished uint64, held bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, false)
	if r == nil {
		return 0, false
	}
	return r.recoveries, m.within(r.evictedAt)
}

// recoveryDone records that the recovery noteEviction opened has finished,
// however it ended.
func (m *playerMemory) recoveryDone(playerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record(playerID, true).recoveries++
}

// noteEviction records that the main body's stale-JS recovery runs now. What
// an earlier one found (mainDead) no longer stands.
func (m *playerMemory) noteEviction(playerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, true)
	r.evictedAt, r.mainDead = m.now(), nil
}

// cancelEviction takes back the window noteEviction opened, for a recovery
// that did not run to its end: the next solve may start another.
func (m *playerMemory) cancelEviction(playerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.record(playerID, false); r != nil {
		r.evictedAt = time.Time{}
	}
}

// setMainDead records that the recovery noteEviction opened ended with ejs
// unable to prepare the main body.
func (m *playerMemory) setMainDead(playerID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record(playerID, true).mainDead = err
}

// mainDead is the error a solve on the main body gets without the sidecar
// being asked, or nil when main is to be asked.
func (m *playerMemory) mainDead(playerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.record(playerID, false); r != nil && m.within(r.evictedAt) {
		return r.mainDead
	}
	return nil
}

// setTVDead records that the tv body cannot be used, from now.
func (m *playerMemory) setTVDead(playerID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, true)
	r.tvDeadAt, r.tvDead = m.now(), err
}

// tvDead is the error a solve on the tv body gets without the sidecar being
// asked, or nil when tv is to be asked.
func (m *playerMemory) tvDead(playerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.record(playerID, false); r != nil && m.within(r.tvDeadAt) {
		return r.tvDead
	}
	return nil
}

// firstGojaFall reports whether this is the first solve of the player known
// to have fallen to goja, and records it.
func (m *playerMemory) firstGojaFall(playerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.record(playerID, true)
	if r.warnedGoja {
		return false
	}
	r.warnedGoja = true
	return true
}
