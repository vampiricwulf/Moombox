package routes

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestJobsListReportsHasStagingForResumableRows is owner decision PE-5 as
// owner ruling R-B2 narrowed it: the jobs list (GET /api/jobs, plain and
// paginated) carries hasStaging for a row Resume can be offered on — a
// YouTube row in Error, Cancelled or COOKIES?, or Finished with an
// incomplete tail — read from the staging directory server-side, so the
// dashboard's row icon can be Resume only where there is staging to resume.
// Every other row carries no flag: a row in another status, a Finished row
// without an incomplete tail, and a Twitch row, which starts over and never
// resumes. The list carried none at all, so the row offered Resume on every
// resumable row and the server answered "use Reinitialize".
//
// Mutants (each run): sendPaginated sending the bare rows (no hasStaging
// anywhere); ListJob flagging no row (likewise); ListJob flagging every
// status (the Downloading row carries one); ListJob flagging every terminal
// row, as before R-B2 (the plain Finished and the Twitch rows carry one);
// HasStaging always true (the Cancelled row with nothing staged reads true).
func TestJobsListReportsHasStagingForResumableRows(t *testing.T) {
	f := newJobsFixture(t)
	seed := map[string]func(*database.Job){
		"err_staged":   func(j *database.Job) { j.Status = database.StatusError },
		"cancel_empty": func(j *database.Job) { j.Status = database.StatusCancelled },
		"cookies_stg":  func(j *database.Job) { j.Status = database.StatusCookies },
		"tail_staged":  func(j *database.Job) { j.Status = database.StatusFinished },
		"fin_staged":   func(j *database.Job) { j.Status = database.StatusFinished },
		"twitch_err":   func(j *database.Job) { j.Status = database.StatusError; j.Platform = "twitch" },
		"downloading":  func(j *database.Job) { j.Status = database.StatusDownloading },
	}
	for id, mutate := range seed {
		f.addJob(t, id, mutate)
	}
	// AddJob does not write the tail flag; the worker sets it with an update.
	if f.db.UpdateJobFields("tail_staged", map[string]any{"incomplete_tail": true}) == nil {
		t.Fatal("could not flag tail_staged's incomplete tail")
	}
	for _, id := range []string{"err_staged", "cookies_stg", "tail_staged", "fin_staged", "twitch_err", "downloading"} {
		dir := filepath.Join(f.stagingDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{
		"err_staged": true, "cancel_empty": false, "cookies_stg": true, "tail_staged": true,
		"fin_staged": nil, "twitch_err": nil, "downloading": nil,
	}

	check := func(t *testing.T, rows []map[string]any) {
		t.Helper()
		if len(rows) != len(want) {
			t.Fatalf("listed %d rows, want %d", len(rows), len(want))
		}
		for _, row := range rows {
			id, _ := row["id"].(string)
			got, present := row["hasStaging"]
			if w := want[id]; w == nil {
				if present {
					t.Errorf("%s: hasStaging = %v, want none for a row Resume is never offered on", id, got)
				}
			} else if got != w {
				t.Errorf("%s: hasStaging = %v (present %v), want %v", id, got, present, w)
			}
			if row["status"] == nil || row["title"] == nil {
				t.Errorf("%s: the row's own fields are missing: %v", id, row)
			}
		}
	}

	rec := doRequest(t, f.router, "GET", "/api/jobs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var rows []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	check(t, rows)

	rec = doRequest(t, f.router, "GET", "/api/jobs?limit=100", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("paginated list: %d", rec.Code)
	}
	var paged struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&paged); err != nil {
		t.Fatal(err)
	}
	check(t, paged.Data)
}

// TestArchivedJobsListReportsHasStaging is the archived half of owner
// decision PE-5, as owner ruling R-B2 narrowed it: the dashboard's archived
// panel (Finished rows older than hide_finished_age_days) is built from GET
// /api/jobs/archived, which sends its page through the same sendPaginated as
// /api/jobs, so an archived row Resume can be offered on — one with an
// incomplete tail — carries hasStaging read from the configured staging base
// too: true over a staged file, false over none. A plain archived row, which
// is nearly every one, carries no flag and costs no directory read, staged
// or not. The rows are aged by writing updated_at straight into the database
// (AddJob stamps "now"), so the 30-day default threshold archives them
// without a clock.
//
// Mutants (run): the archived route reading staging from "" — every flagged
// archived row reads false, and the staged one is caught; ListJob flagging
// every Finished row — the plain rows carry a flag.
func TestArchivedJobsListReportsHasStaging(t *testing.T) {
	f := newJobsFixture(t)
	for _, id := range []string{"old_staged", "old_empty", "plain_staged", "plain_empty"} {
		f.addJob(t, id, nil)
	}
	// AddJob does not write the tail flag; the worker sets it with an update.
	for _, id := range []string{"old_staged", "old_empty"} {
		if f.db.UpdateJobFields(id, map[string]any{"incomplete_tail": true}) == nil {
			t.Fatalf("could not flag %s's incomplete tail", id)
		}
	}
	raw, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.outputDir), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := raw.Exec(`UPDATE jobs SET updated_at = ?`, aged); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old_staged", "plain_staged"} {
		dir := filepath.Join(f.stagingDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"old_staged": true, "old_empty": false, "plain_staged": nil, "plain_empty": nil}

	for _, path := range []string{"/api/jobs/archived", "/api/jobs/archived?limit=100"} {
		rec := doRequest(t, f.router, "GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		var rows []map[string]any
		if path == "/api/jobs/archived" {
			if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
				t.Fatal(err)
			}
		} else {
			var paged struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&paged); err != nil {
				t.Fatal(err)
			}
			rows = paged.Data
		}
		if len(rows) != len(want) {
			t.Fatalf("%s: listed %d rows, want %d (the aged rows must be archived)", path, len(rows), len(want))
		}
		for _, row := range rows {
			id, _ := row["id"].(string)
			if got, present := row["hasStaging"]; got != want[id] || present != (want[id] != nil) {
				t.Errorf("%s %s: hasStaging = %v (present %v), want %v", path, id, got, present, want[id])
			}
		}
	}
}
