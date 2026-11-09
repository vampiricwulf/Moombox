package jobfilter

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	webassets "github.com/vampiricwulf/Moombox/web"
)

// TestTypeClipMatchesTheDashboard is owner decision FE-28's filter half, in
// both twins: type:clip selects the Twitch clip jobs — told by their ID's
// twitch.ClipJobIDPrefix, the platform being "twitch" for every Twitch job —
// negated, OR-ed and in any case as every namespace is; any other type value
// matches nothing. The dashboard's modules are evaluated as they are embedded
// and must agree with Match job for job, and their CLIP_JOB_ID_PREFIX copy
// must be the Twitch package's.
//
// Mutants (each run): "type" missing from either twin's namespaces (type:clip
// becomes a text search and matches nothing in that twin); the type arm
// ignoring the value (type:vod matches the clip); IsClipJob dropping its
// platform check (the clip ID on a youtube row matches); IsClipJob dropping
// its slug check (the bare prefix matches); the dashboard's prefix drifted.
func TestTypeClipMatchesTheDashboard(t *testing.T) {
	var src strings.Builder
	for _, name := range []string{"public/modules/filter-parser.js", "public/modules/filter-engine.js"} {
		raw, err := webassets.PublicFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read the embedded %s: %v", name, err)
		}
		s := strings.ReplaceAll(string(raw), "\r\n", "\n")
		s = regexp.MustCompile(`(?m)^import .*$`).ReplaceAllString(s, "")
		src.WriteString(regexp.MustCompile(`(?m)^export `).ReplaceAllString(s, ""))
		src.WriteString("\n")
	}
	src.WriteString(`function webMatches(query, id, platform) {
		const job = { id, title: "", channelName: "", videoId: id, status: "Finished", platform };
		return applyFilterTokens([job], parseFilterQuery(query)).length === 1;
	}`)
	vm := goja.New()
	if _, err := vm.RunString(src.String()); err != nil {
		t.Fatalf("the dashboard's filter modules do not evaluate: %v", err)
	}
	if got := vm.Get("CLIP_JOB_ID_PREFIX").String(); got != twitch.ClipJobIDPrefix {
		t.Errorf("the dashboard's CLIP_JOB_ID_PREFIX = %q, want twitch.ClipJobIDPrefix %q", got, twitch.ClipJobIDPrefix)
	}
	webMatch, ok := goja.AssertFunction(vm.Get("webMatches"))
	if !ok {
		t.Fatal("webMatches is not a function")
	}
	jobs := []struct{ id, platform string }{
		{twitch.ClipJobID("FaintLightGullWholeWheat"), "twitch"},
		{"tw_v123456", "twitch"},
		{"dQw4w9WgXcQ", "youtube"},
		// Shapes no path writes, where both twins agree with the predicate
		// the worker kept (K16): no slug, and a clip ID on another platform.
		{twitch.ClipJobIDPrefix, "twitch"},
		{twitch.ClipJobID("Slug"), "youtube"},
	}
	for query, want := range map[string][]bool{
		"type:clip":                  {true, false, false, false, false},
		"TYPE:Clip":                  {true, false, false, false, false},
		"-type:clip":                 {false, true, true, true, true},
		"type:clip|platform:youtube": {true, false, true, false, true},
		"type:vod":                   {false, false, false, false, false},
	} {
		for i, j := range jobs {
			tui := Match(Parse(query), &database.Job{ID: j.id, VideoID: j.id, Status: database.StatusFinished, Platform: j.platform})
			v, err := webMatch(goja.Undefined(), vm.ToValue(query), vm.ToValue(j.id), vm.ToValue(j.platform))
			if err != nil {
				t.Fatalf("%s on %s: %v", query, j.id, err)
			}
			if tui != want[i] || v.ToBoolean() != want[i] {
				t.Errorf("%s on %s: TUI %v, dashboard %v, want %v", query, j.id, tui, v.ToBoolean(), want[i])
			}
		}
	}
	// A text term spelled like the new prefix is quoted on the way back out,
	// as status:/channel:/platform: are, so it does not come back a type term.
	if got := Serialize([]Token{Term(KindText, "type:clip", false)}); got != `"type:clip"` {
		t.Errorf("Serialize(text %q) = %s, want it quoted", "type:clip", got)
	}
}
