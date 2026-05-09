package cipher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testPlayerID = "abcdef12"

func testPlayerURL(playerID string) string {
	return "https://www.youtube.com/s/player/" + playerID + "/player_ias.vflset/en_US/base.js"
}

// fakePlayerServer hosts a fake YouTube player JS endpoint that conforms
// to RFC 7232: returns 304 when If-Modified-Since matches the configured
// Last-Modified, otherwise 200 with the configured body + headers.
type fakePlayerServer struct {
	mu           sync.Mutex
	body         string
	lastModified string
	etag         string
	hits         atomic.Int32
	server       *httptest.Server
}

func newFakePlayerServer(body, lastModified, etag string) *fakePlayerServer {
	fs := &fakePlayerServer{
		body:         body,
		lastModified: lastModified,
		etag:         etag,
	}
	fs.server = httptest.NewServer(http.HandlerFunc(fs.handle))
	return fs
}

func (fs *fakePlayerServer) URL() string { return fs.server.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/en_US/base.js" }

func (fs *fakePlayerServer) Hits() int32 { return fs.hits.Load() }

func (fs *fakePlayerServer) Set(body, lastModified, etag string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.body = body
	fs.lastModified = lastModified
	fs.etag = etag
}

func (fs *fakePlayerServer) Close() { fs.server.Close() }

func (fs *fakePlayerServer) handle(w http.ResponseWriter, r *http.Request) {
	fs.hits.Add(1)
	fs.mu.Lock()
	body, lm, etag := fs.body, fs.lastModified, fs.etag
	fs.mu.Unlock()

	ifNoneMatch := r.Header.Get("If-None-Match")
	ifModifiedSince := r.Header.Get("If-Modified-Since")

	if etag != "" && ifNoneMatch == etag {
		if lm != "" {
			w.Header().Set("Last-Modified", lm)
		}
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if lm != "" && ifModifiedSince == lm {
		w.Header().Set("Last-Modified", lm)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if lm != "" {
		w.Header().Set("Last-Modified", lm)
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Length", itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func itoa(n int) string {
	// Avoid pulling in strconv just for tests; use fmt-free path.
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// usePlayerHTTPClient swaps the package-global playerHTTPClient for the
// duration of a test so requests go to the test server instead of YouTube.
// Returns a restore function.
func usePlayerHTTPClient(t *testing.T) func() {
	t.Helper()
	orig := playerHTTPClient
	playerHTTPClient = http.DefaultClient
	return func() { playerHTTPClient = orig }
}

// TestFetch_304_ReturnsCached — server returns 304; Fetch returns cached
// content with Changed=false; mtime is refreshed; meta.json unchanged.
func TestFetch_304_ReturnsCached(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const oldBody = "// old player JS"
	fs := newFakePlayerServer(oldBody, "Wed, 01 Jan 2025 00:00:00 GMT", `"v1"`)
	defer fs.Close()

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	// Prime the cache with a 200 response.
	js1, changed1, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if !changed1 {
		t.Fatalf("first Fetch: want Changed=true (cold cache), got false")
	}
	if js1 != oldBody {
		t.Fatalf("first Fetch body: want %q got %q", oldBody, js1)
	}

	// Backdate the cache file so mtime refresh is observable.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pc.FilePath(fs.URL()), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	metaBefore, _ := os.ReadFile(pc.FilePathMeta(fs.URL()))

	// Second Fetch: server has not rotated; expect 304.
	js2, changed2, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if changed2 {
		t.Errorf("304 path: want Changed=false, got true")
	}
	if js2 != oldBody {
		t.Errorf("304 path body: want %q got %q", oldBody, js2)
	}

	info, err := os.Stat(pc.FilePath(fs.URL()))
	if err != nil {
		t.Fatalf("stat after 304: %v", err)
	}
	if !info.ModTime().After(old.Add(30 * time.Minute)) {
		t.Errorf("304 path mtime: want refreshed past %v, got %v", old, info.ModTime())
	}

	metaAfter, _ := os.ReadFile(pc.FilePathMeta(fs.URL()))
	if string(metaBefore) != string(metaAfter) {
		t.Errorf("304 path: meta should be unchanged, before=%s after=%s", metaBefore, metaAfter)
	}
}

// TestFetch_200_DifferentSize_RotatesCache — server returns 200 with
// different body/length; Fetch evicts the old preprocessed sidecar,
// stores new content, returns Changed=true.
func TestFetch_200_DifferentSize_RotatesCache(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const oldBody = "// old player JS"
	const newBody = "// new player JS with materially different content padding to grow"

	fs := newFakePlayerServer(oldBody, "Wed, 01 Jan 2025 00:00:00 GMT", `"v1"`)
	defer fs.Close()

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	if _, _, err := pc.Fetch(context.Background(), fs.URL()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Plant a preprocessed sidecar — it should be removed on rotation.
	if err := pc.PutPreprocessed(fs.URL(), "preprocessed-from-old-js"); err != nil {
		t.Fatalf("PutPreprocessed: %v", err)
	}

	// Server rotates.
	fs.Set(newBody, "Thu, 02 Jan 2025 00:00:00 GMT", `"v2"`)

	js, changed, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("rotated Fetch: %v", err)
	}
	if !changed {
		t.Errorf("rotated Fetch: want Changed=true, got false")
	}
	if js != newBody {
		t.Errorf("rotated body: want %q got %q", newBody, js)
	}

	// Preprocessed must be gone.
	if _, err := os.Stat(pc.FilePathPreprocessed(fs.URL())); !os.IsNotExist(err) {
		t.Errorf("preprocessed.js should be removed on rotation, stat err = %v", err)
	}

	// Meta should reflect new headers.
	meta := pc.readMeta(fs.URL())
	if meta.LastModified != "Thu, 02 Jan 2025 00:00:00 GMT" {
		t.Errorf("meta.LastModified after rotation: got %q", meta.LastModified)
	}
	if meta.ETag != `"v2"` {
		t.Errorf("meta.ETag after rotation: got %q", meta.ETag)
	}
	if meta.ContentLength != int64(len(newBody)) {
		t.Errorf("meta.ContentLength after rotation: got %d want %d", meta.ContentLength, len(newBody))
	}
}

// TestFetch_200_SameSize_TreatedAsUnchanged — Last-Modified flap defense:
// 200 with identical Content-Length is treated as unchanged content even
// when Last-Modified differs.
func TestFetch_200_SameSize_TreatedAsUnchanged(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const body = "// player JS body that does not change"
	fs := newFakePlayerServer(body, "Wed, 01 Jan 2025 00:00:00 GMT", "")
	defer fs.Close()

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	if _, _, err := pc.Fetch(context.Background(), fs.URL()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Server flaps Last-Modified (CDN edge inconsistency) but content is
	// byte-identical.
	fs.Set(body, "Thu, 02 Jan 2025 00:00:00 GMT", "")

	js, changed, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("flap Fetch: %v", err)
	}
	if changed {
		t.Errorf("Last-Modified flap with identical size: want Changed=false, got true")
	}
	if js != body {
		t.Errorf("flap body: want %q got %q", body, js)
	}

	// Meta should still update so the next conditional GET uses the
	// current Last-Modified value.
	meta := pc.readMeta(fs.URL())
	if meta.LastModified != "Thu, 02 Jan 2025 00:00:00 GMT" {
		t.Errorf("flap meta.LastModified: got %q want updated", meta.LastModified)
	}
}

// TestFetch_Migration_NoMetaUsesFileMtime — when meta.json is absent
// (post-upgrade or first cold fetch), conditional GET still works by
// replaying file mtime as If-Modified-Since.
func TestFetch_Migration_NoMetaUsesFileMtime(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const body = "// migrated cached body"

	// Server with no Last-Modified configured: it'll only honor 304 via
	// our If-Modified-Since header against the formatted mtime.
	mtime := time.Now().Add(-1 * time.Hour).UTC()
	mtimeStr := mtime.Format(http.TimeFormat)
	fs := newFakePlayerServer(body, mtimeStr, "")
	defer fs.Close()

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	// Plant a cached file directly, bypassing Fetch — simulates pre-Stage-1
	// state where no meta.json exists.
	if err := pc.Put(fs.URL(), body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Set its mtime to match what the server claims as Last-Modified so the
	// migration path's mtime-as-If-Modified-Since produces a 304.
	if err := os.Chtimes(pc.FilePath(fs.URL()), mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// Sanity: meta.json is absent.
	if _, err := os.Stat(pc.FilePathMeta(fs.URL())); !os.IsNotExist(err) {
		t.Fatalf("meta.json should be absent for migration test, got err=%v", err)
	}

	js, changed, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("migration Fetch: %v", err)
	}
	if changed {
		t.Errorf("migration: want Changed=false (304), got true")
	}
	if js != body {
		t.Errorf("migration body: want %q got %q", body, js)
	}
}

// TestFetch_NetworkFailure_ServesCached — when revalidation fails (server
// unreachable) and a cache exists, return the cached entry rather than
// crater the caller.
func TestFetch_NetworkFailure_ServesCached(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const body = "// cached player JS"
	fs := newFakePlayerServer(body, "Wed, 01 Jan 2025 00:00:00 GMT", "")

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	if _, _, err := pc.Fetch(context.Background(), fs.URL()); err != nil {
		t.Fatalf("prime Fetch: %v", err)
	}

	// Kill the server; next Fetch must hit the network-error fallback.
	fs.Close()

	js, changed, err := pc.Fetch(context.Background(), fs.URL())
	if err != nil {
		t.Fatalf("offline Fetch: want nil err (cache fallback), got %v", err)
	}
	if changed {
		t.Errorf("offline Fetch: want Changed=false, got true")
	}
	if js != body {
		t.Errorf("offline body: want %q got %q", body, js)
	}
}

// TestFetch_Singleflight_CoalescesByCacheKey — concurrent Fetches for the
// same player (even via locale-variant URLs) share a single round-trip.
func TestFetch_Singleflight_CoalescesByCacheKey(t *testing.T) {
	defer usePlayerHTTPClient(t)()

	const body = "// shared player JS"
	fs := newFakePlayerServer(body, "Wed, 01 Jan 2025 00:00:00 GMT", "")
	defer fs.Close()

	// Build three locale variants of the same URL — singleflight key is
	// CacheKey(playerURL) which strips the locale.
	baseURL := strings.TrimSuffix(fs.URL(), "en_US/base.js")
	urls := []string{
		baseURL + "en_US/base.js",
		baseURL + "en_GB/base.js",
		baseURL + "de/base.js",
	}

	// Add latency so concurrent calls overlap inside fetchInternal.
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
		w.Header().Set("Content-Length", itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer slowServer.Close()

	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	// Rebuild URLs against the slow server's base.
	urls = []string{
		slowServer.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/en_US/base.js",
		slowServer.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/en_GB/base.js",
		slowServer.URL + "/s/player/" + testPlayerID + "/player_ias.vflset/de/base.js",
	}

	startHits := fs.Hits()

	const concurrency = 5
	var wg sync.WaitGroup
	wg.Add(concurrency * len(urls))
	for i := 0; i < concurrency; i++ {
		for _, u := range urls {
			go func(u string) {
				defer wg.Done()
				_, _, err := pc.Fetch(context.Background(), u)
				if err != nil {
					t.Errorf("concurrent Fetch %s: %v", u, err)
				}
			}(u)
		}
	}
	wg.Wait()

	// 5 concurrent × 3 locale variants = 15 Fetch calls, but they all
	// collapse to one CacheKey, so we expect exactly 1 round-trip.
	hits := fs.Hits() - startHits
	if hits != 1 {
		t.Errorf("singleflight coalescing: want 1 round-trip across 15 concurrent Fetches, got %d", hits)
	}
}
