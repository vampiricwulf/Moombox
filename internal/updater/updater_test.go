package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// silentTestLogger is a no-op logger for updater tests. The package's
// logger interface is anonymous so each test file declares its own.
type silentTestLogger struct{}

func (silentTestLogger) Debug(msg string, args ...any) {}
func (silentTestLogger) Info(msg string, args ...any)  {}
func (silentTestLogger) Warn(msg string, args ...any)  {}
func (silentTestLogger) Error(msg string, args ...any) {}

// newTestUpdater constructs an Updater that uses the given httptest
// origin for both the GitHub API (apiBaseURL) AND the asset downloads
// (the test handler must serve both /repos/... and /assets/... paths).
// exePath points at a tempdir file that ApplyUpdate can rename.
func newTestUpdater(t *testing.T, current string, srv *httptest.Server, verifier func(string, string) error) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	exePath := filepath.Join(dir, "moombox.exe")
	if err := os.WriteFile(exePath, []byte("current binary"), 0o755); err != nil {
		t.Fatalf("seed exe: %v", err)
	}
	if verifier == nil {
		verifier = func(string, string) error { return nil }
	}
	return &Updater{
		currentVersion:  strings.TrimPrefix(current, "v"),
		exePath:         exePath,
		logger:          silentTestLogger{},
		repoOwner:       "test",
		repoName:        "Moombox",
		client:          &http.Client{Timeout: 5 * time.Second},
		apiBaseURL:      srv.URL,
		verifySignature: verifier,
	}, exePath
}

// --- CheckForUpdate: server failure modes ---

// TestCheckForUpdateRateLimited covers the GitHub rate-limit branch
// (HTTP 403 / 429 → friendly "rate limit exceeded" error). Audit
// reports/small-packages.md updater test rate-limited.
func TestCheckForUpdateRateLimited(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(code)
		}))
		t.Cleanup(srv.Close)

		u, _ := newTestUpdater(t, "2.0.15", srv, nil)
		_, err := u.CheckForUpdate(context.Background())
		if err == nil {
			t.Fatalf("CheckForUpdate at HTTP %d: want error, got nil", code)
		}
		if !strings.Contains(err.Error(), "rate limit") {
			t.Errorf("HTTP %d: error should mention rate limit, got %v", code, err)
		}
	}
}

// TestCheckForUpdateOtherHTTPError covers any non-OK / non-rate-limit
// status — the helper should surface the status code without claiming
// it's a rate-limit issue.
func TestCheckForUpdateOtherHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "boom", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "2.0.15", srv, nil)
	_, err := u.CheckForUpdate(context.Background())
	if err == nil {
		t.Fatal("CheckForUpdate at HTTP 503: want error, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should contain 503, got %v", err)
	}
	if strings.Contains(err.Error(), "rate limit") {
		t.Errorf("503 should NOT be classified as rate-limit, got %v", err)
	}
}

// TestCheckForUpdateNoMoomboxAsset covers the "release found but no
// Moombox.exe asset" branch. Audit reports/small-packages.md
// updater test no asset.
func TestCheckForUpdateNoMoomboxAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(githubRelease{
			TagName: "v3.0.0",
			Assets: []githubAsset{
				{Name: "something-else.zip", BrowserDownloadURL: "http://example.com/x.zip"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "2.0.15", srv, nil)
	_, err := u.CheckForUpdate(context.Background())
	if err == nil {
		t.Fatal("CheckForUpdate without Moombox.exe asset: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no Moombox.exe asset") {
		t.Errorf("error should mention missing asset, got %v", err)
	}
}

// TestCheckForUpdateUpToDateReturnsNil covers the "remote ≤ current"
// branch: nil release, nil error. With pre-release support added in
// this commit, a v2.6.0-test.10 client checking against v2.6.0-test.27
// must report an update — not "up to date" — because pre-release
// numeric identifiers compare numerically.
func TestCheckForUpdateUpToDateReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(githubRelease{
			TagName: "v2.0.15",
		})
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "2.0.15", srv, nil)
	got, err := u.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate (up-to-date): %v", err)
	}
	if got != nil {
		t.Errorf("up-to-date: want nil, got %+v", got)
	}
}

