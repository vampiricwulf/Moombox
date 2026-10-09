package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// deleteOrphans sends DELETE /api/files/orphaned for paths through a router
// carrying FileRoutes beside the jobs fixture's own, and decodes the answer.
func deleteOrphans(t *testing.T, f *jobsFixture, paths ...string) (int, struct {
	Error   string              `json:"error"`
	Stale   int                 `json:"stale"`
	Deleted []string            `json:"deleted"`
	Errors  []map[string]string `json:"errors"`
}) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"paths": paths})
	req := httptest.NewRequest(http.MethodDelete, "/api/files/orphaned", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	var out struct {
		Error   string              `json:"error"`
		Stale   int                 `json:"stale"`
		Deleted []string            `json:"deleted"`
		Errors  []map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("DELETE /api/files/orphaned answered %d with an undecodable body %q: %v", rec.Code, rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	return rec.Code, out
}

// A Delete from a stale Files list — the sweep listed a file, then a row came
// to name it — is refused by the worker as no longer an orphan, and the route
// answers that 409 with a message telling the operator to refresh the list
// (owner decision, W24). The rest of the request is decided path by path, as
// the terminal's Delete All decides it: the genuine orphan beside it goes,
// and the refused path is named in errors with the same message. A request
// with nothing stale keeps its 200, and any other refusal its fixed
// "failed to delete file".
//
// Mutants:
//   - the route without its NotOrphanError arm (every refusal the default
//     one): the answer is 200 and the message is "failed to delete file".
//   - the stale case answered through jsonResponse (200): the status is wrong.
//   - the top-level error left off the 409: the dashboard has nothing to say.
//   - the count message for several stale paths replaced by the first one's.
//   - stale left off the 409, or counting every refusal: the dashboard
//     cannot tell the stale refusals from the paths that failed for another
//     reason, and hid those or miscounted them.
func TestDeleteOrphanedAnswers409ForAPathThatIsNoLongerAnOrphan(t *testing.T) {
	f := newJobsFixture(t)
	FileRoutes(f.router, &FileRoutesDeps{DB: f.db, Store: f.store, Logger: silentLogger{}})
	write := func(rel string) string {
		p := filepath.Join(f.outputDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	archive := write(filepath.Join("Chan", "Stream [stalevid001].mp4"))
	orphan := write(filepath.Join("Chan", "stray.mp4"))
	// The row lands after the operator's list was read.
	f.addJob(t, "stalevid001", func(j *database.Job) { j.OutputFile = archive })

	code, got := deleteOrphans(t, f, archive, orphan)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a path a Finished job names", code)
	}
	want := "No longer an orphan: job stalevid001 names it now. Refresh the list."
	if got.Error != want {
		t.Errorf("error = %q, want %q", got.Error, want)
	}
	if len(got.Deleted) != 1 || got.Deleted[0] != orphan {
		t.Errorf("deleted = %v, want just the genuine orphan %s", got.Deleted, orphan)
	}
	if len(got.Errors) != 1 || got.Errors[0]["path"] != archive || got.Errors[0]["error"] != want {
		t.Errorf("errors = %v, want the archive named with %q", got.Errors, want)
	}
	if got.Stale != 1 {
		t.Errorf("stale = %d, want 1", got.Stale)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the archive its row names is gone: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the genuine orphan beside it is still there (stat err %v)", err)
	}

	// Two stale paths: one message counting them.
	chat := write(filepath.Join("Chan", "Stream [stalevid001].chat.json"))
	if f.db.UpdateJobFields("stalevid001", map[string]any{"chat_file": chat}) == nil {
		t.Fatal("UpdateJobFields(chat_file) failed")
	}
	code, got = deleteOrphans(t, f, archive, chat)
	if code != http.StatusConflict || got.Error != "2 of these are no longer orphans. Refresh the list." || got.Stale != 2 {
		t.Errorf("two stale paths answered %d %q stale=%d, want 409, a count and stale=2", code, got.Error, got.Stale)
	}

	// A stale path beside one refused for another reason (removed by hand
	// since the list was read): errors names both, stale counts only the one.
	gone := filepath.Join(f.outputDir, "Chan", "already-gone.mp4")
	code, got = deleteOrphans(t, f, archive, gone)
	if code != http.StatusConflict || got.Error != want || got.Stale != 1 || len(got.Errors) != 2 {
		t.Errorf("a stale path beside a failed one answered %d %q stale=%d errors=%v, want 409, the stale message, stale=1 and both named",
			code, got.Error, got.Stale, got.Errors)
	}

	// Nothing stale: 200, no top-level error, other refusals as before.
	other := write(filepath.Join("Chan", "other.mp4"))
	outside := filepath.Join(t.TempDir(), "elsewhere.mp4")
	code, got = deleteOrphans(t, f, other, outside)
	if code != http.StatusOK || got.Error != "" {
		t.Errorf("a request with nothing stale answered %d %q, want 200 and no error", code, got.Error)
	}
	if len(got.Errors) != 1 || got.Errors[0]["error"] != "failed to delete file" {
		t.Errorf("errors = %v, want the outside path refused with the fixed message", got.Errors)
	}
}
