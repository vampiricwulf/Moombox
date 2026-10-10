package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// TestClientTokenTTLIsEnforcedServerSide: client_token_ttl_days used to set
// only the cookie's Max-Age, which the client is free to ignore, so a
// captured moombox_client value authenticated forever. The server now refuses
// a token older than the TTL and deletes its row.
//
// Mutant: drop the clientTokenExpired check from clientTokenFor — the stale
// token still authenticates and its row survives.
func TestClientTokenTTLIsEnforcedServerSide(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log, err := logger.New(filepath.Join(t.TempDir(), "ttl.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cfg := config.Defaults()
	cfg.Network.ClientTokenTTLDays = 30
	s := &runState{db: db, log: log, configStore: config.NewStore(cfg, "")}

	add := func(id string, age time.Duration) string {
		raw, err := web.GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		hash, err := web.HashToken(raw)
		if err != nil {
			t.Fatal(err)
		}
		created := time.Now().Add(-age).UTC().Format(time.RFC3339)
		if err := db.AddClientToken(&database.ClientToken{
			ID: id, TokenPrefix: web.TokenPrefix(raw), TokenHash: hash, Label: id,
			CreatedAt: created, LastUsedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	fresh := add("fresh", 24*time.Hour)
	stale := add("stale", 31*24*time.Hour)

	if s.clientTokenFor(fresh) == nil {
		t.Error("a 1-day-old token was refused under a 30-day TTL")
	}
	if s.clientTokenFor(stale) != nil {
		t.Error("a 31-day-old token still authenticates under a 30-day TTL")
	}
	tokens, err := db.ListClientTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range tokens {
		if ct.ID == "stale" {
			t.Error("the expired token's row was not removed")
		}
	}
}

func TestClientTokenExpired(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	day := func(d int) string { return now.AddDate(0, 0, -d).Format(time.RFC3339) }
	for _, tc := range []struct {
		created string
		ttl     int
		want    bool
	}{
		{day(29), 30, false},
		{day(31), 30, true},
		{day(364), 0, false}, // 0 = the 365-day default, as the cookie uses
		{day(366), 0, true},
		{"not a time", 30, true},
	} {
		if got := clientTokenExpired(tc.created, tc.ttl, now); got != tc.want {
			t.Errorf("clientTokenExpired(%q, %d) = %v, want %v", tc.created, tc.ttl, got, tc.want)
		}
	}
}
