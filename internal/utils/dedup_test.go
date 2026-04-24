package utils

import (
	"strconv"
	"testing"
)

func TestOrderedDedupAdd(t *testing.T) {
	d := NewOrderedDedup[string]()

	if !d.Add("a") {
		t.Error("first Add should return true")
	}
	if d.Add("a") {
		t.Error("duplicate Add should return false")
	}
	if d.Len() != 1 {
		t.Errorf("Len after dup: want 1, got %d", d.Len())
	}

	d.Add("b")
	d.Add("c")
	if d.Len() != 3 {
		t.Errorf("Len after 3 unique: want 3, got %d", d.Len())
	}

	if !d.Seen("a") || !d.Seen("b") || !d.Seen("c") {
		t.Error("expected all three to be Seen")
	}
	if d.Seen("z") {
		t.Error("expected unseen key to report false")
	}
}

func TestOrderedDedupKeepDropsOldest(t *testing.T) {
	d := NewOrderedDedup[string]()
	for i := range 10 {
		d.Add("msg_" + strconv.Itoa(i))
	}

	d.Keep(4)
	if d.Len() != 4 {
		t.Fatalf("Len after Keep(4): want 4, got %d", d.Len())
	}
	snap := d.Snapshot(0)
	want := []string{"msg_6", "msg_7", "msg_8", "msg_9"}
	for i, w := range want {
		if snap[i] != w {
			t.Errorf("Snapshot[%d]: want %q, got %q", i, w, snap[i])
		}
	}

	// Oldest entries no longer Seen
	if d.Seen("msg_0") || d.Seen("msg_5") {
		t.Error("expected dropped entries to report not Seen")
	}
	// Newest still Seen
	if !d.Seen("msg_9") {
		t.Error("expected newest entry still Seen after Keep")
	}
}

func TestOrderedDedupKeepNoOpWhenUnderLimit(t *testing.T) {
	d := NewOrderedDedup[string]()
	for i := range 5 {
		d.Add("m" + strconv.Itoa(i))
	}
	d.Keep(10) // above current size — must not touch anything
	if d.Len() != 5 {
		t.Errorf("Keep above Len should be a no-op; want 5, got %d", d.Len())
	}
}

func TestOrderedDedupKeepZeroOrNegativeIsNoOp(t *testing.T) {
	d := NewOrderedDedup[string]()
	d.Add("a")
	d.Add("b")

	d.Keep(0)
	if d.Len() != 2 {
		t.Errorf("Keep(0) should be a no-op; want 2, got %d", d.Len())
	}
	d.Keep(-5)
	if d.Len() != 2 {
		t.Errorf("Keep(-5) should be a no-op; want 2, got %d", d.Len())
	}
}

func TestOrderedDedupSnapshotCap(t *testing.T) {
	d := NewOrderedDedup[string]()
	for i := range 6 {
		d.Add("k" + strconv.Itoa(i))
	}

	full := d.Snapshot(0)
	if len(full) != 6 {
		t.Errorf("Snapshot(0) len: want 6, got %d", len(full))
	}

	tail := d.Snapshot(3)
	if len(tail) != 3 || tail[0] != "k3" || tail[2] != "k5" {
		t.Errorf("Snapshot(3) should return last 3: got %v", tail)
	}

	// Snapshot copy must be independent
	tail[0] = "mutated"
	reSnap := d.Snapshot(3)
	if reSnap[0] != "k3" {
		t.Errorf("caller mutation of Snapshot leaked into dedup state")
	}
}

func TestOrderedDedupRestore(t *testing.T) {
	d := NewOrderedDedup[string]()
	d.Add("stale")

	d.Restore([]string{"r1", "r2", "r3"})
	if d.Len() != 3 {
		t.Errorf("after Restore: want 3, got %d", d.Len())
	}
	if d.Seen("stale") {
		t.Error("Restore must replace prior state, not merge")
	}
	if !d.Seen("r1") || !d.Seen("r3") {
		t.Error("Restore did not populate Seen")
	}
}

func TestOrderedDedupRestoreCollapsesDuplicates(t *testing.T) {
	d := NewOrderedDedup[string]()
	d.Restore([]string{"a", "b", "a", "c", "b"})
	if d.Len() != 3 {
		t.Errorf("Restore should collapse dupes; want Len 3, got %d", d.Len())
	}
	snap := d.Snapshot(0)
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if snap[i] != w {
			t.Errorf("Snapshot[%d]: want %q, got %q", i, w, snap[i])
		}
	}
}

func TestOrderedDedupWithCapacityUsable(t *testing.T) {
	d := NewOrderedDedupWithCapacity[string](100)
	d.Add("x")
	if d.Len() != 1 || !d.Seen("x") {
		t.Error("NewOrderedDedupWithCapacity result unusable")
	}
}

// TestOrderedDedupKeepReleasesBackingArray confirms Keep rebuilds the order
// slice on a fresh backing array so the large one is eligible for GC. We
// can't directly assert GC; instead we assert the new cap is far smaller
// than the original, which proves a new allocation happened.
func TestOrderedDedupKeepReleasesBackingArray(t *testing.T) {
	d := NewOrderedDedup[int]()
	for i := range 1000 {
		d.Add(i)
	}
	before := cap(d.order)
	d.Keep(5)
	if cap(d.order) >= before {
		t.Errorf("Keep should allocate a smaller backing array; before=%d after=%d", before, cap(d.order))
	}
	if cap(d.order) > 32 {
		t.Errorf("Keep(5) left backing cap=%d; expected close to 5", cap(d.order))
	}
}