// TestCheckForUpdatePrereleaseOrdering verifies the new SemVer-2.0.0
// ordering propagates through CheckForUpdate: a pre-release client
// checking against a higher pre-release tag should see the update.
func TestCheckForUpdatePrereleaseOrdering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(githubRelease{
			TagName: "v2.6.0-test.27",
			Assets: []githubAsset{
				{Name: "Moombox.exe", BrowserDownloadURL: "http://example.com/exe"},
				{Name: "Moombox.exe.sig", BrowserDownloadURL: "http://example.com/sig"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "2.6.0-test.10", srv, nil)
	got, err := u.CheckForUpdate(context.Background())
	if err != nil {
		t.Fatalf("CheckForUpdate (pre-release): %v", err)
	}
	if got == nil {
		t.Fatal("test.10 → test.27 should be an update; CompareVersions ordering broken?")
	}
	if got.TagName != "v2.6.0-test.27" {
		t.Errorf("ReleaseInfo.TagName: want v2.6.0-test.27, got %q", got.TagName)
	}
}

// --- downloadFile: oversize + HTTP error ---

// TestDownloadFileRejectsOversize covers the maxDownloadSize cap. We
// serve a 201 MB body (1 MB over the 200 MB cap) and assert the helper
// errors with a clear message rather than writing the full payload.
func TestDownloadFileRejectsOversize(t *testing.T) {
	if testing.Short() {
		t.Skip("oversize test allocates 201 MB; skipping in -short mode")
	}
	const overSize = 201 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		// Stream zero-bytes; cheaper than allocating a 201MB buffer.
		buf := make([]byte, 1<<20)
		for i := 0; i <= 200; i++ {
			rw.Write(buf)
		}
		rw.Write(buf[:1])
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "1.0.0", srv, nil)
	dest := filepath.Join(t.TempDir(), "big.bin")
	err := u.downloadFile(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("downloadFile on 201MB body: want size-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Errorf("error should mention size limit, got %v", err)
	}
	_ = overSize // intentionally unused — kept as a documentation constant
}

// TestDownloadFileHTTPError covers the non-200 branch.
func TestDownloadFileHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "missing", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "1.0.0", srv, nil)
	dest := filepath.Join(t.TempDir(), "bin")
	err := u.downloadFile(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("downloadFile on 404: want error, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should contain 404, got %v", err)
	}
}

// TestDownloadFileHTMLErrorPage covers yt-dlp #17550: GitHub can answer a
// release-asset URL with an HTML error page under HTTP 200 (a stale asset
// link, a CDN blip). Before this fix that "succeeded" as a download and
// only failed later, confusingly, at ed25519 verification. downloadFile
// must instead name the real problem and leave nothing at dest.
//
// Four bodies guard looksLikeHTML's case-insensitivity and its BOM/
// whitespace trimming, not just the one exact shape GitHub happens to send
// today: an uppercase doctype, a lowercase doctype, a bare <html> tag
// preceded by leading whitespace and no doctype at all, and a UTF-8 BOM
// ahead of an uppercase doctype.
func TestDownloadFileHTMLErrorPage(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "uppercase doctype",
			body: []byte("<!DOCTYPE html>\n<html><head><title>Error</title></head><body>Not Found</body></html>"),
		},
		{
			name: "lowercase doctype",
			body: []byte("<!doctype html>\n<html><body>Not Found</body></html>"),
		},
		{
			name: "leading whitespace, no doctype",
			body: []byte("\n\n  <html><body>404</body></html>"),
		},
		{
			name: "UTF-8 BOM prefix",
			body: append([]byte{0xEF, 0xBB, 0xBF}, []byte("<!DOCTYPE html><html><body>Not Found</body></html>")...),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Type", "text/html")
				rw.WriteHeader(http.StatusOK)
				rw.Write(tc.body)
			}))
			t.Cleanup(srv.Close)

			u, _ := newTestUpdater(t, "1.0.0", srv, nil)
			dest := filepath.Join(t.TempDir(), "moombox.exe.new")
			err := u.downloadFile(context.Background(), srv.URL, dest)
			if err == nil {
				t.Fatal("downloadFile on an HTML 200 body: want an error, got nil")
			}
			if !strings.Contains(err.Error(), "error page") {
				t.Errorf("error should name a GitHub error page, got %v", err)
			}
			if strings.Contains(err.Error(), "signature") {
				t.Errorf("error should not blame signature verification, got %v", err)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Errorf("downloadFile on an HTML body should leave no file at dest, stat err = %v", statErr)
			}
		})
	}
}

// TestDownloadFileSmallBinaryIntact is the sniff's negative case: a small
// (well under the 512-byte sniff window) non-HTML body must still download
// byte-for-byte, proving the sniffed prefix is written to dest along with
// the rest rather than being consumed and dropped.
func TestDownloadFileSmallBinaryIntact(t *testing.T) {
	payload := []byte{0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		rw.Write(payload)
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "1.0.0", srv, nil)
	dest := filepath.Join(t.TempDir(), "moombox.exe.new")
	if err := u.downloadFile(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("downloadFile on a small binary body: want nil, got %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("downloaded content mismatch: got %x, want %x", got, payload)
	}
}

// --- CleanupOldBinary ---

// TestCleanupOldBinaryRemovesStaleArtifacts covers all four suffixes
// the helper sweeps (.old, .new, .new.sig, .sig). The .update-broken
// marker is intentionally preserved.
func TestCleanupOldBinaryRemovesStaleArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	stale := []string{exePath + ".old", exePath + ".new", exePath + ".new.sig", exePath + ".sig"}
	if runtime.GOOS == "windows" {
		stale = append(stale, exePath+"~")
	}
	preserved := exePath + ".update-broken"
	for _, p := range append(stale, preserved) {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	u.CleanupOldBinary()

	for _, p := range stale {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale file not removed: %s", p)
		}
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Errorf("preserved marker was deleted: %v", err)
	}
}

