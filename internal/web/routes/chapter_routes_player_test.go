package routes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChaptersRouteMatchesThePlayersFetch keeps the route and its one caller
// in step (CLAUDE.md "API route prefix"): the player asks for exactly the
// path this package registers.
//
// Mutant: the player fetching /api/jobs/{id}/chapter (or the route renamed).
func TestChaptersRouteMatchesThePlayersFetch(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "public", "modules", "player.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "fetch(`/api/jobs/${jobId}/chapters`)") {
		t.Error("player.js no longer fetches /api/jobs/${jobId}/chapters")
	}
	f := newChapterFixture(t)
	f.addJob(t, "yt_x", nil)
	f.get(t, "yt_x")
}
