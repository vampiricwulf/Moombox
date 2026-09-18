package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestNormalizePath(t *testing.T) {
	// Basic test: relative paths resolve to absolute
	result := normalizePath("test.txt")
	if result == "" {
		t.Error("normalizePath should return non-empty result")
	}

	// Case normalization on Windows
	if runtime.GOOS == "windows" {
		a := normalizePath("C:\\Users\\Test\\file.txt")
		b := normalizePath("c:\\users\\test\\file.txt")
		if a != b {
			t.Errorf("normalizePath should be case-insensitive on Windows: %q != %q", a, b)
		}
	}
}

func TestIsUnderDirectory(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		dir      string
		expected bool
	}{
		{
			name:     "path under directory",
			path:     "./output/subdir/file.mp4",
			dir:      "./output",
			expected: true,
		},
		{
			name:     "path is the directory itself",
			path:     "./output",
			dir:      "./output",
			expected: false, // Correctly rejects deleting the root directory
		},
		{
			name:     "path escapes via ..",
			path:     "./output/../secret/file.txt",
			dir:      "./output",
			expected: false,
		},
		{
			name:     "completely unrelated path",
			path:     "./other/file.txt",
			dir:      "./output",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUnderDirectory(tt.path, tt.dir)
			if result != tt.expected {
				t.Errorf("isUnderDirectory(%q, %q) = %v, want %v",
					tt.path, tt.dir, result, tt.expected)
			}
		})
	}
}

// TestDeleteOrphanedFileRefusesActiveJob verifies that DeleteOrphanedFile refuses to
// remove a staging directory whose owning job has become active between scan and
// delete. This is the race that reports/worker.md Finding 18 flagged.
func TestDeleteOrphanedFileRefusesActiveJob(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: stagingDir}}

	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_activeOrphanRaceTest"
	job := &database.Job{
		ID:      jobID,
		VideoID: "activeOrphanRaceTest",
		URL:     "https://youtube.com/watch?v=activeOrphanRaceTest",
		Status:  database.StatusDownloading, // active — must not be deletable
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	jobStagingPath := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(jobStagingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err == nil {
		t.Fatal("expected DeleteOrphanedFile to refuse active job's staging dir, got nil error")
	}

	if _, err := os.Stat(jobStagingPath); err != nil {
		t.Errorf("staging dir should still exist after refused delete: %v", err)
	}

	// Now transition to a terminal state — delete should succeed.
	db.UpdateJobFields(jobID, map[string]any{"status": database.StatusCancelled})
	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err != nil {
		t.Errorf("expected delete to succeed after job transitioned to Cancelled, got %v", err)
	}
	if _, err := os.Stat(jobStagingPath); !os.IsNotExist(err) {
		t.Errorf("staging dir should be gone after successful delete, got err=%v", err)
	}
}

// TestDeleteOrphanedFileRefusesFinishedIncompleteTail verifies that a Finished
// job flagged IncompleteTail is protected the same way an active job is: its
// staging directory + resume sidecar are deliberately preserved (see
// Job.IncompleteTail) so Resume can append the missing tail, and the orphan
// scanner's pre-delete recheck must not let that preserved state be deleted.
// This pins the fix for the data-loss path where findActiveJobForPath's
// activeJobStatuses set omitted StatusFinished entirely.
func TestDeleteOrphanedFileRefusesFinishedIncompleteTail(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: stagingDir}}

	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_incompleteTailOrphanTest"
	job := &database.Job{
		ID:      jobID,
		VideoID: "incompleteTailOrphanTest",
		URL:     "https://youtube.com/watch?v=incompleteTailOrphanTest",
		Status:  database.StatusFinished,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// insertJobExec doesn't carry incomplete_tail — only UpdateJobFields
	// flips it (see database.TestMigrateV17IncompleteTail).
	if updated := db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": true}); updated == nil {
		t.Fatal("UpdateJobFields(incomplete_tail) returned nil")
	}

	jobStagingPath := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(jobStagingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err == nil {
		t.Fatal("expected DeleteOrphanedFile to refuse Finished+IncompleteTail job's staging dir, got nil error")
	}

	if _, err := os.Stat(jobStagingPath); err != nil {
		t.Errorf("staging dir should still exist after refused delete: %v", err)
	}
}

// TestIncompleteStagingExpired pins the expiry rule (owner ruling
// 2026-08-21): only the disk-heavy staging shield expires — never the flag —
// and every ambiguous input errs toward preservation.
func TestIncompleteStagingExpired(t *testing.T) {
	cfgDays := func(d float64) *config.MoomboxConfig {
		return &config.MoomboxConfig{Downloader: config.DownloaderConfig{
			IncompleteStagingExpiryDays: config.FlexDuration{Value: d},
		}}
	}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name string
		cfg  *config.MoomboxConfig
		upd  string
		want bool
	}{
		{"nil cfg preserves", nil, old, false},
		{"zero days preserves forever", cfgDays(0), old, false},
		{"fresh job preserved", cfgDays(7), fresh, false},
		{"aged job expired", cfgDays(7), old, true},
		{"unparseable timestamp preserves", cfgDays(7), "not-a-time", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := incompleteStagingExpired(c.cfg, &database.Job{UpdatedAt: c.upd})
			if got != c.want {
				t.Errorf("incompleteStagingExpired = %v, want %v", got, c.want)
			}
		})
	}
}

