package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDashboardJobRowsCarryHasStaging is owner decision PE-5 on the
// WebSocket side: the dashboard's job list is built from initial_state,
// jobs_update and job_update, not GET /api/jobs, so each of them sends its
// rows through listedJob(s), which reads the configured staging base and
// flags hasStaging on a row Resume can be offered on (owner ruling R-B2
// narrowed the flag to those) — the row icon's Resume needs it. The
// rows keep the hub's frame ordering: a listed row still answers
// JobVersion, so a stale job_update is still dropped behind a later one.
//
// The call sites are pinned by source, as the progress frame's are
// (TestEveryProgressTickStillProducesExactlyOneFrame): driving the
// real subscribers needs a whole runState.
//
// Mutants (each run): any one job_update, jobs_update or initial_state site
// sending the bare row (its pin fails); listedJob reading another staging
// base (the staged row reads false); listedJob flagging every terminal row
// (the Finished and Twitch rows carry one).
func TestDashboardJobRowsCarryHasStaging(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))
	if err := os.MkdirAll(filepath.Join(cfg.Paths.StagingDirectory, "staged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Paths.StagingDirectory, "staged", "video_stream"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := []*database.Job{
		{ID: "staged", Status: database.StatusError, Platform: "youtube", Version: 7},
		{ID: "bare", Status: database.StatusCancelled, Platform: "youtube"},
		{ID: "live", Status: database.StatusDownloading, Platform: "youtube"},
		{ID: "staged", Status: database.StatusFinished, Platform: "youtube"},
		{ID: "staged", Status: database.StatusFinished, Platform: "youtube", IncompleteTail: true},
		{ID: "staged", Status: database.StatusError, Platform: "twitch"},
	}
	raw, err := json.Marshal(listedJobs(store, jobs))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for i, want := range []any{true, false, nil, nil, true, nil} {
		got, present := rows[i]["hasStaging"]
		if want == nil && present || want != nil && got != want {
			t.Errorf("%s: hasStaging = %v (present %v), want %v", jobs[i].ID, got, present, want)
		}
	}
	type versioned interface{ JobVersion() (string, uint64) }
	if v, ok := any(listedJob(store, jobs[0])).(versioned); !ok {
		t.Error("a listed row lost JobVersion: the hub would send a stale job_update over a later one")
	} else if id, ver := v.JobVersion(); id != "staged" || ver != 7 {
		t.Errorf("JobVersion = %s %d, want staged 7", id, ver)
	}

	broadcast := regexp.MustCompile(`s\.wsHub\.BroadcastJobs?Update\(([^\n]*)\)\n`)
	sites := 0
	for _, file := range []string{"monitor_callbacks.go", "routes_wiring.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range broadcast.FindAllStringSubmatch(string(src), -1) {
			sites++
			if !strings.HasPrefix(m[1], "listedJob") {
				t.Errorf("%s: %s sends rows that are not listedJob(s)", file, strings.TrimSpace(m[0]))
			}
		}
	}
	if sites != 5 {
		t.Errorf("found %d job_update/jobs_update sites, want 5: re-check every one sends listedJob(s)", sites)
	}
	src, err := os.ReadFile("ws_wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"jobs":                listedJobs(s.configStore, jobs),`) {
		t.Error("initial_state's jobs are not listedJobs")
	}
}
