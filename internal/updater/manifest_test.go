package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// platformManifest returns a manifest for version/tag listing every
// releaseAssetMap platform, the way BuildManifest does, with the running
// platform's entry hashing to body. Skips on a platform the updater does not
// support — ApplyUpdate refuses there before any of this matters.
func platformManifest(t *testing.T, version, tag string, body []byte) *Manifest {
	t.Helper()
	assets, ok := currentPlatformAssets()
	if !ok {
		t.Skipf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	m := &Manifest{Version: version, Tag: tag, Platforms: map[string]ManifestPlatform{}}
	for key, a := range releaseAssetMap {
		other := sha256.Sum256([]byte("the " + key + " binary"))
		m.Platforms[key] = ManifestPlatform{Asset: a.binary, SHA256: hex.EncodeToString(other[:])}
	}
	sum := sha256.Sum256(body)
	m.Platforms[runtime.GOOS+"/"+runtime.GOARCH] = ManifestPlatform{Asset: assets.binary, SHA256: hex.EncodeToString(sum[:])}
	return m
}

func manifestJSON(t *testing.T, m *Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return data
}

// serveManifest registers /manifest and /manifest.sig. The signature is 64
// zero bytes, for the tests that stub verifySignature; signedRelease below
// serves a real one.
func serveManifest(mux *http.ServeMux, data []byte) {
	mux.HandleFunc("/manifest", func(rw http.ResponseWriter, _ *http.Request) { rw.Write(data) })
	mux.HandleFunc("/manifest.sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(make([]byte, ed25519.SignatureSize))
	})
}

// signedRelease is a release whose manifest and binary carry REAL signatures,
// by a test key the updater is made to trust. exeHits counts binary
// downloads, so a test can show a refusal came before one.
type signedRelease struct {
	srv     *httptest.Server
	pub     ed25519.PublicKey
	exeHits atomic.Int32
}

// newSignedRelease serves binary (signed by priv) at /exe and manifest
// (signed by manifestKey, which may differ — the stranger-key case) at
// /manifest.
func newSignedRelease(t *testing.T, binary, manifest []byte, manifestKey ed25519.PrivateKey) *signedRelease {
	t.Helper()
	pub, priv := generateTestKeyPair(t)
	if manifestKey == nil {
		manifestKey = priv
	}
	sr := &signedRelease{pub: pub}
	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, _ *http.Request) {
		sr.exeHits.Add(1)
		rw.Write(binary)
	})
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) { rw.Write(ed25519.Sign(priv, binary)) })
	mux.HandleFunc("/manifest", func(rw http.ResponseWriter, _ *http.Request) { rw.Write(manifest) })
	mux.HandleFunc("/manifest.sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(ed25519.Sign(manifestKey, manifest))
	})
	sr.srv = httptest.NewServer(mux)
	t.Cleanup(sr.srv.Close)
	return sr
}

// updater returns an Updater running `current` that trusts sr's key.
func (sr *signedRelease) updater(t *testing.T, current string) (*Updater, string) {
	t.Helper()
	return newTestUpdater(t, current, sr.srv, func(bin, sig string) error {
		return verifySignatureWithKey(sr.pub, bin, sig)
	})
}

func (sr *signedRelease) release(version, tag string) *ReleaseInfo {
	return &ReleaseInfo{
		Version: version, TagName: tag,
		DownloadURL: sr.srv.URL + "/exe", SignatureURL: sr.srv.URL + "/sig",
		ManifestURL: sr.srv.URL + "/manifest", ManifestSignatureURL: sr.srv.URL + "/manifest.sig",
		ReleaseURL: "https://github.com/vampiricwulf/Moombox/releases/tag/" + tag,
	}
}

// applyRefused applies release over a running `current` that trusts sr's key
// and requires a refusal containing wantErr that left the running binary and
// its directory as they were, after wantDownloads binary downloads. Returns
// the refusal.
func applyRefused(t *testing.T, sr *signedRelease, current string, release *ReleaseInfo, wantErr string, wantDownloads int32) error {
	t.Helper()
	u, exePath := sr.updater(t, current)
	err := u.ApplyUpdate(context.Background(), release)
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("ApplyUpdate = %v, want a refusal containing %q", err, wantErr)
	}
	if !fileIs(exePath, "current binary") {
		t.Error("a refused update changed the running binary")
	}
	for _, suffix := range []string{".new", ".new.sig", ".old", PendingVersionSuffix} {
		if _, statErr := os.Stat(exePath + suffix); statErr == nil {
			t.Errorf("%s left behind by a refused update", suffix)
		}
	}
	if got := sr.exeHits.Load(); got != wantDownloads {
		t.Errorf("binary downloaded %d times, want %d", got, wantDownloads)
	}
	return err
}

