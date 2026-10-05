package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
)

// At hide_finished_age_days = 0 the job_update subscriber's archive gate
// swallowed the Muxing → Finished transition: the row it had just written was
// already "archived", so the dashboard kept a Muxing card until it
// reconnected. Every event the subscriber sees comes from UpdateJobFields,
// which stamps updated_at with the write time, so the gate could fire nowhere
// else; it is gone, and nothing between the subscription and the dispatch
// may return early. Driving the subscriber needs a whole runState, so the
// body is pinned by source.
//
// Mutant: restoring the IsArchivedAt gate.
func TestEveryJobChangeIsBroadcast(t *testing.T) {
	// The premise: at threshold 0 a row written this second already counts as
	// archived.
	fresh := &database.Job{Status: database.StatusFinished, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if !jobfilter.IsArchivedAt(fresh, 0, time.Now().Add(time.Millisecond)) {
		t.Fatal("premise: a just-written Finished row is archived at threshold 0")
	}

	src, err := os.ReadFile("monitor_callbacks.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "s.unsubWSJobUpdate = s.db.OnJobChange(func(ev *database.JobChange) {")
	dispatch := strings.Index(text, "\t\tif isProgressOnlyChange(ev.Changes) {")
	if start < 0 || dispatch < start {
		t.Fatal("cannot find the job_update subscriber and its dispatch")
	}
	var code strings.Builder
	for _, line := range strings.Split(text[start:dispatch], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code.WriteString(line + "\n")
		}
	}
	for _, banned := range []string{"return", "IsArchivedAt"} {
		if strings.Contains(code.String(), banned) {
			t.Errorf("the job_update subscriber has %q before its dispatch — an event can be dropped", banned)
		}
	}
}
