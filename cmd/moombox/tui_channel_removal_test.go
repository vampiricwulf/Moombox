package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// TestTUIChannelRemovalAdapters: the TUI's removal prompt gets the counts
// and the delete the dashboard gets — worker.SummarizeChannelRemoval and
// DeletePendingChannelJobs over the configured staging directory — with the
// footage rows' titles and the kept count carried across.
//
// Mutants killed: the footage titles not copied (the prompt names nothing);
// the staging directory not read from the config (the parked capture reads
// as pending and is deleted); the kept count reported as 0.
func TestTUIChannelRemovalAdapters(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log, err := logger.New(filepath.Join(dir, "removal.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	s := &runState{db: db, log: log, configStore: config.NewStore(cfg, "")}

	ch := "UCremoving"
	for _, row := range []struct {
		id     string
		status database.JobStatus
	}{{"queued", database.StatusQueued}, {"parked", database.StatusCookies}, {"done", database.StatusFinished}} {
		if _, err := db.AddJob(&database.Job{ID: row.id, VideoID: row.id, URL: "u", Title: "T " + row.id,
			Status: row.status, ChannelID: &ch}); err != nil {
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

	app := tui.NewApp()
	s.wireTUIChannelRemoval(app)

	info, err := app.OnChannelRemovalSummary(ch)
	if err != nil {
		t.Fatalf("OnChannelRemovalSummary: %v", err)
	}
	if info.Total != 3 || info.Pending != 1 || !slices.Equal(info.Footage, []string{"T parked"}) {
		t.Errorf("info = %+v, want 3 jobs, 1 pending, the parked capture named", info)
	}

	n, kept, err := app.OnDeletePendingChannelJobs(ch)
	if err != nil || n != 1 || kept != 1 {
		t.Errorf("delete = %d deleted, %d kept, err %v; want 1 and 1", n, kept, err)
	}
	if j, _ := db.GetJob("parked"); j == nil {
		t.Error("the parked capture with footage was deleted")
	}
	if j, _ := db.GetJob("queued"); j != nil {
		t.Error("the pending job survived")
	}
}

// TestTUIChannelRemovalIsWired reads tui_wiring.go as text, in the idiom of
// TestJobLogWiringReadsTheDatabaseBuffer: runTUI must install the removal
// prompt's callbacks, or the prompt never counts and offers keep and Esc
// only.
//
// Mutant killed: the wireTUIChannelRemoval call dropped from runTUI.
func TestTUIChannelRemovalIsWired(t *testing.T) {
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !regexp.MustCompile(`(?m)^\s*s\.wireTUIChannelRemoval\(app\)\s*$`).MatchString(text) {
		t.Error("runTUI does not call s.wireTUIChannelRemoval(app)")
	}
}