// TestApplyUpdateBindsTheBinaryToTheSignedManifest drives ApplyUpdate with
// real signatures: the genuine case, and each way a validly SIGNED artifact
// used to pass while being the wrong one (D-U4). A per-binary signature says
// only that the key signed these bytes, so an older release's binary, or
// another platform's, verified as well as the right one.
//
// THE MUTANTS, each failing the row named:
//   - ApplyUpdate without verifiedManifestEntry (or one returning no error
//     for an empty ManifestURL) — "no manifest";
//   - the manifest's signature not verified — "stranger's key";
//   - the version comparison dropped from entryFor — "this tag, another
//     version" (the "older release's manifest replayed" row differs in tag
//     as well, so the tag comparison alone refuses it);
//   - the tag comparison dropped — "another tag";
//   - both dropped — "older release's manifest replayed";
//   - the newer-than-running check dropped — "not newer";
//   - the asset-name check dropped — "names another platform's asset";
//   - verifyFileSHA256 not called — "another platform's binary";
//   - the size bound dropped — "oversized".
func TestApplyUpdateBindsTheBinaryToTheSignedManifest(t *testing.T) {
	binary := []byte("the v2.0.0 binary for this platform")
	older := []byte("the v1.5.0 binary for this platform")
	other := []byte("the v2.0.0 binary for another platform")
	key := runtime.GOOS + "/" + runtime.GOARCH

	t.Run("the genuine release is applied", func(t *testing.T) {
		sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", binary)), nil)
		u, exePath := sr.updater(t, "1.0.0")
		if err := u.ApplyUpdate(context.Background(), sr.release("2.0.0", "v2.0.0")); err != nil {
			t.Fatalf("ApplyUpdate: %v", err)
		}
		if !fileIs(exePath, string(binary)) {
			t.Error("the verified binary was not placed")
		}
	})

	t.Run("no manifest", func(t *testing.T) {
		sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", binary)), nil)
		rel := sr.release("2.0.0", "v2.0.0")
		rel.ManifestURL, rel.ManifestSignatureURL = "", ""
		applyRefused(t, sr, "1.0.0", rel, "update manually", 0)
	})

	t.Run("a manifest signed by a stranger's key", func(t *testing.T) {
		_, stranger := generateTestKeyPair(t)
		sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", binary)), stranger)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "manifest signature verification failed", 0)
	})

	t.Run("an older release's manifest replayed with its binary", func(t *testing.T) {
		sr := newSignedRelease(t, older, manifestJSON(t, platformManifest(t, "1.5.0", "v1.5.0", older)), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "not the release being applied", 0)
	})

	t.Run("a manifest for this tag that names another version", func(t *testing.T) {
		sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, "1.5.0", "v2.0.0", binary)), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "not the release being applied", 0)
	})

	t.Run("a manifest for another tag", func(t *testing.T) {
		sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0-rebuilt", binary)), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "not the release being applied", 0)
	})

	t.Run("a release not newer than the running one", func(t *testing.T) {
		sr := newSignedRelease(t, older, manifestJSON(t, platformManifest(t, "1.5.0", "v1.5.0", older)), nil)
		applyRefused(t, sr, "2.0.0", sr.release("1.5.0", "v1.5.0"), "not newer", 0)
	})

	t.Run("no entry for this platform", func(t *testing.T) {
		m := platformManifest(t, "2.0.0", "v2.0.0", binary)
		delete(m.Platforms, key)
		sr := newSignedRelease(t, binary, manifestJSON(t, m), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "no entry for "+key, 0)
	})

	t.Run("this platform's entry names another platform's asset", func(t *testing.T) {
		m := platformManifest(t, "2.0.0", "v2.0.0", binary)
		p := m.Platforms[key]
		p.Asset = "moombox-plan9-amd64"
		m.Platforms[key] = p
		sr := newSignedRelease(t, binary, manifestJSON(t, m), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "moombox-plan9-amd64", 0)
	})

	t.Run("another platform's validly signed binary", func(t *testing.T) {
		sr := newSignedRelease(t, other, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", binary)), nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "does not match the signed manifest", 1)
	})

	t.Run("an oversized manifest", func(t *testing.T) {
		padded := append(manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", binary)), []byte(strings.Repeat(" ", maxManifestSize))...)
		sr := newSignedRelease(t, binary, padded, nil)
		applyRefused(t, sr, "1.0.0", sr.release("2.0.0", "v2.0.0"), "over the", 0)
	})
}

// TestApplyUpdateRefusesAManifestReleaseWithoutItsManifest: the apply
// refused every release without its signed manifest the same way, telling
// the operator to update manually — download it from the release page and
// replace the binary. For a release at or past FirstManifestVersion, which is
// every release a binary carrying this code is offered, the manifest missing
// is what deleting it to pass another release's validly signed binary looks
// like, and VerifyCurrentSignature fails that release for it: the advice sent
// the operator round the binding check by hand, and R S then failed the
// install it had advised. Such a release is now refused as a failure naming
// the missing asset, with no manual-install advice; a release before
// FirstManifestVersion keeps it. The decision is on the release being
// applied, not the running version: the boundary rows run 2.8.10.
//
// THE MUTANTS, each failing the row named:
//   - the releaseCarriesManifest branch dropped (if false) — "no manifest",
//     "an unsigned manifest", "a later release";
//   - it taken for every release (if true) — "before the manifest, no
//     manifest", "before the manifest, an unsigned manifest" (and
//     TestApplyUpdateBindsTheBinaryToTheSignedManifest's "no manifest");
//   - releaseCarriesManifest asked of u.currentVersion, not release.Version —
//     "no manifest", "an unsigned manifest";
//   - the release.ManifestURL == "" test inverted — "no manifest", "an
//     unsigned manifest", "a later release" (each gets the other message).
func TestApplyUpdateRefusesAManifestReleaseWithoutItsManifest(t *testing.T) {
	binary := []byte("the binary of the release being applied")
	first := FirstManifestVersion
	// release is the release of version without its manifest's signature,
	// and without the manifest too unless keepManifest.
	release := func(sr *signedRelease, version string, keepManifest bool) *ReleaseInfo {
		rel := sr.release(version, "v"+version)
		rel.ManifestSignatureURL = ""
		if !keepManifest {
			rel.ManifestURL = ""
		}
		return rel
	}
	noManualAdvice := func(t *testing.T, err error) {
		t.Helper()
		for _, advice := range []string{"update manually", "replace the binary"} {
			if strings.Contains(err.Error(), advice) {
				t.Errorf("refusal %q advises a manual install (%q) of a release the verify fails", err, advice)
			}
		}
	}

	for _, tc := range []struct {
		name, current, version string
		keepManifest           bool
		wantErr                string
		manualAdvice           bool
	}{
		{"no manifest", lastReleaseWithoutManifest, first, false,
			"release v" + first + " publishes no manifest (" + ManifestAsset + "), though every release from " + first + " on", false},
		{"an unsigned manifest", lastReleaseWithoutManifest, first, true,
			"release v" + first + " publishes its manifest without a signature (" + ManifestSignatureAsset + ")", false},
		{"a later release", first, "3.0.0", false, "publishes no manifest (" + ManifestAsset + ")", false},
		{"before the manifest, no manifest", "1.0.0", "2.0.0", false,
			"update manually: download it from https://github.com/vampiricwulf/Moombox/releases/tag/v2.0.0", true},
		{"before the manifest, an unsigned manifest", "1.0.0", "2.0.0", true, "update manually", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sr := newSignedRelease(t, binary, manifestJSON(t, platformManifest(t, tc.version, "v"+tc.version, binary)), nil)
			err := applyRefused(t, sr, tc.current, release(sr, tc.version, tc.keepManifest), tc.wantErr, 0)
			if !tc.manualAdvice {
				noManualAdvice(t, err)
			}
		})
	}
}

