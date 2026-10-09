package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// prefer_60fps was honoured only by the whole-file VOD selector: every live
// path (DASH, manifestless, HLS, and the quality probe) ranked the renditions
// at a size by bandwidth alone, so a stream that offered both 1080p60 and
// 1080p30 recorded at 60 whatever the setting said, and the only way to a 30
// fps recording was pinning an itag. An explicit "…p60" still asks for 60; a
// suffix-less preference and "best" now follow the setting.
//
// Mutant: fpsPreference ignoring prefer60fps, or either selector ranking the
// chosen size by bandwidth alone again.
func TestLiveSelectorsHonourPrefer60fps(t *testing.T) {
	dash := []DashStreamInfo{
		{Itag: 299, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 60, Bandwidth: 6_000_000},
		{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4_000_000},
		{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, FPS: 30, Bandwidth: 2_000_000},
	}
	hls := []engine.HlsVariant{
		{Width: 1920, Height: 1080, FPS: 60, Bandwidth: 6_000_000, URL: "60"},
		{Width: 1920, Height: 1080, FPS: 30, Bandwidth: 4_000_000, URL: "30"},
		{Width: 1280, Height: 720, FPS: 30, Bandwidth: 2_000_000, URL: "720"},
	}
	for _, tc := range []struct {
		pref      string
		prefer60  bool
		wantFPS   int
		wantShort int
	}{
		{"", true, 60, 1080},
		{"best", true, 60, 1080},
		{"", false, 30, 1080},
		{"best", false, 30, 1080},
		{"1080p", true, 60, 1080},
		{"1080p", false, 30, 1080},
		{"1080p60", false, 60, 1080}, // an explicit ask outranks the setting
		{"720p", true, 30, 720},      // nothing at 60 there: the preference cannot empty a size
	} {
		d := SelectBestDashStream(dash, 0, 2160, true, tc.pref, tc.prefer60)
		if d == nil || d.FPS != tc.wantFPS || d.Height != tc.wantShort {
			t.Errorf("DASH pref=%q prefer60=%v: got %+v, want %dp%d", tc.pref, tc.prefer60, d, tc.wantShort, tc.wantFPS)
		}
		h := selectHlsVariant(hls, tc.pref, 2160, tc.prefer60)
		if h == nil || h.FPS != tc.wantFPS || h.Height != tc.wantShort {
			t.Errorf("HLS pref=%q prefer60=%v: got %+v, want %dp%d", tc.pref, tc.prefer60, h, tc.wantShort, tc.wantFPS)
		}
	}

	// A frame rate the manifest does not state is never "preferred" — with
	// none known, bandwidth decides as it always did.
	unknown := []DashStreamInfo{
		{Itag: 1, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 1},
		{Itag: 2, MimeType: "video/mp4", Width: 1920, Height: 1080, Bandwidth: 2},
	}
	for _, prefer60 := range []bool{true, false} {
		if d := SelectBestDashStream(unknown, 0, 2160, true, "", prefer60); d == nil || d.Itag != 2 {
			t.Errorf("prefer60=%v with no stated frame rates: got %+v, want the higher bandwidth", prefer60, d)
		}
	}
}

// Ruling R1 made the CAP short-edge aware, but the preference still compared
// raw Height: a portrait stream's "1080p" (1080x1920) found nothing 1080 tall
// and descended to the next lower HEIGHT it did find — the 480x854
// rendition. Both selectors measure the short edge now.
//
// Mutant: dashFieldAccessor / hlsFieldAccessor (which the next-lower descent
// shares) returning Height again.
func TestPortraitPreferenceMatchesTheShortEdge(t *testing.T) {
	dash := []DashStreamInfo{
		{Itag: 1, MimeType: "video/mp4", Width: 1080, Height: 1920, FPS: 30, Bandwidth: 5},
		{Itag: 2, MimeType: "video/mp4", Width: 720, Height: 1280, FPS: 30, Bandwidth: 3},
		{Itag: 3, MimeType: "video/mp4", Width: 480, Height: 854, FPS: 30, Bandwidth: 1},
	}
	if d := SelectBestDashStream(dash, 0, 2160, true, "1080p", true); d == nil || d.Itag != 1 {
		t.Errorf("DASH portrait 1080p: got %+v, want the 1080x1920 rendition", d)
	}
	if d := SelectBestDashStream(dash, 0, 2160, true, "900p", true); d == nil || d.Itag != 2 {
		t.Errorf("DASH portrait 900p: got %+v, want the next lower short edge, 720x1280", d)
	}
	hls := []engine.HlsVariant{
		{Width: 1080, Height: 1920, FPS: 30, Bandwidth: 5, URL: "1080"},
		{Width: 720, Height: 1280, FPS: 30, Bandwidth: 3, URL: "720"},
		{Width: 480, Height: 854, FPS: 30, Bandwidth: 1, URL: "480"},
	}
	if h := selectHlsVariant(hls, "1080p", 2160, true); h == nil || h.URL != "1080" {
		t.Errorf("HLS portrait 1080p: got %+v, want the 1080x1920 variant", h)
	}
	if h := selectHlsVariant(hls, "900p", 2160, true); h == nil || h.URL != "720" {
		t.Errorf("HLS portrait 900p: got %+v, want the next lower short edge, 720x1280", h)
	}
}

