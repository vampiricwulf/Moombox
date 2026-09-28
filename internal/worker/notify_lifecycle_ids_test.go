package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// assertCarriesItsJob is the one assertion every row below makes: the embed
// this producer sent names its job.
//
// Three fields, not one, because they arrive together and are useless apart.
// JobID is what Manager.planLifecycle (internal/notifications/lifecycle.go)
// keys on — an embed without it can never be the job's edited lifecycle
// message, so an edit-mode target would manage the three events that DO carry
// one and post the other eight separately. Platform is the footer's middle
// term. Author is the second half of the dashboard deep link, which
// Manager.Send applies only when publicURL, JobID and Author are all present.
func assertCarriesItsJob(t *testing.T, rec *notificationtest.Recorder, event string, job *database.Job) {
	t.Helper()
	got := rec.ByEvent(event)
	if len(got) != 1 {
		t.Fatalf("%s sends = %d, want 1: %+v", event, len(got), rec.Calls())
	}
	if got[0].Opts.JobID != job.ID {
		t.Errorf("%s: JobID = %q, want %q — planLifecycle keys on it", event, got[0].Opts.JobID, job.ID)
	}
	if got[0].Opts.Platform != job.Platform {
		t.Errorf("%s: Platform = %q, want %q", event, got[0].Opts.Platform, job.Platform)
	}
	if got[0].Opts.Author == nil {
		t.Errorf("%s: no Author — the dashboard deep link needs JobID AND Author", event)
	}
}

