package routes

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChapterImportOnlyIntoACompleteFinishedJob: chapters are imported only
// into a Finished job that is not flagged incomplete_tail (owner decision,
// 2026-11-07). An Error or Cancelled row with an archive, and a Finished one
// missing its tail, can still be resumed, and a resumed run rewrites the
// archive the import would be rewriting, so preview, save and restore all
// refuse them (409) and write nothing; a complete Finished job is accepted.
//
// Mutant: importableJob accepting Error and Cancelled rows again, or ignoring
// IncompleteTail (that row's preview is answered 200).
func TestChapterImportOnlyIntoACompleteFinishedJob(t *testing.T) {
	imp := &fakeChapterImporter{}
	f := newChapterImportFixture(t, imp)
	if err := os.WriteFile(filepath.Join(f.outputDir, "a.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 600
	for id, st := range map[string]database.JobStatus{"yt_err": database.StatusError, "yt_can": database.StatusCancelled, "yt_tail": database.StatusFinished, "yt_ok": database.StatusFinished} {
		f.addJob(t, id, func(j *database.Job) { j.Status = st; j.Filename = "a.mp4"; j.LengthSeconds = &length })
	}
	if f.db.UpdateJobFields("yt_tail", map[string]any{"incomplete_tail": true}) == nil {
		t.Fatal("could not flag incomplete_tail")
	}
	body := map[string]any{"text": "0:01 a"}
	for _, id := range []string{"yt_err", "yt_can", "yt_tail"} {
		for _, path := range []string{"/chapters/preview", "/chapters", "/chapters/restore"} {
			code, _, resp := f.postImport(t, "/api/jobs/"+id+path, body)
			if code != http.StatusConflict || !strings.Contains(resp, "finished, complete archive") {
				t.Errorf("%s%s = %d %s, want 409 naming a finished, complete archive", id, path, code, resp)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(f.outputDir, "a.chapters.json")); !os.IsNotExist(err) {
		t.Error("a refused import wrote the sidecar")
	}
	if code, _, resp := f.postImport(t, "/api/jobs/yt_ok/chapters/preview", body); code != http.StatusOK {
		t.Errorf("preview on a complete Finished job = %d %s, want 200", code, resp)
	}
}
