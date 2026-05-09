// One-shot sig probe: spawn an isolated sidecar instance, load the player
// JS for a given video's player, ask the sidecar to solve a real
// signatureCipher encountered in the wild. Prints whether sidecar
// Sig succeeds and what it returns. Useful for diagnosing why production
// sees "cipher: sig unavailable for this player" warnings — if THIS tool
// succeeds against the same player, the production sidecar instance has a
// state issue rather than a fundamental ejs/player-shape mismatch.
//
// Run via:
//   go run ./tools/sidecar-sig-probe -wp PATH_TO_WATCH_PAGE.html
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

type stdLogger struct{}

func (stdLogger) Debug(msg string, args ...any) { fmt.Fprintf(os.Stderr, "[DEBUG] %s %v\n", msg, args) }
func (stdLogger) Info(msg string, args ...any)  { fmt.Fprintf(os.Stderr, "[INFO ] %s %v\n", msg, args) }
func (stdLogger) Warn(msg string, args ...any)  { fmt.Fprintf(os.Stderr, "[WARN ] %s %v\n", msg, args) }
func (stdLogger) Error(msg string, args ...any) { fmt.Fprintf(os.Stderr, "[ERROR] %s %v\n", msg, args) }

var (
	jsURLRe = regexp.MustCompile(`"jsUrl":"([^"]+base\.js)"`)
	playerIDRe = regexp.MustCompile(`/s/player/([a-f0-9]+)/`)
)

func main() {
	wpPath := flag.String("wp", "", "watch-page HTML path")
	playerJSOverride := flag.String("player-js", "", "use this local file as the player JS instead of fetching")
	flag.Parse()
	if *wpPath == "" {
		fail("usage: -wp PATH_TO_WATCH_PAGE.html")
	}

	htmlBytes, err := os.ReadFile(*wpPath)
	if err != nil {
		fail("read watch page: %v", err)
	}
	html := string(htmlBytes)

	jsMatch := jsURLRe.FindStringSubmatch(html)
	if jsMatch == nil {
		fail("no jsUrl in watch page")
	}
	playerPath := strings.ReplaceAll(jsMatch[1], `\/`, "/")
	playerURL := "https://www.youtube.com" + playerPath
	idMatch := playerIDRe.FindStringSubmatch(playerURL)
	if idMatch == nil {
		fail("could not extract playerID from URL: %s", playerURL)
	}
	playerID := idMatch[1]
	// Production reconstructs the player URL via playerURLForID, which always
	// uses the `player_ias.vflset` variant regardless of what the watch page
	// actually shipped (`player_es6`, `player_es6_tce`, etc). Mirror that
	// here so the probe exercises the same fetch as the production path.
	productionPlayerURL := fmt.Sprintf("https://www.youtube.com/s/player/%s/player_ias.vflset/en_US/base.js", playerID)
	fmt.Printf("playerID: %s\n", playerID)
	fmt.Printf("watch-page player URL: %s\n", playerURL)
	fmt.Printf("production-style URL : %s\n", productionPlayerURL)
	playerURL = productionPlayerURL

	// Pull a real signatureCipher 's' value out of the watch page.
	// streamingData.formats[].signatureCipher = "url=...&s=ENCRYPTED&sp=sig"
	scMatch := regexp.MustCompile(`"signatureCipher":"((?:[^"\\]|\\.)+)"`).FindStringSubmatch(html)
	if scMatch == nil {
		fail("no signatureCipher in watch page (stream may have only direct URLs)")
	}
	sigCipher := strings.ReplaceAll(scMatch[1], `&`, "&")
	parsed, err := url.ParseQuery(sigCipher)
	if err != nil {
		fail("parse signatureCipher: %v", err)
	}
	encS := parsed.Get("s")
	if encS == "" {
		fail("signatureCipher has no s param")
	}
	fmt.Printf("encrypted sig (len=%d): %s\n", len(encS), encS[:min(60, len(encS))]+"...")

	// Fetch player JS — or use a local override (useful for replaying a
	// stale cached version against the current sidecar to test whether
	// the failure is content-mismatch).
	var playerJS string
	if *playerJSOverride != "" {
		b, err := os.ReadFile(*playerJSOverride)
		if err != nil {
			fail("read player-js override: %v", err)
		}
		playerJS = string(b)
		fmt.Printf("player JS source: file %s\n", *playerJSOverride)
	} else {
		playerJS = fetchPlayerJS(playerURL)
		fmt.Printf("player JS source: fetched\n")
	}
	fmt.Printf("player JS size: %d bytes\n", len(playerJS))

	// Spawn an isolated sidecar
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cacheDir, _ := os.MkdirTemp("", "moombox-sig-probe-*")
	defer os.RemoveAll(cacheDir)
	sc := sidecar.New(sidecar.Config{
		CacheDir:       cacheDir,
		Logger:         stdLogger{},
		ExposeGC:       false,
		V8HardLimitMB:  512,
		StartupTimeout: 60 * time.Second,
	})
	if err := sc.Start(ctx); err != nil {
		fail("sidecar Start: %v", err)
	}
	defer sc.Stop()

	if !sc.IsHealthy() {
		fail("sidecar not healthy after Start")
	}
	fmt.Println("sidecar ready")

	// Solve sig
	res, err := sc.SolveCipher(ctx, sidecar.SolveCipherRequest{
		PlayerID:      playerID,
		PlayerJS:      playerJS,
		SigChallenges: []string{encS},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n*** sidecar SolveCipher returned ERROR: %v ***\n", err)
		os.Exit(1)
	}
	out, ok := res.SigResults[encS]
	if !ok {
		fmt.Fprintf(os.Stderr, "\n*** sidecar returned no result for the input encrypted sig ***\n")
		fmt.Fprintf(os.Stderr, "    SigResults keys: %v\n", mapKeys(res.SigResults))
		os.Exit(1)
	}
	fmt.Printf("\n*** SUCCESS ***\n")
	fmt.Printf("decrypted sig: %s\n", out)
	fmt.Printf("input  length: %d\n", len(encS))
	fmt.Printf("output length: %d\n", len(out))
	fmt.Printf("input  prefix: %s\n", encS[:min(40, len(encS))])
	fmt.Printf("output prefix: %s\n", out[:min(40, len(out))])
}

func fetchPlayerJS(playerURL string) string {
	req, _ := http.NewRequest("GET", playerURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("fetch player JS: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k[:min(20, len(k))])
	}
	return keys
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
