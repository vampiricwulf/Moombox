package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// assertCarriesItsJob is the assertion every row of
// TestHandBuiltSendsEscapeAndCarryTheirJob (notify_escape_test.go) makes
// beside its escaping: the embed this producer sent names its job.
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

// TestChannelLessRowCarriesNoAuthorButNamesItsJob is the unknown-channel arm
// of the shape assertCarriesItsJob checks: a row with no channel name (a
// manual `moombox add` of a bare URL) must carry NO author object — Discord
// answers an author with an empty name with a 400, and DiscordWebhook.Send
// treats a non-429 4xx as permanent, so the embed would be lost rather than
// degraded. JobID and Platform still travel.
func TestChannelLessRowCarriesNoAuthorButNamesItsJob(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{notifier: rec, logger: discardLogger{}}
	anon := &database.Job{
		ID: "tw_anon", VideoID: "anon", Platform: "twitch",
		Title: "A Stream", URL: "https://twitch.tv/videos/anon",
	}
	o.sendGapSplitNotification(&JobContext{Job: anon}, 0, QualityInfo{Label: "1080p60"})

	got := rec.ByEvent("gap_split")
	if len(got) != 1 {
		t.Fatalf("gap_split sends = %d, want 1", len(got))
	}
	if got[0].Opts.Author != nil {
		t.Errorf("Author = %+v, want nil for a row with no channel name", got[0].Opts.Author)
	}
	if got[0].Opts.JobID != anon.ID || got[0].Opts.Platform != anon.Platform {
		t.Errorf("JobID/Platform = %q/%q, want %q/%q — the author is the only field the unknown channel costs",
			got[0].Opts.JobID, got[0].Opts.Platform, anon.ID, anon.Platform)
	}
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