// A preference whose size the stream does not offer descends to the next lower
// size, and that size used to be ranked by bandwidth alone: "1440p" on a
// stream topping out at 1080 recorded 1080p60 with prefer_60fps off, though
// "1080p" itself took 1080p30, and a "1440p60" took whichever 1080 rendition
// carried more bits. The descent now ranks the lower size exactly as the
// named size would have been — the fps suffix, else prefer_60fps, then
// bandwidth — the rule Twitch's selectNextLowerVariant applies.
//
// Mutants: selectNextLowerIdx taking the highest bandwidth at the lower size
// again; either descent call ranking with fpsPreference(0, prefer60fps) (the
// suffix dropped), or with a literal true or false for prefer60fps.
func TestQualityDescentRanksTheLowerSizeByFrameRate(t *testing.T) {
	ladder := func(bw60, bw30 int) []DashStreamInfo {
		return []DashStreamInfo{
			{Itag: 299, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 60, Bandwidth: bw60},
			{Itag: 137, MimeType: "video/mp4", Width: 1920, Height: 1080, FPS: 30, Bandwidth: bw30},
			{Itag: 136, MimeType: "video/mp4", Width: 1280, Height: 720, FPS: 30, Bandwidth: 2_000_000},
		}
	}
	for _, tc := range []struct {
		pref       string
		prefer60   bool
		bw60, bw30 int
		wantFPS    int
	}{
		{"1440p", false, 6_000_000, 4_000_000, 30},   // the 60 out-bits the 30 the setting asks for
		{"1440p", true, 4_000_000, 6_000_000, 60},    // the 30 out-bits the 60 the setting asks for
		{"1440p60", false, 4_000_000, 6_000_000, 60}, // the suffix asks for 60 at the lower size too
	} {
		dash := ladder(tc.bw60, tc.bw30)
		hls := make([]engine.HlsVariant, len(dash))
		for i, s := range dash {
			hls[i] = engine.HlsVariant{Width: s.Width, Height: s.Height, FPS: s.FPS, Bandwidth: s.Bandwidth}
		}
		if d := SelectBestDashStream(dash, 0, 2160, true, tc.pref, tc.prefer60); d == nil || d.Height != 1080 || d.FPS != tc.wantFPS {
			t.Errorf("DASH pref=%q prefer60=%v: got %+v, want 1080p%d", tc.pref, tc.prefer60, d, tc.wantFPS)
		}
		if h := selectHlsVariant(hls, tc.pref, 2160, tc.prefer60); h == nil || h.Height != 1080 || h.FPS != tc.wantFPS {
			t.Errorf("HLS pref=%q prefer60=%v: got %+v, want 1080p%d", tc.pref, tc.prefer60, h, tc.wantFPS)
		}
	}
}

// Every VIDEO selection outside tests must carry the job's prefer_60fps — the
// strategies, the cipher re-selection and above all the quality probe: a probe
// that ranked frame rates differently from the downloader would report a
// "quality change" on every 30 s tick and split the recording each time.
// Pinned structurally because the probe runs against the concrete
// *youtube.Service.
//
// Mutant: any call site passing a literal true/false for a video selection.
func TestEveryVideoSelectionCarriesPrefer60fps(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		text := func(n ast.Node) string {
			return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			var arg ast.Expr
			switch {
			case id.Name == "SelectBestDashStream" && len(call.Args) == 6 && text(call.Args[3]) == "true":
				arg = call.Args[5]
			case id.Name == "selectHlsVariant" && len(call.Args) == 4:
				arg = call.Args[3]
			case id.Name == "resolveManifestlessStream" && len(call.Args) > 8 && text(call.Args[6]) == "true":
				arg = call.Args[8]
			default:
				return true
			}
			checked++
			if a := text(arg); !strings.Contains(strings.ToLower(a), "prefer60fps") {
				t.Errorf("%s: %s passes %q for prefer_60fps — a video selection must carry the job's setting",
					fset.Position(call.Pos()), id.Name, a)
			}
			return true
		})
	}
	if checked < 6 {
		t.Errorf("found %d video selections; the pin expects the strategies, the re-selection and both probe arms", checked)
	}
}
