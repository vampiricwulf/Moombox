package worker

import (
	"os"
	"strings"
	"testing"
)

// TestCancelListenersDoNotReadTheLiveJob: the orchestrators' cancel listener
// runs on whichever goroutine wrote the job row — the progress tracker's
// activity loop, a route — while muxAndFinalize replaces jobCtx.Job with a
// fresh read, so a listener reading jobCtx.Job raced it (the race detector
// caught it under TestTwitchVodRidesOutAnOutage). The listeners compare
// against an ID read before subscribing. CI runs this package without -race,
// so the rule is pinned by source. The same holds for the goroutines nothing
// waits for before the mux: the Twitch thumbnail prefetch and both chat
// goroutines (whose panic path logged jobCtx.Job.ID).
//
// Mutants: compare against jobCtx.Job.ID inside either listener again; read
// jobCtx.Job inside the thumbnail goroutine again.
func TestCancelListenersDoNotReadTheLiveJob(t *testing.T) {
	for _, file := range []string{"orchestrator.go", "orchestrator_twitch.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		const open = "o.db.OnJobUpdate(func("
		i := strings.Index(body, open)
		if i < 0 {
			t.Fatalf("%s: no cancel listener found", file)
		}
		end := strings.Index(body[i:], "\n\t})")
		if end < 0 {
			t.Fatalf("%s: cancel listener has no end", file)
		}
		if listener := body[i : i+end]; strings.Contains(listener, "jobCtx.Job") {
			t.Errorf("%s: the cancel listener reads jobCtx.Job, which muxAndFinalize replaces on another goroutine", file)
		}
	}

	for _, g := range []struct{ file, marker string }{
		{"orchestrator_twitch.go", "panic in thumbnail download"},
		{"orchestrator_twitch.go", "panic in Twitch chat downloader"},
		{"orchestrator.go", "panic in YouTube chat downloader"},
	} {
		src, err := os.ReadFile(g.file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(src)
		m := strings.Index(body, g.marker)
		if m < 0 {
			t.Fatalf("%s: %q not found", g.file, g.marker)
		}
		start := strings.LastIndex(body[:m], "go func() {")
		end := strings.Index(body[m:], "}()\n\t}")
		if start < 0 || end < 0 {
			t.Fatalf("%s: the goroutine around %q not found", g.file, g.marker)
		}
		if strings.Contains(body[start:m+end], "jobCtx.Job") {
			t.Errorf("%s: the goroutine logging %q reads jobCtx.Job, which muxAndFinalize replaces", g.file, g.marker)
		}
	}
}
