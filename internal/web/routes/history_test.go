package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeleteOrphanedHistoryLeavesARowWhoseVideoHasAJob: the route passed any
// ID it was given to the delete, so a Delete All from a list fetched before a
// video was added again took the history row of a job that now exists, and
// the answer claimed every requested ID deleted. Only rows that are still
// orphans go now, and `removed` says how many did.
//
// Mutants: the delete without its orphan condition removes the job's row and
// answers removed 2; the answer without `removed` leaves a script no way to
// see the shortfall.
func TestDeleteOrphanedHistoryLeavesARowWhoseVideoHasAJob(t *testing.T) {
	f := newJobsFixture(t)
	HistoryRoutes(f.router, &HistoryRoutesDeps{DB: f.db, Logger: silentLogger{}})
	for _, id := range []string{"orphanvid01", "rejobbed002"} {
		if err := f.db.AddToHistory(id); err != nil {
			t.Fatal(err)
		}
	}
	// The job lands after the operator's list was read.
	f.addJob(t, "rejobbed002", nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/history/orphaned",
		strings.NewReader(`{"videoIds":["orphanvid01","rejobbed002"]}`))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		Deleted []string `json:"deleted"`
		Removed *int     `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Removed == nil || *got.Removed != 1 {
		t.Errorf("removed = %v, want 1 (body %s)", got.Removed, rec.Body)
	}
	if len(got.Deleted) != 2 {
		t.Errorf("deleted = %v: the 200 shape the dashboard reads must not change", got.Deleted)
	}
	if p, _ := f.db.HasProcessed("rejobbed002"); !p {
		t.Error("the history row of a video that has a job was deleted")
	}
	if p, _ := f.db.HasProcessed("orphanvid01"); p {
		t.Error("the orphaned row was not deleted")
	}
}
