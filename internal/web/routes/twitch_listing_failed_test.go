package routes

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// lockedBuffer is a bytes.Buffer safe for the worker's logger to write from
// any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAddTwitchListingReportsAndLogsWhatCouldNotBeQueued is purpose review
// M9's server side: a pasted listing's answer always carries failed, 0
// included, which the add dialogs show as "F could not be queued", and each
// VOD that could not be queued is logged by ID through the worker's logger.
//
// Mutants: failed back to omitempty (absent from a listing that queued
// every VOD); the route not handing the worker's logger to the queue (the
// owner-less VOD is not named in the log).
func TestAddTwitchListingReportsAndLogsWhatCouldNotBeQueued(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))
	logs := &lockedBuffer{}
	w := worker.NewDownloadWorker(db, nil, cfg, slog.New(slog.NewTextHandler(logs, nil)), nil)
	t.Cleanup(w.Stop)
	rl := web.NewRateLimiter(1000, time.Minute)
	t.Cleanup(rl.Close)

	// A collection whose second node names no owner, and Twitch gave none.
	owner := &struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	}{Login: "monstercat"}
	tw := &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "7", Title: "Part 1", Owner: owner}, {ID: "8", Title: "Part 2"}}}
	r := chi.NewRouter()
	JobRoutes(r, db, store, w, rl, tw, &fakeYouTubeFetcher{}, notificationtest.New())

	rec := postListing(t, r, "https://www.twitch.tv/collections/abc")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res twitchListingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Added != 1 || res.Failed != 1 {
		t.Fatalf("response %s, want 1 added and 1 failed", rec.Body)
	}
	if l := logs.String(); !strings.Contains(l, "could not be queued") || !strings.Contains(l, "vod=8") {
		t.Errorf("the VOD that could not be queued is not logged by ID; log:\n%s", l)
	}

	// Everything queued: failed is still there, as 0.
	tw.videos = []twitch.TwitchVideo{{ID: "9", Title: "Part 3", Owner: owner}}
	rec = postListing(t, r, "https://www.twitch.tv/collections/abc")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["failed"]) != "0" {
		t.Errorf("failed = %q in %s, want 0", raw["failed"], rec.Body)
	}
}
