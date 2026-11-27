package cipher

import "sync"

// playerVariant names which body of a player the sidecar is asked to solve
// with (owner ruling RV-11): the main one first, the tv one when ejs cannot
// solve with main. Goja comes after both, and only ever reads main.
type playerVariant string

const (
	variantMain playerVariant = "main"
	variantTV   playerVariant = "tv"
)

// tvKeySuffix is what the tv body's player key carries behind the player id
// in a sidecar request: "<id>-tv". The sidecar keys its loaded players, its
// prepared-player store and its idle eviction by that key alone, so the two
// bodies of one player never stand in for each other there. The suffixed key
// exists only in sidecar requests and in sidecarSolver's sent record: it is
// never handed to a PlayerSource, to CacheKey or to the sig spec store, which
// all know a player by its id.
const tvKeySuffix = "-tv"

// sidecarPlayerKey is the player key a sidecar request for this variant of
// the player carries.
func sidecarPlayerKey(playerID string, v playerVariant) string {
	if v == variantTV {
		return playerID + tvKeySuffix
	}
	return playerID
}

// playerMemorySize bounds playerMemory. Only a player the sidecar failed on
// gets a record, and YouTube serves a handful of players at a time, so the
// bound is never met in practice; the oldest record goes first.
const playerMemorySize = 32

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
}

// playerRecord is one player's entry in playerMemory.
type playerRecord struct {
	// useTV is set once ejs has failed on the main body and solved with the
	// tv one: every later solve of the player asks tv alone.
	useTV bool
}

func newPlayerMemory() *playerMemory {
	return &playerMemory{players: make(map[string]*playerRecord)}
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