// TestParseManifestRefusesMalformedDocuments pins ParseManifest's shape
// checks. THE MUTANTS: drop any one check — its row parses.
func TestParseManifestRefusesMalformedDocuments(t *testing.T) {
	good := `"platforms":{"linux/amd64":{"asset":"moombox-linux-amd64","sha256":"` + strings.Repeat("ab", 32) + `"}}`
	for _, tc := range []struct{ name, doc string }{
		{"not JSON", `{`},
		{"no version", `{"tag":"v2.0.0",` + good + `}`},
		{"no tag", `{"version":"2.0.0",` + good + `}`},
		{"no platforms", `{"version":"2.0.0","tag":"v2.0.0","platforms":{}}`},
		{"an entry with no asset", `{"version":"2.0.0","tag":"v2.0.0","platforms":{"linux/amd64":{"sha256":"` + strings.Repeat("ab", 32) + `"}}}`},
		{"a short hash", `{"version":"2.0.0","tag":"v2.0.0","platforms":{"linux/amd64":{"asset":"a","sha256":"abcd"}}}`},
		{"a hash that is not hex", `{"version":"2.0.0","tag":"v2.0.0","platforms":{"linux/amd64":{"asset":"a","sha256":"` + strings.Repeat("zz", 32) + `"}}}`},
	} {
		if _, err := ParseManifest([]byte(tc.doc)); err == nil {
			t.Errorf("%s: ParseManifest accepted %s", tc.name, tc.doc)
		}
	}
	if _, err := ParseManifest([]byte(`{"version":"2.0.0","tag":"v2.0.0",` + good + `,"later":"field"}`)); err != nil {
		t.Errorf("a well-formed manifest with an unknown field was refused: %v", err)
	}
}

