package routes

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

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
