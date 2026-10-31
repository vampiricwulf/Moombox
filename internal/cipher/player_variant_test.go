package cipher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
)

// TestCanonicalPlayerURL pins the rewrite every player URL takes: any variant
// or locale of a player becomes that player's main variant
// (yt-dlp's _construct_player_url, default variant 'main'), the host is kept,
// and a URL with no /s/player/<id>/ segment is left as it is.
//
// Mutants: CanonicalPlayerURL returning its input (every rewriting row
// fails); PlayerIDOf answering for a URL with no /s/player/<hex>/ segment.
func TestCanonicalPlayerURL(t *testing.T) {
	const main = "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js"
	for _, tc := range []struct{ in, want string }{
		{main, main},
		{"https://www.youtube.com/s/player/abcdef12/player_es6.vflset/en_US/base.js", main},
		{"https://www.youtube.com/s/player/abcdef12/player_ias_tce.vflset/en_US/base.js", main},
		{"https://www.youtube.com/s/player/abcdef12/player_ias.vflset/de_DE/base.js", main},
		{"https://www.youtube.com/s/player/abcdef12/tv-player-ias.vflset/tv-player-ias.js", main},
		{"http://127.0.0.1:8080/s/player/ABCDEF12/player_es6.vflset/en_US/base.js", "http://127.0.0.1:8080/s/player/ABCDEF12/player_ias.vflset/en_US/base.js"},
		// Nothing yt-dlp's _extract_player_info would read an id out of.
		{"https://www.youtube.com/s/player/not-hex!/player_es6.vflset/en_US/base.js", "https://www.youtube.com/s/player/not-hex!/player_es6.vflset/en_US/base.js"},
		{"https://example.com/player.js", "https://example.com/player.js"},
		{"", ""},
	} {
		if got := CanonicalPlayerURL(tc.in); got != tc.want {
			t.Errorf("CanonicalPlayerURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := PlayerURLForID("abcdef12"); got != main {
		t.Errorf("PlayerURLForID = %q, want the main variant %q", got, main)
	}
	for in, want := range map[string]string{
		main: "abcdef12",
		"https://www.youtube.com/s/player/not-hex!/base.js": "",
		"https://example.com/player.js":                     "",
	} {
		if got := PlayerIDOf(in); got != want {
			t.Errorf("PlayerIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlayerCacheVariantsDoNotOverwriteEachOther is the disk-cache half of
// audit G50. The page's player URL may name a variant other than main (es6
// here, as during one of YouTube's variant experiments) while the sidecar
// always fetches main (playerURLForID), and the disk entry is keyed by player
// id alone. Both variants were published together, so they share a
// Last-Modified.
//
// Before: the es6 fetch cached the es6 body; the sidecar's main fetch then
// replayed that body's If-Modified-Since, got a 304, and was handed the es6
// code — and a server that answered 200 instead would have overwritten the
// es6 body with main's, a length change read as a rotation (Changed) that
// drops the compiled solver, and so on in turn. Now every fetch asks for
// main: one request, one body, both callers served it, nothing ever changed.
//
// Mutant: the CanonicalPlayerURL call dropped from Fetch (the es6 path is
// requested and its body reaches the main fetch).
func TestPlayerCacheVariantsDoNotOverwriteEachOther(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const (
		mainBody     = "// main variant player JS, the one the sidecar runs"
		es6Body      = "// es6 variant player JS — a different body of a different length"
		lastModified = "Wed, 01 Jan 2025 00:00:00 GMT"
	)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Last-Modified", lastModified)
		if r.Header.Get("If-Modified-Since") == lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := mainBody
		if r.URL.Path == "/s/player/"+testPlayerID+"/player_es6.vflset/en_US/base.js" {
			body = es6Body
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	es6URL := srv.URL + "/s/player/" + testPlayerID + "/player_es6.vflset/en_US/base.js"
	mainURL := srv.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/en_US/base.js"

	pc, err := NewPlayerCache(t.TempDir(), nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}
	ctx := context.Background()

	// STS / goja with the page's es6 URL, then the sidecar with main, then
	// the page's URL again.
	for i, u := range []string{es6URL, mainURL, es6URL} {
		js, changed, err := pc.Fetch(ctx, u)
		if err != nil {
			t.Fatalf("fetch %d (%s): %v", i, u, err)
		}
		if js != mainBody {
			t.Errorf("fetch %d (%s) served %q, want the main variant's body", i, u, js)
		}
		if wantChanged := i == 0; changed != wantChanged {
			t.Errorf("fetch %d (%s): changed = %v, want %v — only the cold fetch may read as new code", i, u, changed, wantChanged)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(paths, "/s/player/"+testPlayerID+"/player_es6.vflset/en_US/base.js") {
		t.Errorf("requested paths %v include the es6 variant; every fetch must ask for main", paths)
	}
	disk, err := os.ReadFile(pc.FilePath(es6URL))
	if err != nil || string(disk) != mainBody {
		t.Errorf("disk entry = %q, %v; want the main variant's body", disk, err)
	}
}