// TestBuildManifestHashesEveryPlatformBinary pins what cmd/sign -manifest
// writes: every releaseAssetMap platform, its asset name, the SHA-256 of the
// file, and a document ParseManifest reads back unchanged.
//
// THE MUTANTS: skip a platform whose binary is missing instead of failing —
// the missing-binary row passes; hash the asset NAME instead of the file —
// the hash comparison fails; drop the version/tag check — the two empty rows
// pass.
func TestBuildManifestHashesEveryPlatformBinary(t *testing.T) {
	dir := t.TempDir()
	for _, a := range releaseAssetMap {
		writeTempFile(t, dir, a.binary, []byte("bytes of "+a.binary))
	}
	if _, err := BuildManifest("", "v2.0.0", dir); err == nil {
		t.Error("BuildManifest accepted an empty version")
	}
	if _, err := BuildManifest("2.0.0", "", dir); err == nil {
		t.Error("BuildManifest accepted an empty tag")
	}
	m, err := BuildManifest("2.0.0", "v2.0.0", dir)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Version != "2.0.0" || m.Tag != "v2.0.0" || len(m.Platforms) != len(releaseAssetMap) {
		t.Fatalf("manifest = %+v", m)
	}
	for key, a := range releaseAssetMap {
		sum := sha256.Sum256([]byte("bytes of " + a.binary))
		if got := m.Platforms[key]; got.Asset != a.binary || got.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s entry = %+v, want %s with the file's SHA-256", key, got, a.binary)
		}
	}
	back, err := ParseManifest(manifestJSON(t, m))
	if err != nil || !reflect.DeepEqual(back, m) {
		t.Errorf("round trip = %+v, %v; want %+v", back, err, m)
	}

	os.Remove(filepath.Join(dir, releaseAssetMap["linux/arm64"].binary))
	if _, err := BuildManifest("2.0.0", "v2.0.0", dir); err == nil {
		t.Error("BuildManifest wrote a manifest with a platform's binary missing")
	}
}