// TestJobNeedsStagingExpiryGate: an aged incomplete-tail job's staging is no
// longer shielded (becomes an ordinary orphan candidate), while a fresh one
// stays protected — and the unmuxed-parts shield is independent of expiry.
func TestJobNeedsStagingExpiryGate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &config.MoomboxConfig{Downloader: config.DownloaderConfig{
		IncompleteStagingExpiryDays: config.FlexDuration{Value: 7},
	}}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	stagingDir := filepath.Join(dir, "staging", "j1")

	flagged := func(upd string) *database.Job {
		return &database.Job{ID: "j1", Status: database.StatusFinished, IncompleteTail: true, UpdatedAt: upd}
	}
	if !jobNeedsStaging(db, cfg, flagged(fresh), stagingDir) {
		t.Error("fresh incomplete-tail job must stay shielded")
	}
	if jobNeedsStaging(db, cfg, flagged(old), stagingDir) {
		t.Error("aged incomplete-tail job must no longer be shielded")
	}
}

// TestOrphanSweepShieldsAnAsideOnlyStagingDir pins extra item (b): the sweep
// learns engine.IsStagedRestartPath. A staging dir whose only remaining media
// is a set-aside recording holds captured footage no mux has consumed, so it
// is not an orphan to delete — but unlike the unmuxed-PART shield it defers to
// the same age rule the other two incomplete shields use, because finalize now
// muxes every readable aside to its own sibling file: one still sitting here
// days later is one FFmpeg could not read, not footage awaiting a routine
// recovery. When the dir IS offered, the sweep names the aside so the operator
// can see what the deletion would take.
//
// Mutants, one per arm: dropping the aside term from jobNeedsStaging (the
// sweep offers a live aside-only dir for deletion); making the aside shield
// unconditional (an aged dir is pinned forever); dropping the Asides field
// (the offer says only "staging", with no hint that it holds a recording).
func TestOrphanSweepShieldsAnAsideOnlyStagingDir(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stagingRoot := filepath.Join(dir, "staging")
	cfg := &config.MoomboxConfig{Downloader: config.DownloaderConfig{
		IncompleteStagingExpiryDays: config.FlexDuration{Value: 7},
	}}
	cfg.Paths.StagingDirectory = stagingRoot

	asideIn := func(jobID string) (string, string) {
		jobDir := filepath.Join(stagingRoot, jobID)
		if err := os.MkdirAll(jobDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", jobDir, err)
		}
		name := "video.mp4.restart-1700000000"
		if err := os.WriteFile(filepath.Join(jobDir, name), []byte("set aside"), 0o644); err != nil {
			t.Fatalf("write the aside: %v", err)
		}
		return jobDir, name
	}

	liveDir, asideName := asideIn("j-shield")
	if _, err := db.AddJob(&database.Job{ID: "j-shield", VideoID: "j-shield", Status: database.StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// A dir with no job row at all — the ordinary "left by a removed job"
	// orphan, which the sweep still offers (and must name).
	goneDir, _ := asideIn("j-gone")

	entries, err := scanStagingOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanStagingOrphans: %v", err)
	}
	offered := map[string]OrphanedEntry{}
	for _, e := range entries {
		offered[normalizePath(e.Path)] = e
	}
	if _, ok := offered[normalizePath(liveDir)]; ok {
		t.Error("the sweep offered a staging dir whose only media is an unmuxed set-aside recording — deleting it destroys captured footage")
	}
	gone, ok := offered[normalizePath(goneDir)]
	if !ok {
		t.Fatalf("the sweep did not offer %s, whose job no longer exists", goneDir)
	}
	if len(gone.Asides) != 1 || gone.Asides[0] != asideName {
		t.Errorf("offered entry Asides = %v, want [%s] — the report must name the recording the deletion would take", gone.Asides, asideName)
	}

	// The shield's age rule, read directly: scanStagingOrphans can only see
	// the updated_at AddJob stamps (now), and the window is measured from it.
	aged := &database.Job{ID: "j-shield", Status: database.StatusFinished,
		UpdatedAt: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	if jobNeedsStaging(db, cfg, aged, liveDir) {
		t.Error("an aside-only staging dir is still shielded past the expiry window — the aside shield must defer to the same age rule as the other two")
	}
	fresh := &database.Job{ID: "j-shield", Status: database.StatusFinished,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if !jobNeedsStaging(db, cfg, fresh, liveDir) {
		t.Error("a fresh aside-only staging dir is not shielded")
	}
}

// TestScanTrimOrphansIssuesOneQuery pins ENGINE-17 (report #52): the scan used
// one GetTrimsForJob per job (N+1) on every orphan sweep.
//
// Mutant: restoring the per-job loop — queries counts once per job instead of
// once per scan.
func TestScanTrimOrphansIssuesOneQuery(t *testing.T) {
	_, db := testWorkerSetup(t)

	for i := range 3 {
		jobID := "j" + strconv.Itoa(i)
		if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, Status: database.StatusFinished}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		if err := db.AddTrim(&database.TrimRecord{
			ID: jobID + "-t", JobID: jobID, StartTime: 0, EndTime: 10,
			Filename: jobID + "/trim/clip.mp4", CreatedAt: "2026-09-17T00:00:00Z", Duration: 10,
		}); err != nil {
			t.Fatalf("AddTrim: %v", err)
		}
	}

	trims, err := db.GetAllTrims()
	if err != nil {
		t.Fatalf("GetAllTrims: %v", err)
	}
	if len(trims) != 3 {
		t.Fatalf("GetAllTrims returned %d records, want 3 — the scan would miss a referenced trim and offer it for deletion", len(trims))
	}
	seen := map[string]bool{}
	for _, tr := range trims {
		seen[tr.JobID] = true
	}
	for i := range 3 {
		if !seen["j"+strconv.Itoa(i)] {
			t.Errorf("GetAllTrims omitted job j%d's trim", i)
		}
	}

	// The query COUNT is the finding, and no assertion over a *database.Database
	// can see it — so read the shape from the syntax tree, the package's
	// technique for exactly this (queue_lifecycle_test.go's take-site pin).
	// The mutant the brief names (restoring the per-job loop) puts
	// GetTrimsForJob back inside scanTrimOrphans and fires the first arm.
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "orphans.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse orphans.go: %v", err)
	}
	var scan *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "scanTrimOrphans" {
			scan = fn
		}
	}
	if scan == nil {
		t.Fatal("no scanTrimOrphans declaration in orphans.go")
	}
	if n := len(methodCallPositions(scan, "GetTrimsForJob")); n != 0 {
		t.Errorf("scanTrimOrphans calls GetTrimsForJob %d time(s) — that call is per JOB, so the "+
			"sweep pays one query per job again (ENGINE-17)", n)
	}
	if n := len(methodCallPositions(scan, "GetAllTrims")); n != 1 {
		t.Errorf("scanTrimOrphans calls GetAllTrims %d time(s), want exactly 1 — one query per sweep", n)
	}
}

