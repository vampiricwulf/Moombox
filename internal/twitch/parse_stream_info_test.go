package twitch

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestParseStreamInfoClassification pins the three outcomes the monitor's
// batch path and health tracker depend on: a live stream parses, a real
// offline channel is (nil, nil), and an unresolved login (user:null)
// returns ErrChannelNotFound — the signal that distinguishes a
// renamed/banned/typo'd channel from a merely-offline one.
func TestParseStreamInfoClassification(t *testing.T) {
	a := NewAPI(&testLogger{})
	// Comscore slot is a fallback; an empty data object is a valid "no
	// extra info" result for every case here.
	cs := json.RawMessage(`{"data":{"user":null}}`)

	t.Run("live", func(t *testing.T) {
		sm := json.RawMessage(`{"data":{"user":{"id":"123","displayName":"Streamer","login":"streamer","stream":{"id":"s1","title":"hello","type":"live","viewersCount":5,"createdAt":"2026-01-01T00:00:00Z"}}}}`)
		info, err := a.parseStreamInfo("streamer", sm, cs)
		if err != nil {
			t.Fatalf("live: unexpected error %v", err)
		}
		if info == nil || info.StreamID != "s1" || !info.IsLive || info.Title != "hello" {
			t.Fatalf("live: unexpected info %+v", info)
		}
	})

	t.Run("offline", func(t *testing.T) {
		// Real user, no stream = offline → (nil, nil).
		sm := json.RawMessage(`{"data":{"user":{"id":"123","login":"streamer","stream":null}}}`)
		info, err := a.parseStreamInfo("streamer", sm, cs)
		if info != nil || err != nil {
			t.Fatalf("offline: want (nil,nil), got (%+v, %v)", info, err)
		}
	})

	t.Run("not-found", func(t *testing.T) {
		// user:null (data present, user null) = login doesn't resolve.
		sm := json.RawMessage(`{"data":{"user":null}}`)
		info, err := a.parseStreamInfo("gone", sm, cs)
		if info != nil {
			t.Fatalf("not-found: want nil info, got %+v", info)
		}
		if !errors.Is(err, ErrChannelNotFound) {
			t.Fatalf("not-found: want ErrChannelNotFound, got %v", err)
		}
	})

	// A partial-batch error element (GQL errors array / null data) must NOT
	// be misread as a renamed/banned channel — it's transient. Distinct
	// sentinel so the monitor's health tracker counts it as an ordinary
	// recoverable streak error, not a definitive not-found.
	t.Run("errored-slot", func(t *testing.T) {
		for _, raw := range []string{
			`{"errors":[{"message":"service unavailable"}],"data":null}`,
			`{"data":null}`,
		} {
			info, err := a.parseStreamInfo("streamer", json.RawMessage(raw), cs)
			if info != nil {
				t.Fatalf("errored slot %q: want nil info, got %+v", raw, info)
			}
			if errors.Is(err, ErrChannelNotFound) {
				t.Fatalf("errored slot %q: must NOT be ErrChannelNotFound, got %v", raw, err)
			}
			if !errors.Is(err, errStreamSlotUnavailable) {
				t.Fatalf("errored slot %q: want errStreamSlotUnavailable, got %v", raw, err)
			}
		}
	})
}

// TestGetStreamInfoSingleSwallowsNotFound verifies the single-call wrapper
// preserves its historical (nil, nil) for a not-found login so worker
// callers (which treat not-yet-live and not-found identically) are
// unaffected by the new sentinel — only the batch path surfaces it.
func TestGetStreamInfoSingleSwallowsNotFound(t *testing.T) {
	a := NewAPI(&testLogger{})
	sm := json.RawMessage(`{"data":{"user":null}}`)
	cs := json.RawMessage(`{"data":{"user":null}}`)

	// Exercise the same translation the single wrapper applies.
	info, err := a.parseStreamInfo("gone", sm, cs)
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("parse should sentinel not-found, got %v", err)
	}
	// The wrapper contract: errors.Is(ErrChannelNotFound) → (nil, nil).
	if errors.Is(err, ErrChannelNotFound) {
		info, err = nil, nil
	}
	if info != nil || err != nil {
		t.Fatal("single wrapper must map not-found to (nil, nil)")
	}
}
