package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// removalRoutesFixture serves ChannelRemovalRoutes over a real database and
// staging directory, with one channel — an @handle, as the dashboard sends
// it escaped — holding a pending job, a parked capture with footage, a
// download in progress and a finished job.
type removalRoutesFixture struct {
	router  chi.Router
	store   *config.Store
	db      *database.Database
	kicks   int
	channel string
}

func newRemovalRoutesFixture(t *testing.T) *removalRoutesFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "removal.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	ch := "@SomeHandle"
	cfg.Channels = []config.ChannelConfig{{ID: ch, Platform: "youtube"}, {ID: "UCother", Platform: "youtube"}}
	f := &removalRoutesFixture{store: config.NewStore(cfg, filepath.Join(dir, "config.toml")), db: db, channel: ch}

	for _, row := range []struct {
		id     string
		status database.JobStatus
	}{
		{"queued", database.StatusQueued},
		{"parked", database.StatusCookies},
		{"downloading", database.StatusDownloading},
		{"finished", database.StatusFinished},
	} {
		if _, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: "u", Title: row.id,
			Status: row.status, ChannelID: &ch}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddToHistory(row.id); err != nil {
			t.Fatal(err)
		}
	}
	parked := filepath.Join(cfg.Paths.StagingDirectory, "parked")
	if err := os.MkdirAll(parked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parked, "video_00001.ts"), []byte("footage"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.router = chi.NewRouter()
	ChannelRemovalRoutes(f.router, &ChannelRemovalRoutesDeps{DB: db, Store: f.store,
		OnChannelChange: func() { f.kicks++ }, Logger: nopRouteLogger{}})
	return f
}

func (f *removalRoutesFixture) serve(method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func (f *removalRoutesFixture) configured(id string) bool {
	found := false
	f.store.Read(func(c *config.MoomboxConfig) {
		for _, ch := range c.Channels {
			found = found || ch.ID == id
		}
	})
	return found
}

func (f *removalRoutesFixture) jobsLeft(t *testing.T) map[string]bool {
	t.Helper()
	left := map[string]bool{}
	for _, id := range []string{"queued", "parked", "downloading", "finished"} {
		if j, _ := f.db.GetJob(id); j != nil {
			left[id] = true
		}
	}
	return left
}

// TestChannelRemovalSummaryRoute: the confirmation's counts, for an ID the
// dashboard sends escaped.
//
// Mutant killed: the route reading chi.URLParam raw (the escaped ID names no
// job: total 0).
func TestChannelRemovalSummaryRoute(t *testing.T) {
	f := newRemovalRoutesFixture(t)
	rec := f.serve("GET", "/api/config/channels/%40SomeHandle/removal")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET removal: %d %s", rec.Code, rec.Body.String())
	}
	var sum worker.ChannelRemoval
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Total != 4 || sum.Pending != 1 || sum.Active != 1 || len(sum.Footage) != 1 || sum.Footage[0].ID != "parked" {
		t.Errorf("summary = %+v, want total 4, pending 1, active 1, footage [parked]", sum)
	}
}

// TestChannelRemovalKeepsJobsByDefault: a DELETE that names no choice, and
// one that says keep, remove the channel and leave every job (W25-09: the
// sweep used to delete the pending ones on every removal).
//
// Mutant killed: jobs=keep (or no choice) treated as delete.
func TestChannelRemovalKeepsJobsByDefault(t *testing.T) {
	for _, target := range []string{"/api/config/channels/%40SomeHandle", "/api/config/channels/%40SomeHandle?jobs=keep"} {
		t.Run(target, func(t *testing.T) {
			f := newRemovalRoutesFixture(t)
			if rec := f.serve("DELETE", target); rec.Code != http.StatusOK {
				t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
			}
			if f.configured(f.channel) {
				t.Error("the channel is still configured")
			}
			if left := f.jobsLeft(t); len(left) != 4 {
				t.Errorf("jobs left = %v, want all four kept", left)
			}
			if f.kicks != 1 {
				t.Errorf("monitors kicked %d times, want 1", f.kicks)
			}
		})
	}
}

// TestChannelRemovalDeletesPendingJobsOnRequest: jobs=delete removes the
// channel and deletes its pending rows — not the parked capture with
// footage, not the download, not the finished job — and says what it did.
//
// Mutant killed: the jobs query ignored (nothing deleted).
func TestChannelRemovalDeletesPendingJobsOnRequest(t *testing.T) {
	f := newRemovalRoutesFixture(t)
	rec := f.serve("DELETE", "/api/config/channels/%40SomeHandle?jobs=delete")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		JobsDeleted int                    `json:"jobsDeleted"`
		FootageKept []worker.ChannelJobRef `json:"footageKept"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.JobsDeleted != 1 || len(body.FootageKept) != 1 || body.FootageKept[0].ID != "parked" {
		t.Errorf("response = %+v, want 1 deleted and the parked capture kept", body)
	}
	if f.configured(f.channel) {
		t.Error("the channel is still configured")
	}
	left := f.jobsLeft(t)
	if left["queued"] || !left["parked"] || !left["downloading"] || !left["finished"] {
		t.Errorf("jobs left = %v, want parked, downloading and finished", left)
	}
}

// TestChannelRemovalRefusesAnUnknownChoice: a jobs value that is neither
// keep nor delete is refused before anything changes — a typo must not
// fall back to either choice.
//
// Mutant killed: the default arm accepting any value (the channel is
// removed).
func TestChannelRemovalRefusesAnUnknownChoice(t *testing.T) {
	f := newRemovalRoutesFixture(t)
	if rec := f.serve("DELETE", "/api/config/channels/%40SomeHandle?jobs=all"); rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE ?jobs=all: %d, want 400", rec.Code)
	}
	if !f.configured(f.channel) || len(f.jobsLeft(t)) != 4 || f.kicks != 0 {
		t.Error("a refused removal changed something")
	}
}

// TestChannelRemovalOfAnUnknownChannelDeletesNothing: a 404 removal deletes
// no jobs, whatever it asked.
//
// Mutant killed: the pending delete run before the channel lookup.
func TestChannelRemovalOfAnUnknownChannelDeletesNothing(t *testing.T) {
	f := newRemovalRoutesFixture(t)
	ghost := "UCghost"
	if _, err := f.db.AddJob(&database.Job{ID: "ghost_job", VideoID: "ghost_job", URL: "u",
		Status: database.StatusQueued, ChannelID: &ghost}); err != nil {
		t.Fatal(err)
	}
	if rec := f.serve("DELETE", "/api/config/channels/UCghost?jobs=delete"); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown: %d, want 404", rec.Code)
	}
	if j, _ := f.db.GetJob("ghost_job"); j == nil {
		t.Error("a removal that found no channel deleted its jobs")
	}
}
