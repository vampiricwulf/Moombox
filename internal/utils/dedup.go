package utils

// OrderedDedup tracks whether a key has been seen and preserves insertion
// order for deterministic culling and resume-state snapshots.
//
// Not thread-safe by design — chat downloaders already hold a mutex around
// the per-downloader state this backs. Adding an internal mutex would double
// the lock cost on every message without changing the contract.
//
// The zero value is not usable; construct via NewOrderedDedup.
type OrderedDedup[K comparable] struct {
	seen  map[K]struct{}
	order []K
}

// NewOrderedDedup returns a new dedup with zero entries.
func NewOrderedDedup[K comparable]() *OrderedDedup[K] {
	return &OrderedDedup[K]{seen: make(map[K]struct{})}
}

// NewOrderedDedupWithCapacity returns a new dedup pre-sized for approximately
// the given number of entries — useful when restoring from a resume snapshot.
func NewOrderedDedupWithCapacity[K comparable](capacity int) *OrderedDedup[K] {
	if capacity < 0 {
		capacity = 0
	}
	return &OrderedDedup[K]{
		seen:  make(map[K]struct{}, capacity),
		order: make([]K, 0, capacity),
	}
}

// Add inserts k. Returns true if the key was newly inserted, false if it was
// already present (caller typically uses false as the "skip this message"
// signal in a dedup loop).
func (d *OrderedDedup[K]) Add(k K) bool {
	if _, seen := d.seen[k]; seen {
		return false
	}
	d.seen[k] = struct{}{}
	d.order = append(d.order, k)
	return true
}

// Seen reports whether k was already added. Cheaper than Add when the caller
// only wants to query without inserting.
func (d *OrderedDedup[K]) Seen(k K) bool {
	_, ok := d.seen[k]
	return ok
}

// Len returns the number of unique entries tracked.
func (d *OrderedDedup[K]) Len() int {
	return len(d.order)
}

// Keep retains only the most recently inserted keepN entries. Earlier entries
// are dropped from both the map and the slice. No-op when Len <= keepN or
// keepN <= 0 (a non-positive keepN would wipe everything, which is never what
// a caller wants; use Reset if that's the intent).
func (d *OrderedDedup[K]) Keep(keepN int) {
	if keepN <= 0 || len(d.order) <= keepN {
		return
	}
	drop := d.order[:len(d.order)-keepN]
	for _, k := range drop {
		delete(d.seen, k)
	}
	// Copy to a fresh slice so the backing array of the previous order can be GC'd.
	d.order = append([]K(nil), d.order[len(d.order)-keepN:]...)
}

// Snapshot returns a copy of the insertion-ordered keys, optionally capped to
// the most recent cap entries. cap <= 0 returns all entries. Always returns a
// fresh slice so mutation by the caller does not affect the dedup.
func (d *OrderedDedup[K]) Snapshot(cap int) []K {
	src := d.order
	if cap > 0 && len(src) > cap {
		src = src[len(src)-cap:]
	}
	out := make([]K, len(src))
	copy(out, src)
	return out
}

// Restore replaces the current state with the given keys in order. Duplicate
// keys in the input are silently collapsed (the first occurrence wins for
// order; all map to the same seen entry). Intended for reloading a resume
// snapshot.
func (d *OrderedDedup[K]) Restore(keys []K) {
	d.seen = make(map[K]struct{}, len(keys))
	d.order = make([]K, 0, len(keys))
	for _, k := range keys {
		if _, seen := d.seen[k]; seen {
			continue
		}
		d.seen[k] = struct{}{}
		d.order = append(d.order, k)
	}
}
