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

// TestJobsListReportsHasStagingForTerminalRows is owner decision PE-5: the
// jobs list (GET /api/jobs, plain and paginated) carries hasStaging for a
// terminal row — Error, Cancelled, COOKIES?, Finished — read from the
// staging directory server-side, so the dashboard's row icon can be Resume
// only where there is staging to resume. A row in any other status carries
// no flag. The list carried none at all, so the row offered Resume on every
// resumable row and the server answered "use Reinitialize".
//
// Mutants (each run): sendPaginated sending the bare rows (no hasStaging
// anywhere); ListJob flagging no status (likewise); ListJob flagging every
// status (the Downloading row carries one); HasStaging always true (the
// Cancelled row with nothing staged reads true).
func TestJobsListReportsHasStagingForTerminalRows(t *testing.T) {
	f := newJobsFixture(t)
	statuses := map[string]database.JobStatus{
		"err_staged":   database.StatusError,
		"cancel_empty": database.StatusCancelled,
		"cookies_stg":  database.StatusCookies,
		"fin_staged":   database.StatusFinished,
		"downloading":  database.StatusDownloading,
	}
	for id, st := range statuses {
		f.addJob(t, id, func(j *database.Job) { j.Status = st })
	}
	for _, id := range []string{"err_staged", "cookies_stg", "fin_staged", "downloading"} {
		dir := filepath.Join(f.stagingDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"err_staged": true, "cancel_empty": false, "cookies_stg": true, "fin_staged": true, "downloading": nil}

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
					t.Errorf("%s: hasStaging = %v, want none for a non-terminal row", id, got)
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
// decision PE-5: the dashboard's archived panel (Finished rows older than
// hide_finished_age_days) is built from GET /api/jobs/archived, which sends
// its page through the same sendPaginated as /api/jobs, so an archived row
// carries hasStaging read from the configured staging base too — true over a
// staged file, false over none. The rows are aged by writing updated_at
// straight into the database (AddJob stamps "now"), so the 30-day default
// threshold archives them without a clock.
//
// Mutant (run): the archived route reading staging from "" — every
// archived row reads false, and the staged one is caught.
func TestArchivedJobsListReportsHasStaging(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "old_staged", nil)
	f.addJob(t, "old_empty", nil)
	raw, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.outputDir), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := raw.Exec(`UPDATE jobs SET updated_at = ? WHERE id IN ('old_staged', 'old_empty')`, aged); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.stagingDir, "old_staged")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"old_staged": true, "old_empty": false}

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
			if got, present := row["hasStaging"]; got != want[id] {
				t.Errorf("%s %s: hasStaging = %v (present %v), want %v", path, id, got, present, want[id])
			}
		}
	}
}