// TestLifecycleScheduleSendsCarryTheirJob covers the `scheduled` and
// `rescheduled` halves of updateJobMetadata, driven exactly as
// TestStreamProcessorNotifiesThroughTheSenderSeam drives the first of them.
//
// One processor and one row for both: the reschedule only fires on a refresh
// whose ScheduledStartTime differs from the one already stored, so the second
// call needs the first one's write.
func TestLifecycleScheduleSendsCarryTheirJob(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rec := notificationtest.New()
	sp := &StreamProcessor{db: db, notifier: rec}

	if _, err := db.AddJob(&database.Job{
		ID: "yt_sched", VideoID: "sched", Platform: "youtube",
		Status: database.StatusUpcoming,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob("yt_sched")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	// First sight: no stored start time, so the confirmation fires.
	sp.updateJobMetadata(stored, &youtube.VideoInfo{
		Title:              "A Scheduled Stream",
		ChannelName:        "A Channel",
		ScheduledStartTime: "2026-10-01T12:00:00Z",
		IsUpcoming:         true,
	}, false)
	// A refresh that moves the time, with the stream still upcoming: the
	// reschedule fires and the confirmation does not fire twice.
	sp.updateJobMetadata(stored, &youtube.VideoInfo{
		Title:              "A Scheduled Stream",
		ChannelName:        "A Channel",
		ScheduledStartTime: "2026-10-01T14:00:00Z",
		IsUpcoming:         true,
	}, true)

	assertCarriesItsJob(t, rec, "scheduled", stored)
	assertCarriesItsJob(t, rec, "rescheduled", stored)
}

// TestLifecycleSplitSendsCarryTheirJob covers `gap_split` and `quality_split`,
// the two mid-download part boundaries.
func TestLifecycleSplitSendsCarryTheirJob(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{notifier: rec, logger: discardLogger{}}
	job := &database.Job{
		ID: "tw_split", VideoID: "split", Platform: "twitch",
		Title: "A Stream", ChannelName: "A Channel",
		URL: "https://twitch.tv/videos/split",
	}
	jobCtx := &JobContext{Job: job}

	o.sendGapSplitNotification(jobCtx, 0, QualityInfo{Label: "1080p60"})
	o.sendQualitySplitNotification(jobCtx, "Twitch",
		QualityInfo{Label: "1080p60"}, QualityInfo{Label: "720p60"}, 1, true)

	assertCarriesItsJob(t, rec, "gap_split", job)
	assertCarriesItsJob(t, rec, "quality_split", job)
}

// TestLifecycleMuxingSendCarriesItsJob covers `muxing`. sendMuxingStarting
// re-reads the row before building its embed, so the fixture writes the
// channel the author line needs to the DATABASE, not just to the JobContext.
func TestLifecycleMuxingSendCarriesItsJob(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "mux.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	job := &database.Job{
		ID: "yt_muxid", VideoID: "muxid", Platform: "youtube",
		Title: "A Job", ChannelName: "A Channel",
		Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	rec := notificationtest.New()
	o := &DownloadOrchestrator{db: db, notifier: rec, logger: discardLogger{}}
	o.sendMuxingStarting(&JobContext{Job: stored})

	assertCarriesItsJob(t, rec, "muxing", stored)
}

// TestLifecycleTwitchSessionSendsCarryTheirJob covers both keys
// sendTwitchSessionNotification delivers. The event stays a PARAMETER of that
// helper — only the three identity fields are added — so both of its callers
// have to be exercised to prove the change is in the helper and not in one
// call site.
func TestLifecycleTwitchSessionSendsCarryTheirJob(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{notifier: rec, logger: discardLogger{}}
	job := &database.Job{
		ID: "tw_sess", VideoID: "sess", Platform: "twitch",
		Title: "A Stream", ChannelName: "A Channel",
		URL: "https://twitch.tv/videos/sess",
	}
	jobCtx := &JobContext{Job: job}

	o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
		"Connectivity restored, resuming download: "+job.Title,
		notifications.TypeDownload, "connectivity_resume", QualityInfo{Label: "1080p60"}, 2)
	o.sendTwitchSessionNotification(jobCtx, "Twitch Download Finalizing — Connectivity Lost",
		"Connectivity lost during download: "+job.Title,
		notifications.TypeDownload, "connectivity_split", QualityInfo{Label: "1080p60"}, 3)

	assertCarriesItsJob(t, rec, "connectivity_resume", job)
	assertCarriesItsJob(t, rec, "connectivity_split", job)
}

// TestEveryLifecycleSendCarriesItsJob parses internal/worker and fails any
// notifications.SendOptions literal whose Event is a lifecycle key but which
// sets no JobID. Two of the eight sends live inside Execute paths a unit test
// cannot reach (ExecuteWithChat and ExecuteTwitch both do network work before
// they notify); this reads the source rather than pretending otherwise, and it
// is the row that catches the ninth producer someone adds next year.
//
// A literal whose Event is COMPUTED rather than spelled out is held to the
// same rule: sendTwitchSessionNotification takes its event as a parameter, so
// nothing here can prove such a send is not a lifecycle one. Requiring the job
// id is the answer that cannot be wrong — every job send wants it anyway, for
// the footer if for nothing else.
//
// THE MUTANT: delete the JobID line from any one of the eight literals.
func TestEveryLifecycleSendCarriesItsJob(t *testing.T) {
	// The same set as internal/notifications' lifecycleEvents (lifecycle.go).
	// Duplicated rather than exported: the notifications package is where the
	// set is DEFINED, and a test in this package widening that package's API
	// to read it would be the tail wagging the dog. TestLifecycleEventSet over
	// there pins the membership.
	lifecycle := map[string]bool{
		"found": true, "added": true, "scheduled": true, "rescheduled": true,
		"downloading": true, "quality_split": true, "gap_split": true,
		"connectivity_resume": true, "connectivity_split": true,
		"muxing": true, "finished": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SendOptions" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "notifications" {
				return true
			}
			var event ast.Expr
			hasJobID := false
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Event":
					event = kv.Value
				case "JobID":
					hasJobID = true
				}
			}
			if event == nil || hasJobID {
				// No event at all is not a lifecycle send: planLifecycle
				// returns an empty plan for an empty Event.
				return true
			}
			at := fset.Position(lit.Pos())
			if basic, ok := event.(*ast.BasicLit); ok && basic.Kind == token.STRING {
				key, err := strconv.Unquote(basic.Value)
				if err == nil && lifecycle[key] {
					problems = append(problems, at.String()+": Event "+basic.Value+" sets no JobID")
				}
				return true
			}
			problems = append(problems,
				at.String()+": Event is computed, so it may be a lifecycle key — it must set a JobID")
			return true
		})
	}
	for _, p := range problems {
		t.Errorf("%s — planLifecycle keys on Opts.JobID, so this embed can never be the job's edited lifecycle message", p)
	}
}