// --- ApplyUpdate: end-to-end happy + rollback ---

// TestApplyUpdateEndToEnd covers the full success path: the updater
// downloads .new + .new.sig, verifies the (stub) signature, swaps the
// running exe via .old → exe, .new → exe, and removes the .sig.
func TestApplyUpdateEndToEnd(t *testing.T) {
	const newBody = "fresh moombox binary"

	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, newBody)
	})
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(make([]byte, ed25519.SignatureSize))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	verifyCalls := atomic.Int32{}
	verifier := func(binPath, sigPath string) error {
		verifyCalls.Add(1)
		// Sanity: both files must exist when verify runs.
		if _, err := os.Stat(binPath); err != nil {
			return fmt.Errorf("verifier: binary missing: %w", err)
		}
		if _, err := os.Stat(sigPath); err != nil {
			return fmt.Errorf("verifier: signature missing: %w", err)
		}
		return nil
	}
	u, exePath := newTestUpdater(t, "1.0.0", srv, verifier)

	err := u.ApplyUpdate(context.Background(), &ReleaseInfo{
		Version:      "2.0.0",
		TagName:      "v2.0.0",
		DownloadURL:  srv.URL + "/exe",
		SignatureURL: srv.URL + "/sig",
	})
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	// Post-conditions: new binary at exePath, .old contains the original,
	// no .new or .sig leftovers.
	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read post-update exe: %v", err)
	}
	if string(got) != newBody {
		t.Errorf("exe contents: want %q, got %q", newBody, string(got))
	}
	if old, err := os.ReadFile(exePath + ".old"); err != nil {
		t.Errorf("old binary missing: %v", err)
	} else if string(old) != "current binary" {
		t.Errorf(".old contents: want %q, got %q", "current binary", string(old))
	}
	for _, suffix := range []string{".new", ".new.sig", ".sig"} {
		if _, err := os.Stat(exePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stray %s file: %v", suffix, err)
		}
	}
	// The pending-version breadcrumb records the tag this install updated TO,
	// for the post-restart boot (a successful boot deletes it; a rolled-back
	// boot marks it skipped).
	if pending, err := os.ReadFile(exePath + PendingVersionSuffix); err != nil {
		t.Errorf("pending-version breadcrumb missing: %v", err)
	} else if string(pending) != "v2.0.0" {
		t.Errorf("pending breadcrumb: want %q, got %q", "v2.0.0", string(pending))
	}
	if got := verifyCalls.Load(); got != 1 {
		t.Errorf("verifier calls: want 1, got %d", got)
	}
}

// TestApplyUpdateRollbackOnVerifyFailure covers the rollback path:
// signature verification fails → .new is removed → original exe is
// untouched.
func TestApplyUpdateRollbackOnVerifyFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, "tampered binary")
	})
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(make([]byte, ed25519.SignatureSize))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	verifier := func(string, string) error {
		return errors.New("signature mismatch")
	}
	u, exePath := newTestUpdater(t, "1.0.0", srv, verifier)

	err := u.ApplyUpdate(context.Background(), &ReleaseInfo{
		Version:      "2.0.0",
		DownloadURL:  srv.URL + "/exe",
		SignatureURL: srv.URL + "/sig",
	})
	if err == nil {
		t.Fatal("ApplyUpdate with bad signature: want error, got nil")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Errorf("error: want signature-verification phrasing, got %v", err)
	}
	// Original exe must still exist and be unchanged.
	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("read exe after failed apply: %v", err)
	}
	if string(got) != "current binary" {
		t.Errorf("exe was modified despite verify failure: got %q", string(got))
	}
	// .new + .sig must be cleaned up.
	for _, suffix := range []string{".new", ".sig"} {
		if _, err := os.Stat(exePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s leaked after verify failure: %v", suffix, err)
		}
	}
}

// TestApplyUpdateRefusesMissingSignature covers the explicit refusal
// when ReleaseInfo.SignatureURL is empty — never apply unsigned binaries.
func TestApplyUpdateRefusesMissingSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, "x")
	}))
	t.Cleanup(srv.Close)

	u, _ := newTestUpdater(t, "1.0.0", srv, nil)

	err := u.ApplyUpdate(context.Background(), &ReleaseInfo{
		Version:      "2.0.0",
		DownloadURL:  srv.URL,
		SignatureURL: "",
	})
	if err == nil {
		t.Fatal("ApplyUpdate without signature URL: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no signature URL") {
		t.Errorf("error: want 'no signature URL' phrasing, got %v", err)
	}
}