// TestScanTrimOrphansReportsOnlyUnreferencedClips is the behaviour half of
// ENGINE-17: swapping the per-job loop for one query must not change WHAT the
// sweep offers. A clip a trim record points at is referenced (whatever job it
// belongs to, and whether the record's filename is relative or absolute); a
// clip nothing points at is the orphan.
//
// Mutant: dropping the relative-path resolution against the output dir — every
// referenced clip is offered for deletion, which deletes a trim the UI lists.
func TestScanTrimOrphansReportsOnlyUnreferencedClips(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir

	write := func(rel string) string {
		p := filepath.Join(outputDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte("clip"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return p
	}
	kept := write("j0/trim/kept.mp4")
	abs := write("j1/trim/absolute.mp4")
	orphan := write("j0/trim/orphan.mp4")

	for _, id := range []string{"j0", "j1"} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Status: database.StatusFinished}); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
	}
	if err := db.AddTrim(&database.TrimRecord{
		ID: "t-rel", JobID: "j0", Filename: "j0/trim/kept.mp4",
		CreatedAt: "2026-09-17T00:00:00Z", EndTime: 10, Duration: 10,
	}); err != nil {
		t.Fatalf("AddTrim relative: %v", err)
	}
	// A second job's clip, recorded by absolute path: one query must cover
	// every job's trims, not just the one whose files the walk is under.
	if err := db.AddTrim(&database.TrimRecord{
		ID: "t-abs", JobID: "j1", Filename: abs,
		CreatedAt: "2026-09-17T00:00:00Z", EndTime: 10, Duration: 10,
	}); err != nil {
		t.Fatalf("AddTrim absolute: %v", err)
	}

	entries, err := scanTrimOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanTrimOrphans: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[normalizePath(e.Path)] = true
	}
	if !got[normalizePath(orphan)] {
		t.Errorf("scanTrimOrphans did not offer the unreferenced clip %s", orphan)
	}
	if got[normalizePath(kept)] {
		t.Errorf("scanTrimOrphans offered %s, which a trim record references by relative path", kept)
	}
	if got[normalizePath(abs)] {
		t.Errorf("scanTrimOrphans offered %s, which a trim record references by absolute path", abs)
	}
}