// TestCheckForUpdateCarriesTheManifestURLs: the check records the manifest
// assets for the apply, and a release without them is still OFFERED — the
// operator hears it exists and reads its notes — leaving the refusal to the
// apply (TestApplyUpdateRefusesAManifestReleaseWithoutItsManifest).
//
// THE MUTANTS: the two manifest cases dropped from the asset loop — the
// first row's URLs are empty; an error returned for a missing manifest — the
// second row fails.
func TestCheckForUpdateCarriesTheManifestURLs(t *testing.T) {
	assets, ok := currentPlatformAssets()
	if !ok {
		t.Skipf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	serve := func(withManifest bool) *httptest.Server {
		list := []githubAsset{
			{Name: assets.binary, BrowserDownloadURL: "http://example.com/exe"},
			{Name: assets.sig, BrowserDownloadURL: "http://example.com/sig"},
		}
		if withManifest {
			list = append(list,
				githubAsset{Name: ManifestAsset, BrowserDownloadURL: "http://example.com/manifest"},
				githubAsset{Name: ManifestSignatureAsset, BrowserDownloadURL: "http://example.com/manifest.sig"})
		}
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(rw).Encode(githubRelease{TagName: "v3.0.0", Assets: list})
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	u, _ := newTestUpdater(t, "2.0.0", serve(true), nil)
	got, err := u.CheckForUpdate(context.Background())
	if err != nil || got == nil {
		t.Fatalf("CheckForUpdate = %+v, %v", got, err)
	}
	if got.ManifestURL != "http://example.com/manifest" || got.ManifestSignatureURL != "http://example.com/manifest.sig" {
		t.Errorf("manifest URLs = %q, %q", got.ManifestURL, got.ManifestSignatureURL)
	}

	u, _ = newTestUpdater(t, "2.0.0", serve(false), nil)
	got, err = u.CheckForUpdate(context.Background())
	if err != nil || got == nil {
		t.Fatalf("a release without a manifest: CheckForUpdate = %+v, %v — want it offered", got, err)
	}
	if got.ManifestURL != "" {
		t.Errorf("ManifestURL = %q for a release that has none", got.ManifestURL)
	}
}

// warnRecorder is a logger that keeps the Warn lines; the rest is dropped.
type warnRecorder struct {
	mu    sync.Mutex
	warns []string
}

func (*warnRecorder) Debug(msg string, args ...any) {}
func (*warnRecorder) Info(msg string, args ...any)  {}
func (r *warnRecorder) Warn(msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warns = append(r.warns, msg)
}
func (*warnRecorder) Error(msg string, args ...any) {}

func (r *warnRecorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.warns...)
}

// TestCheckForUpdateWarnsByTheReleaseItOffers: the check's Warn for a release
// without its signed manifest said it must be installed manually, whatever
// the release. From FirstManifestVersion on that is a release the apply
// refuses as a failure and VerifyCurrentSignature fails, so the Warn says it
// will be refused and must not be installed by hand; a release before it
// keeps the manual-install line. Both are still offered.
//
// THE MUTANTS, each failing the row named:
//   - the releaseCarriesManifest split dropped, the manual line always —
//     "no manifest", "an unsigned manifest";
//   - the new line always — "before the manifest";
//   - releaseCarriesManifest asked of u.currentVersion, not the offered
//     release's — "no manifest", "an unsigned manifest" (both run 2.8.10).
func TestCheckForUpdateWarnsByTheReleaseItOffers(t *testing.T) {
	assets, ok := currentPlatformAssets()
	if !ok {
		t.Skipf("auto-update unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const refusedLine = "must not be installed by hand"
	const manualLine = "it must be installed manually"
	for _, tc := range []struct {
		name, current, latest string
		manifest, manifestSig bool
		wantWarn              string // "" for no Warn at all
	}{
		{"no manifest", lastReleaseWithoutManifest, FirstManifestVersion, false, false, refusedLine},
		{"an unsigned manifest", lastReleaseWithoutManifest, FirstManifestVersion, true, false, refusedLine},
		{"before the manifest", "1.0.0", "2.0.0", false, false, manualLine},
		{"the signed manifest", lastReleaseWithoutManifest, FirstManifestVersion, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := []githubAsset{
				{Name: assets.binary, BrowserDownloadURL: "http://example.com/exe"},
				{Name: assets.sig, BrowserDownloadURL: "http://example.com/sig"},
			}
			if tc.manifest {
				list = append(list, githubAsset{Name: ManifestAsset, BrowserDownloadURL: "http://example.com/manifest"})
			}
			if tc.manifestSig {
				list = append(list, githubAsset{Name: ManifestSignatureAsset, BrowserDownloadURL: "http://example.com/manifest.sig"})
			}
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				json.NewEncoder(rw).Encode(githubRelease{TagName: "v" + tc.latest, Assets: list})
			}))
			t.Cleanup(srv.Close)
			u, _ := newTestUpdater(t, tc.current, srv, nil)
			rec := &warnRecorder{}
			u.logger = rec

			got, err := u.CheckForUpdate(context.Background())
			if err != nil || got == nil {
				t.Fatalf("CheckForUpdate = %+v, %v — want v%s offered", got, err, tc.latest)
			}
			warns := rec.lines()
			if tc.wantWarn == "" {
				if len(warns) != 0 {
					t.Errorf("Warn lines %q for a release with its signed manifest", warns)
				}
				return
			}
			if len(warns) != 1 || !strings.Contains(warns[0], tc.wantWarn) {
				t.Errorf("Warn lines %q, want one containing %q", warns, tc.wantWarn)
			}
		})
	}
}