// _ keeps signing-package round-trip imports referenced even when the
// happy-path test stubs the verifier rather than using a real signature.
var _ = func() string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return hex.EncodeToString(pub)
}

// --- stripDownloadLinks ---

func TestStripDownloadLinks(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{
			name:  "with separator",
			input: "[**`Download Foo`**](url)\n\n---\n\n## Features\n- thing",
			want:  "## Features\n- thing",
		},
		{
			name:  "with multiple links",
			input: "[A](u)\n[B](u2)\n\n---\n\n# Notes",
			want:  "# Notes",
		},
		{
			name:  "no separator returns unchanged",
			input: "## Just notes\n- thing",
			want:  "## Just notes\n- thing",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripDownloadLinks(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- renderReleaseNotesHtml ---

func TestRenderReleaseNotesHtmlSanitizesScripts(t *testing.T) {
	html := renderReleaseNotesHtml("## OK\n\n<script>alert(1)</script>")
	if strings.Contains(html, "<script>") {
		t.Errorf("script tag survived sanitization: %s", html)
	}
	if !strings.Contains(html, "OK") {
		t.Errorf("legitimate content stripped: %s", html)
	}
}

func TestRenderReleaseNotesHtmlRendersHeading(t *testing.T) {
	html := renderReleaseNotesHtml("## Heading\n- bullet")
	if !strings.Contains(html, "<h2>") {
		t.Errorf("expected <h2> in rendered output: %s", html)
	}
	if !strings.Contains(html, "<li>") {
		t.Errorf("expected <li> in rendered output: %s", html)
	}
}

// --- assetsForPlatform / releaseAssetMap ---

// TestAssetsForPlatformKnownPlatforms verifies the lookup table covers all
// three supported platforms with the correct binary and sig asset names.
func TestAssetsForPlatformKnownPlatforms(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantBinary   string
		wantSig      string
	}{
		{"windows", "amd64", "Moombox.exe", "Moombox.exe.sig"},
		{"linux", "amd64", "moombox-linux-amd64", "moombox-linux-amd64.sig"},
		{"linux", "arm64", "moombox-linux-arm64", "moombox-linux-arm64.sig"},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			a, ok := assetsForPlatform(tc.goos, tc.goarch)
			if !ok {
				t.Fatalf("expected assets for %s/%s, got !ok", tc.goos, tc.goarch)
			}
			if a.binary != tc.wantBinary {
				t.Errorf("binary: got %q, want %q", a.binary, tc.wantBinary)
			}
			if a.sig != tc.wantSig {
				t.Errorf("sig: got %q, want %q", a.sig, tc.wantSig)
			}
		})
	}
}

// --- FetchReleaseNotes ---

func TestFetchReleaseNotesStripsAndRenders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/tags/v2.6.3") {
			t.Errorf("unexpected URL: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(githubRelease{
			TagName:     "v2.6.3",
			Body:        "[**`Download`**](url)\n\n---\n\n## Features\n- Linux support",
			PublishedAt: "2026-05-03T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	u, _ := newTestUpdater(t, "2.6.3", srv, nil)

	info, err := u.FetchReleaseNotes(context.Background(), "2.6.3")
	if err != nil {
		t.Fatalf("FetchReleaseNotes: %v", err)
	}
	if info.TagName != "v2.6.3" {
		t.Errorf("TagName: got %q, want v2.6.3", info.TagName)
	}
	if strings.Contains(info.ReleaseNotes, "Download") {
		t.Errorf("download link should be stripped: %q", info.ReleaseNotes)
	}
	if !strings.Contains(info.ReleaseNotesHtml, "<h2>") {
		t.Errorf("HTML should contain rendered heading: %q", info.ReleaseNotesHtml)
	}
}

func TestFetchReleaseNotesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)
	u, _ := newTestUpdater(t, "0.0.1", srv, nil)

	_, err := u.FetchReleaseNotes(context.Background(), "0.0.1")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "no release found") {
		t.Errorf("error should mention 'no release found', got %v", err)
	}
}

// TestAssetsForPlatformUnknown verifies that unsupported OS/arch combinations
// return !ok rather than panicking or returning garbage.
func TestAssetsForPlatformUnknown(t *testing.T) {
	if _, ok := assetsForPlatform("plan9", "amd64"); ok {
		t.Error("expected !ok for unknown platform")
	}
	if _, ok := assetsForPlatform("linux", "mips"); ok {
		t.Error("expected !ok for unsupported arch")
	}
}
