// One-shot probe to verify the "manifest-free DASH" path works for streams
// where YouTube returned adaptiveFormats[] in the watch page but withheld
// dashManifestUrl from every cookied client (the yt-dlp issue #15274
// experiment). Decrypts the encrypted `n` query param, fetches a POT from
// the running Moombox HTTP endpoint, appends `&sq=0&pot=...&n=...` to the
// adaptiveFormat URL, and reports whether sq=0 returns 200 with bytes.
//
// Not part of the production binary — run via:
//   go run ./tools/manifestless-dash-probe -wp D:\Moombox\_wp.html -itag 299 \
//       -pot https://127.0.0.1:774 -binding 3IyCk5NPX3M
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cipher"
)

type stdLogger struct{}

func (stdLogger) Debug(msg string, args ...any) { fmt.Fprintf(os.Stderr, "[DEBUG] %s %v\n", msg, args) }
func (stdLogger) Info(msg string, args ...any)  { fmt.Fprintf(os.Stderr, "[INFO ] %s %v\n", msg, args) }
func (stdLogger) Warn(msg string, args ...any)  { fmt.Fprintf(os.Stderr, "[WARN ] %s %v\n", msg, args) }
func (stdLogger) Error(msg string, args ...any) { fmt.Fprintf(os.Stderr, "[ERROR] %s %v\n", msg, args) }

var initialPlayerResponseRe = regexp.MustCompile(`ytInitialPlayerResponse\s*=\s*(\{.*?\});`)
var jsURLRe = regexp.MustCompile(`"jsUrl":"([^"]+base\.js)"`)

type playerResp struct {
	StreamingData struct {
		AdaptiveFormats []struct {
			Itag int    `json:"itag"`
			URL  string `json:"url"`
		} `json:"adaptiveFormats"`
	} `json:"streamingData"`
}

func main() {
	wpPath := flag.String("wp", "", "path to watch-page HTML")
	itag := flag.Int("itag", 299, "adaptiveFormat itag to probe")
	potBase := flag.String("pot", "https://127.0.0.1:774", "Moombox base URL for /get_pot")
	binding := flag.String("binding", "", "POT content binding (videoID for the experiment)")
	cookiePath := flag.String("cookies", "", "Netscape cookie file path (optional)")
	useEncryptedN := flag.Bool("raw-n", false, "skip n decryption — useful for testing whether YouTube tolerates raw n")
	flag.Parse()
	if *wpPath == "" || *binding == "" {
		fmt.Fprintln(os.Stderr, "usage: -wp PATH -itag ITAG -binding VIDEOID")
		os.Exit(2)
	}

	htmlBytes, err := os.ReadFile(*wpPath)
	if err != nil {
		fail("read watch page: %v", err)
	}
	html := string(htmlBytes)

	prMatch := initialPlayerResponseRe.FindStringSubmatch(html)
	if prMatch == nil {
		fail("no ytInitialPlayerResponse in watch page")
	}
	var pr playerResp
	if err := json.Unmarshal([]byte(prMatch[1]), &pr); err != nil {
		fail("parse player response: %v", err)
	}

	jsMatch := jsURLRe.FindStringSubmatch(html)
	if jsMatch == nil {
		fail("no jsUrl in watch page")
	}
	playerURL := "https://www.youtube.com" + strings.ReplaceAll(jsMatch[1], `\/`, "/")
	fmt.Printf("player URL: %s\n", playerURL)

	var streamURL string
	for _, f := range pr.StreamingData.AdaptiveFormats {
		if f.Itag == *itag {
			streamURL = f.URL
			break
		}
	}
	if streamURL == "" {
		fail("itag %d not found in adaptiveFormats", *itag)
	}
	fmt.Printf("stream URL prefix: %.150s...\n", streamURL)

	// Build cipher solver
	cacheDir, _ := os.MkdirTemp("", "moombox-probe-cipher-*")
	defer os.RemoveAll(cacheDir)
	resolver, err := cipher.NewGojaResolver(cacheDir, stdLogger{})
	if err != nil {
		fail("NewGojaResolver: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parsed, err := url.Parse(streamURL)
	if err != nil {
		fail("parse URL: %v", err)
	}
	q := parsed.Query()
	encryptedN := q.Get("n")
	if encryptedN == "" {
		fail("URL has no n query param")
	}
	fmt.Printf("encrypted n: %s\n", encryptedN)

	solvers, err := resolver.GetSolvers(ctx, playerURL)
	if err != nil {
		fail("GetSolvers: %v", err)
	}
	if !solvers.HasN() {
		fail("solver does not have N (player extraction failed)")
	}
	decryptedN, err := solvers.DecryptN(encryptedN)
	if err != nil {
		fail("DecryptN: %v", err)
	}
	fmt.Printf("decrypted n: %s\n", decryptedN)

	// Fetch POT from Moombox
	pot := fetchPOT(*potBase, *binding)
	fmt.Printf("POT: %.40s... (len=%d)\n", pot, len(pot))

	// Construct sq=0 URL
	if !*useEncryptedN {
		q.Set("n", decryptedN)
	}
	q.Set("pot", pot)
	q.Set("sq", "0")
	parsed.RawQuery = q.Encode()
	probeURL := parsed.String()

	// Send the request and report status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		fail("build req: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	if *cookiePath != "" {
		if cookieHeader, err := loadCookieHeader(*cookiePath); err == nil && cookieHeader != "" {
			req.Header.Set("Cookie", cookieHeader)
			fmt.Printf("attached cookie header (%d bytes)\n", len(cookieHeader))
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("GET sq=0: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	fmt.Printf("\n=== sq=0 RESPONSE ===\nstatus: %s\nX-Head-Seqnum: %s\nContent-Length: %d (read %d)\n",
		resp.Status, resp.Header.Get("X-Head-Seqnum"), resp.ContentLength, len(body))
	if resp.StatusCode == 200 {
		fmt.Println("\n*** SUCCESS: manifest-free DASH segment fetch works ***")
		fmt.Printf("First 32 bytes: %x\n", body[:min(32, len(body))])
	}
}

func fetchPOT(base, binding string) string {
	body, _ := json.Marshal(map[string]any{"content_binding": binding, "bypass_cache": false})
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	req, _ := http.NewRequest("POST", base+"/get_pot", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fail("POT request: %v", err)
	}
	defer resp.Body.Close()
	var r struct {
		PoToken string `json:"poToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		fail("POT decode: %v", err)
	}
	return r.PoToken
}

// loadCookieHeader reads a Netscape cookie file and produces a single
// `Cookie:` header value. Filters to .youtube.com and youtube.com domains.
func loadCookieHeader(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var pairs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		domain := parts[0]
		if !strings.Contains(domain, "youtube.com") {
			continue
		}
		pairs = append(pairs, parts[5]+"="+parts[6])
	}
	return strings.Join(pairs, "; "), nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