// runningRelease serves the GitHub release of the running version, v2.0.0:
// the platform binary's .sig over the running binary ("current binary", as
// newTestUpdater seeds it) by the trusted key, and — unless manifest is nil —
// the manifest, signed by manifestKey (the trusted key when nil), with its
// .sig asset listed only when withManifestSig. Asset URLs are built from the
// request's Host, so no handler reads the server variable.
func runningRelease(t *testing.T, manifest []byte, manifestKey ed25519.PrivateKey, withManifestSig bool) *Updater {
	t.Helper()
	return runningReleaseAt(t, "2.0.0", manifest, manifestKey, withManifestSig)
}

// runningReleaseAt is runningRelease for a running version other than 2.0.0.
func runningReleaseAt(t *testing.T, version string, manifest []byte, manifestKey ed25519.PrivateKey, withManifestSig bool) *Updater {
	t.Helper()
	assets, ok := currentPlatformAssets()
	if !ok {
		t.Skipf("signature verification unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	pub, priv := generateTestKeyPair(t)
	if manifestKey == nil {
		manifestKey = priv
	}
	running := []byte("current binary")
	mux := http.NewServeMux()
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) { rw.Write(ed25519.Sign(priv, running)) })
	mux.HandleFunc("/manifest", func(rw http.ResponseWriter, _ *http.Request) { rw.Write(manifest) })
	mux.HandleFunc("/manifest.sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(ed25519.Sign(manifestKey, manifest))
	})
	mux.HandleFunc("/repos/test/Moombox/releases/tags/v"+version, func(rw http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		rel := githubRelease{TagName: "v" + version, Assets: []githubAsset{{Name: assets.sig, BrowserDownloadURL: base + "/sig"}}}
		if manifest != nil {
			rel.Assets = append(rel.Assets, githubAsset{Name: ManifestAsset, BrowserDownloadURL: base + "/manifest"})
			if withManifestSig {
				rel.Assets = append(rel.Assets, githubAsset{Name: ManifestSignatureAsset, BrowserDownloadURL: base + "/manifest.sig"})
			}
		}
		json.NewEncoder(rw).Encode(rel)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := newTestUpdater(t, version, srv, func(bin, sig string) error {
		return verifySignatureWithKey(pub, bin, sig)
	})
	return u
}

// TestVerifyCurrentSignatureChecksTheReleaseManifest: the verify action (R S,
// POST /api/update/verify) checked only the running binary's .sig, which says
// the key signed these bytes and nothing about which release they are — a
// validly signed binary of another release or platform passed it. When the
// running version's release publishes a signed manifest, the binary is now
// held to it as ApplyUpdate holds a download; a release before the manifest
// (FirstManifestVersion) that publishes none still verifies, and says it
// checked the signature alone — the running 2.0.0 here is one.
// TestVerifyCurrentSignatureHoldsAManifestReleaseToItsManifest covers a
// release at or past it.
//
// THE MUTANTS, each failing the row named:
//   - VerifyCurrentSignature reporting true without calling
//     verifyRunningAgainstManifest — "another release's manifest", "another
//     binary", "a stranger's key";
//   - the names check dropped — "another release's manifest";
//   - verifyFileSHA256 not called — "another binary";
//   - true reported for a release with no manifest — "a release with no
//     manifest";
//   - only the manifest asset required, not its signature — "an unsigned
//     manifest" (the empty signature URL fails the download).
func TestVerifyCurrentSignatureChecksTheReleaseManifest(t *testing.T) {
	running := []byte("current binary")
	key := runtime.GOOS + "/" + runtime.GOARCH

	t.Run("the release's manifest binds the running binary", func(t *testing.T) {
		u := runningRelease(t, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", running)), nil, true)
		manifest, err := u.VerifyCurrentSignature(context.Background())
		if err != nil || !manifest {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want the manifest checked and no error", manifest, err)
		}
	})

	t.Run("a release before the manifest verifies by its signature alone", func(t *testing.T) {
		u := runningRelease(t, nil, nil, false)
		manifest, err := u.VerifyCurrentSignature(context.Background())
		if err != nil || manifest {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want a signature-only verification", manifest, err)
		}
	})

	t.Run("an unsigned manifest is no manifest", func(t *testing.T) {
		u := runningRelease(t, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", running)), nil, false)
		manifest, err := u.VerifyCurrentSignature(context.Background())
		if err != nil || manifest {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want a signature-only verification", manifest, err)
		}
	})

	failed := func(t *testing.T, u *Updater, wantErr string) {
		t.Helper()
		manifest, err := u.VerifyCurrentSignature(context.Background())
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want a failure containing %q", manifest, err, wantErr)
		}
	}

	t.Run("another release's manifest", func(t *testing.T) {
		failed(t, runningRelease(t, manifestJSON(t, platformManifest(t, "1.5.0", "v1.5.0", running)), nil, true), "not the running 2.0.0")
	})

	t.Run("another binary than the release published", func(t *testing.T) {
		failed(t, runningRelease(t, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", []byte("the published binary"))), nil, true),
			"does not match the signed manifest")
	})

	t.Run("a manifest signed by a stranger's key", func(t *testing.T) {
		_, stranger := generateTestKeyPair(t)
		failed(t, runningRelease(t, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", running)), stranger, true),
			"manifest signature verification failed")
	})

	t.Run("no entry for this platform", func(t *testing.T) {
		m := platformManifest(t, "2.0.0", "v2.0.0", running)
		delete(m.Platforms, key)
		failed(t, runningRelease(t, manifestJSON(t, m), nil, true), "no entry for "+key)
	})
}

// TestVerifyCurrentSignatureHoldsAManifestReleaseToItsManifest: the verify
// action reported a release with no manifest as a signature-only check, in
// yellow, whatever the running version. Every release from
// FirstManifestVersion on is published with the signed manifest, so for one
// of those the manifest missing, or its signature missing, is exactly what
// deleting them to pass another release's validly signed binary looks like.
// That now FAILS; a release before the manifest keeps the signature-only
// result.
//
// THE MUTANTS, each failing the row named:
//   - the releaseCarriesManifest branch dropped (if false) — "no manifest",
//     "an unsigned manifest", "a later release", "a pre-release";
//   - it taken for every version (if true) — "the last release before the
//     manifest" (and TestVerifyCurrentSignatureChecksTheReleaseManifest's two
//     signature-only rows);
//   - releaseCarriesManifest comparing the whole version, pre-release suffix
//     included — "a pre-release";
//   - its >= 0 made > 0 — "no manifest", "an unsigned manifest", "a
//     pre-release";
//   - the manifestURL == "" test inverted — "no manifest", "an unsigned
//     manifest", "a later release", "a pre-release" (each gets the other
//     message).
func TestVerifyCurrentSignatureHoldsAManifestReleaseToItsManifest(t *testing.T) {
	running := []byte("current binary")
	signedFor := func(version string) []byte {
		return manifestJSON(t, platformManifest(t, version, "v"+version, running))
	}
	failed := func(t *testing.T, u *Updater, wantErr string) {
		t.Helper()
		manifest, err := u.VerifyCurrentSignature(context.Background())
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want a failure containing %q", manifest, err, wantErr)
		}
		if manifest {
			t.Errorf("VerifyCurrentSignature reported the manifest checked beside its failure")
		}
	}
	first := FirstManifestVersion

	t.Run("its signed manifest binds the running binary", func(t *testing.T) {
		manifest, err := runningReleaseAt(t, first, signedFor(first), nil, true).VerifyCurrentSignature(context.Background())
		if err != nil || !manifest {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want the manifest checked and no error", manifest, err)
		}
	})

	t.Run("no manifest", func(t *testing.T) {
		failed(t, runningReleaseAt(t, first, nil, nil, false), "publishes no manifest ("+ManifestAsset+")")
	})

	t.Run("an unsigned manifest", func(t *testing.T) {
		failed(t, runningReleaseAt(t, first, signedFor(first), nil, false),
			"publishes its manifest without a signature ("+ManifestSignatureAsset+")")
	})

	t.Run("a later release", func(t *testing.T) {
		failed(t, runningReleaseAt(t, "3.0.0", nil, nil, false), "publishes no manifest")
	})

	t.Run("a pre-release of the first release with the manifest", func(t *testing.T) {
		failed(t, runningReleaseAt(t, first+"-rc.1", nil, nil, false), "publishes no manifest")
	})

	t.Run("the last release before the manifest", func(t *testing.T) {
		manifest, err := runningReleaseAt(t, lastReleaseWithoutManifest, nil, nil, false).VerifyCurrentSignature(context.Background())
		if err != nil || manifest {
			t.Fatalf("VerifyCurrentSignature = (%v, %v), want a signature-only verification", manifest, err)
		}
	})
}

// lastReleaseWithoutManifest is the last release cut before the manifest
// pipeline: v2.8.10's bump commit precedes it.
const lastReleaseWithoutManifest = "2.8.10"

// TestReleaseCarriesManifest pins which running versions are held to their
// release's manifest.
//
// THE MUTANTS, each failing the row named:
//   - the comparison made on the whole version — "2.9.0-rc.1",
//     "2.9.0-test.1";
//   - >= 0 made > 0 — "2.9.0" (both rows), "v2.9.0", "2.9.0-rc.1",
//     "2.9.0-test.1";
//   - an unparseable version not held to the manifest — "not-a-version";
//   - FirstManifestVersion at or below 2.8.10 — "2.8.10".
func TestReleaseCarriesManifest(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"2.0.0", false},
		{"2.8.9", false},
		{lastReleaseWithoutManifest, false},
		{"v" + lastReleaseWithoutManifest, false},
		{lastReleaseWithoutManifest + "-rc.1", false},
		{FirstManifestVersion, true},
		{"v" + FirstManifestVersion, true},
		{FirstManifestVersion + "-rc.1", true},
		{FirstManifestVersion + "-test.1", true},
		{"2.9.0", true},
		{"3.0.0", true},
		{"not-a-version", true},
	} {
		if got := releaseCarriesManifest(tc.version); got != tc.want {
			t.Errorf("releaseCarriesManifest(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestFirstManifestVersionKeepsStepWithTheReleases ties FirstManifestVersion
// to the version cmd/moombox/main.go declares, read as release.yml reads it.
// Until a release past 2.8.10 is cut it must be the lowest number the next
// release can have, the next patch: any higher, and that release, cut under
// the next patch, would verify with its manifest removed. Once one is cut, it
// must not be past the declared release.
//
// THE MUTANTS: FirstManifestVersion "2.8.10" (also failing
// TestReleaseCarriesManifest) or "2.9.1", while main.go declares 2.9.0 (and,
// before 2.9.0 was declared, "2.9.0" while main.go declared 2.8.10).
func TestFirstManifestVersionKeepsStepWithTheReleases(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "moombox", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/moombox/main.go: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*version\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("cmd/moombox/main.go declares no version")
	}
	declared, err := ParseVersionFull(string(m[1]))
	if err != nil {
		t.Fatalf("cmd/moombox/main.go declares %q: %v", m[1], err)
	}
	core := fmt.Sprintf("%d.%d.%d", declared.Major, declared.Minor, declared.Patch)

	if CompareVersions(FirstManifestVersion, lastReleaseWithoutManifest) <= 0 {
		t.Fatalf("FirstManifestVersion %s is not past %s, the last release cut without the manifest",
			FirstManifestVersion, lastReleaseWithoutManifest)
	}
	if CompareVersions(core, lastReleaseWithoutManifest) <= 0 {
		next := fmt.Sprintf("%d.%d.%d", declared.Major, declared.Minor, declared.Patch+1)
		if FirstManifestVersion != next {
			t.Errorf("main.go declares %s, so no release with the manifest has been cut: FirstManifestVersion is %s, want the next release's lowest number, %s",
				m[1], FirstManifestVersion, next)
		}
		return
	}
	if CompareVersions(FirstManifestVersion, core) > 0 {
		t.Errorf("FirstManifestVersion %s is past %s, the release main.go declares — set it to the release that first shipped the manifest",
			FirstManifestVersion, m[1])
	}
}
