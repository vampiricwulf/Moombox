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

// TestOrphanSweepShieldsAnAsideOnlyStagingDir pins extra item (b) as fix round
// 1 re-ruled it: the sweep learns engine.IsStagedRestartPath, and a staging dir
// whose only remaining media is a set-aside recording is never an orphan to
// delete — with NO age rule, because an aside that survives finalize is one
// FFmpeg could not read and the only copy of that footage. When such a dir IS
// offered (its job row is gone entirely), the sweep names the aside so the
// operator sees what the deletion would take.
//
// Mutants, one per arm: dropping the aside term from jobNeedsStaging (the
// sweep offers a live aside-only dir for deletion); putting the age rule back
// on the aside shield (a month-old recording is offered on a stock install,
// whose expiry default is 7 days, not 0); dropping the Asides field (the offer
// says only "staging", with no hint that it holds a recording).
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

	// The shield has NO age rule (fix round 1, Important 2): read directly,
	// because scanStagingOrphans can only ever see the updated_at AddJob
	// stamps. incomplete_staging_expiry_days DEFAULTS TO 7 — "0 = preserve
	// forever" is the meaning of the value, not the default — so an age rule
	// here would offer a month-old unmuxed recording for deletion on a stock
	// install, and that recording is exactly what ENGINE-6 exists to keep.
	for _, age := range []time.Duration{0, 8 * 24 * time.Hour, 30 * 24 * time.Hour} {
		job := &database.Job{ID: "j-shield", Status: database.StatusFinished,
			UpdatedAt: time.Now().Add(-age).UTC().Format(time.RFC3339)}
		if !jobNeedsStaging(db, cfg, job, liveDir) {
			t.Errorf("an aside-only staging dir aged %v is no longer shielded — an unmuxed set-aside "+
				"recording never expires; it is shielded until it is muxed or the job is deleted", age)
		}
	}
	// The control: the SAME job, the same age, with the aside gone is an
	// ordinary expired orphan — proving the aside is what held it.
	emptyDir := filepath.Join(stagingRoot, "j-empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expired := &database.Job{ID: "j-shield", Status: database.StatusFinished, IncompleteTail: true,
		UpdatedAt: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	if jobNeedsStaging(db, cfg, expired, emptyDir) {
		t.Error("an aged incomplete-tail dir with no aside in it is still shielded — the tail and chat shields keep their age rule")
	}
}

// TestOutputSweepOwnsARecoveredAsideSibling pins fix round 1's Important 1: a
// recovered set-aside recording is written into the OUTPUT directory as
// <stem>.restart-<ts>.mp4, and nothing references it (it is deliberately not a
// segment row), so the output sweep offered it as a deletable orphan the
// moment finalize produced it — with no age rule and nothing naming it, one
// click from the Files tab's "Delete All". It is owned by the job whose stem
// it carries.
//
// The three shapes, one sub-test each. Mutants: dropping the sibling term from
// scanOutputOrphans (the sibling is offered beside a live job); dropping the
// stem-ownership check (a sibling whose job is gone is silently unreachable);
// dropping the Asides annotation (an offered sibling reads as scratch space).
func TestOutputSweepOwnsARecoveredAsideSibling(t *testing.T) {
	const siblingName = "Stream Title.restart-1700000000.mp4"

	setup := func(t *testing.T, files ...string) (*database.Database, *config.MoomboxConfig, string) {
		t.Helper()
		_, db := testWorkerSetup(t)
		outputDir := t.TempDir()
		cfg := &config.MoomboxConfig{}
		cfg.Paths.OutputDirectory = outputDir
		for _, name := range files {
			if err := os.WriteFile(filepath.Join(outputDir, name), []byte("media"), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		return db, cfg, outputDir
	}
	offered := func(t *testing.T, db *database.Database, cfg *config.MoomboxConfig) map[string]OrphanedEntry {
		t.Helper()
		entries, err := scanOutputOrphans(db, cfg)
		if err != nil {
			t.Fatalf("scanOutputOrphans: %v", err)
		}
		out := map[string]OrphanedEntry{}
		for _, e := range entries {
			out[normalizePath(e.Path)] = e
		}
		return out
	}

	t.Run("the job still exists: the sibling is owned, never offered", func(t *testing.T) {
		db, cfg, outputDir := setup(t, "Stream Title.mp4", siblingName)
		if _, err := db.AddJob(&database.Job{ID: "j-own", VideoID: "j-own", Status: database.StatusFinished,
			OutputFile: filepath.Join(outputDir, "Stream Title.mp4")}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		got := offered(t, db, cfg)
		if _, ok := got[normalizePath(filepath.Join(outputDir, siblingName))]; ok {
			t.Error("the recovered set-aside recording was offered as a deletable output orphan — it is the job's own captured footage, and Delete All takes it")
		}
		if len(got) != 0 {
			t.Errorf("scanOutputOrphans offered %d entries for a job with everything referenced: %v", len(got), got)
		}
	})

	t.Run("the job is gone: the sibling folds into its stem's entry", func(t *testing.T) {
		db, cfg, outputDir := setup(t, "Stream Title.mp4", siblingName)
		got := offered(t, db, cfg)
		if _, ok := got[normalizePath(filepath.Join(outputDir, siblingName))]; ok {
			t.Error("the sibling was offered as its own row — it belongs to the archive beside it, and deleting one without the other is not a choice worth offering")
		}
		main, ok := got[normalizePath(filepath.Join(outputDir, "Stream Title.mp4"))]
		if !ok {
			t.Fatal("the orphaned archive itself was not offered")
		}
		if len(main.Asides) != 1 || main.Asides[0] != siblingName {
			t.Errorf("the archive's entry lists Asides = %v, want [%s] — the operator must see the recovered recording that goes with it", main.Asides, siblingName)
		}
	})

	t.Run("only the sibling is left: it stands alone, still named", func(t *testing.T) {
		db, cfg, outputDir := setup(t, siblingName)
		got := offered(t, db, cfg)
		e, ok := got[normalizePath(filepath.Join(outputDir, siblingName))]
		if !ok {
			t.Fatal("a sibling with neither a job nor an archive beside it was not offered at all — it would be undeletable forever")
		}
		if len(e.Asides) != 1 || e.Asides[0] != siblingName {
			t.Errorf("the lone sibling's entry lists Asides = %v, want [%s]", e.Asides, siblingName)
		}
	})
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
			ID: jobID + "-t", JobID: jobID, StartTime: 3, EndTime: 10,
			Filename: jobID + "/trim/clip.mp4", CreatedAt: "2026-09-17T00:00:00Z", Duration: 7,
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
		// Every column lands in its own field. GetAllTrims and GetTrimsForJob
		// share one projection and one scan (fix round 1, Minor 5), so this
		// also pins the pair against a column added to one and not the other.
		// Mutant: swapping two Scan targets in scanTrims.
		if tr.ID != tr.JobID+"-t" || tr.StartTime != 3 || tr.EndTime != 10 || tr.Duration != 7 ||
			tr.Filename != tr.JobID+"/trim/clip.mp4" || tr.CreatedAt != "2026-09-17T00:00:00Z" {
			t.Errorf("GetAllTrims returned %+v — a column is landing in the wrong field", tr)
		}
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
