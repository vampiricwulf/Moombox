package cookies

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// runtimeGOOS returns runtime.GOOS — a seam for testing.
func runtimeGOOS() string {
	return runtime.GOOS
}

// browserDetectCache caches the DetectBrowser result to avoid repeated registry
// queries and filesystem I/O on every GetStatus call.
//
// The available/availableChecked/availableExpires fields are DetectBrowsers'
// slot — same TTL, same mutex as the single-browser slot above, added so
// GetStatus (the most-polled method in this package; see its doc comment)
// stops paying for a second full filesystem+registry scan on every call.
// Before this, DetectBrowser was cached but DetectBrowsers was not, so every
// status poll still rebuilt the full list from scratch.
var browserDetectCache struct {
	mu      sync.Mutex
	browser *DetectedBrowser
	checked bool
	expires time.Time

	available        []DetectedBrowser
	availableChecked bool
	availableExpires time.Time
}

const browserDetectCacheTTL = 60 * time.Second

// detectBrowserUncached and detectBrowsersUncached are package vars, not
// plain funcs, so a test can swap either out — to count invocations or to
// fake a result entirely — without spawning a real reg.exe or touching the
// filesystem. Same seam convention as killProcessTree / setupBrowserGone
// elsewhere in this package. Production never reassigns them.
var (
	detectBrowserUncached  = detectBrowserUncachedImpl
	detectBrowsersUncached = detectBrowsersUncachedImpl
)

// DetectedBrowser holds info about a detected browser.
type DetectedBrowser struct {
	Type string `json:"type"` // "firefox", "waterfox", "chrome", "brave", "opera", "edge"
	Path string `json:"path"`
	Name string `json:"name"`
}

// isFirefoxBased returns true for Firefox and Firefox-family browsers
// (Waterfox, LibreWolf, Zen) that use cookies.sqlite and the -profile flag.
func isFirefoxBased(browserType string) bool {
	switch browserType {
	case "firefox", "waterfox", "librewolf", "zen":
		return true
	}
	return false
}

// browserInfo maps a browser type to its display name and paths/candidates.
type browserInfo struct {
	typ          string
	name         string
	pathsFn      func() []string
	windowsPaths []string // relative paths under Program Files / LocalAppData
}

// knownBrowsers is the search-order list. Firefox-family entries come first
// so the Firefox-based extraction path (cookies.sqlite, no CDP) is preferred
// when both kinds are installed. Within each family, more privacy-focused or
// less-common forks come ahead of mainline browsers so a user who took the
// trouble to install LibreWolf isn't auto-detected as Firefox.
var knownBrowsers = []browserInfo{
	{"librewolf", "LibreWolf", librewolfPaths, []string{`LibreWolf\librewolf.exe`}},
	{"zen", "Zen Browser", zenPaths, []string{`Zen Browser\zen.exe`}},
	{"waterfox", "Waterfox", waterfoxPaths, []string{`Waterfox\waterfox.exe`}},
	{"firefox", "Firefox", firefoxPaths, []string{`Mozilla Firefox\firefox.exe`}},
	{"vivaldi", "Vivaldi", vivaldiPaths, []string{`Vivaldi\Application\vivaldi.exe`}},
	{"thorium", "Thorium", thoriumPaths, []string{`Thorium\Application\thorium.exe`}},
	{"brave", "Brave", bravePaths, []string{`BraveSoftware\Brave-Browser\Application\brave.exe`}},
	{"chrome", "Google Chrome", chromePaths, []string{`Google\Chrome\Application\chrome.exe`}},
	{"opera", "Opera GX", operaPaths, []string{`Programs\Opera GX\opera.exe`, `Programs\Opera\opera.exe`}},
	{"edge", "Microsoft Edge", edgePaths, []string{`Microsoft\Edge\Application\msedge.exe`}},
}

// DetectBrowser finds the best available browser, caching the result for 60s.
// It checks the system's default browser first, then falls back to the
// knownBrowsers order (Firefox-family ahead of Chromium-family).
//
// Returns the cache's own *DetectedBrowser, shared across every caller until
// the next scan replaces it — callers must not mutate what it points to. See
// DetectBrowsers' doc for the same invariant on its slice.
func DetectBrowser() *DetectedBrowser {
	browserDetectCache.mu.Lock()
	defer browserDetectCache.mu.Unlock()

	if browserDetectCache.checked && time.Now().Before(browserDetectCache.expires) {
		return browserDetectCache.browser
	}

	result := detectBrowserUncached()
	browserDetectCache.browser = result
	browserDetectCache.checked = true
	browserDetectCache.expires = time.Now().Add(browserDetectCacheTTL)
	return result
}

// detectBrowserUncachedImpl performs the actual browser detection (registry + filesystem I/O).
// Assigned to the detectBrowserUncached var above; see its doc for why.
func detectBrowserUncachedImpl() *DetectedBrowser {
	// Build search order: default browser first, then remaining browsers.
	// Edge is excluded from promotion — it frequently hijacks the Windows
	// registry default even when the user has set another browser.
	order := knownBrowsers
	if defType := detectDefaultBrowserType(); defType != "" && defType != "edge" {
		reordered := make([]browserInfo, 0, len(knownBrowsers))
		for _, b := range knownBrowsers {
			if b.typ == defType {
				reordered = append([]browserInfo{b}, reordered...)
			} else {
				reordered = append(reordered, b)
			}
		}
		order = reordered
	}

	// Build Windows install path roots once.
	var windowsRoots []string
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("PROGRAMFILES"); pf != "" {
			windowsRoots = append(windowsRoots, pf)
		}
		if pf86 := os.Getenv("PROGRAMFILES(X86)"); pf86 != "" {
			windowsRoots = append(windowsRoots, pf86)
		}
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			windowsRoots = append(windowsRoots, localApp)
		}
	}

	// For each browser, try PATH then Windows install paths before moving
	// to the next browser. This prevents Edge (always on PATH) from winning
	// over browsers installed in Program Files but not on PATH.
	for _, b := range order {
		for _, name := range b.pathsFn() {
			if path, err := exec.LookPath(name); err == nil {
				return &DetectedBrowser{Type: b.typ, Path: path, Name: b.name}
			}
		}
		for _, relPath := range b.windowsPaths {
			for _, root := range windowsRoots {
				fullPath := filepath.Join(root, relPath)
				if _, err := os.Stat(fullPath); err == nil {
					return &DetectedBrowser{Type: b.typ, Path: fullPath, Name: b.name}
				}
			}
		}
	}

	return nil
}

// DetectBrowsers enumerates every browser the package can find, in the
// same priority order as DetectBrowser (system default first if known,
// then knownBrowsers list with Firefox-family before Chromium-family).
// Returns an empty (non-nil) slice when none are detected so callers
// can range without nil-checks.
//
// Cached the same way DetectBrowser is — same 60s TTL, same mutex (see
// browserDetectCache's available/availableChecked/availableExpires fields).
// It used to be uncached on the theory that a caller rendering a UI on top
// wants freshness more than it wants to save a ~ms scan; in practice its one
// caller is GetStatus, the most-polled method in the package, and every poll
// paid for a full filesystem+registry scan (a reg.exe spawn on Windows) it
// almost always threw away unread — the AVAILABLE list only needs to be
// fresh enough to reflect a browser install that happened in the last
// minute. InvalidateBrowserDetection clears this early when the configured
// browser changes.
//
// The returned slice is the CACHE'S OWN backing array, shared across every
// caller until the next scan replaces it wholesale (always under
// browserDetectCache.mu, never mutated in place) — callers must treat it as
// read-only. DetectBrowser has shared its single *DetectedBrowser the same
// way since before this cache existed; both of today's callers (GetStatus,
// resolvedBrowser) only ever range or read, never write, so this is
// currently safe but is a new invariant to keep now that a second caller
// (a future one) could plausibly want to sort or filter its own copy rather
// than the shared one.
func DetectBrowsers() []DetectedBrowser {
	browserDetectCache.mu.Lock()
	defer browserDetectCache.mu.Unlock()

	if browserDetectCache.availableChecked && time.Now().Before(browserDetectCache.availableExpires) {
		return browserDetectCache.available
	}

	result := detectBrowsersUncached()
	browserDetectCache.available = result
	browserDetectCache.availableChecked = true
	browserDetectCache.availableExpires = time.Now().Add(browserDetectCacheTTL)
	return result
}

// detectBrowsersUncachedImpl performs the actual DetectBrowsers scan
// (registry + filesystem I/O). Assigned to the detectBrowsersUncached var
// declared above; see its doc for why.
func detectBrowsersUncachedImpl() []DetectedBrowser {
	out := make([]DetectedBrowser, 0, 4)

	// Build search order: default browser first, then remaining.
	order := knownBrowsers
	if defType := detectDefaultBrowserType(); defType != "" && defType != "edge" {
		reordered := make([]browserInfo, 0, len(knownBrowsers))
		for _, b := range knownBrowsers {
			if b.typ == defType {
				reordered = append([]browserInfo{b}, reordered...)
			} else {
				reordered = append(reordered, b)
			}
		}
		order = reordered
	}

	// Build Windows install path roots once.
	var windowsRoots []string
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("PROGRAMFILES"); pf != "" {
			windowsRoots = append(windowsRoots, pf)
		}
		if pf86 := os.Getenv("PROGRAMFILES(X86)"); pf86 != "" {
			windowsRoots = append(windowsRoots, pf86)
		}
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			windowsRoots = append(windowsRoots, localApp)
		}
	}

	seen := map[string]struct{}{} // dedupe by absolute path

	addIfNew := func(b DetectedBrowser) {
		if _, dup := seen[b.Path]; dup {
			return
		}
		seen[b.Path] = struct{}{}
		out = append(out, b)
	}

	for _, b := range order {
		for _, name := range b.pathsFn() {
			if path, err := exec.LookPath(name); err == nil {
				addIfNew(DetectedBrowser{Type: b.typ, Path: path, Name: b.name})
			}
		}
		for _, relPath := range b.windowsPaths {
			for _, root := range windowsRoots {
				fullPath := filepath.Join(root, relPath)
				if _, err := os.Stat(fullPath); err == nil {
					addIfNew(DetectedBrowser{Type: b.typ, Path: fullPath, Name: b.name})
				}
			}
		}
	}

	return out
}

// InvalidateBrowserDetection clears both detection caches — DetectBrowser's
// single pick and DetectBrowsers' full list — so the next call re-scans
// immediately instead of riding out the remainder of the 60s TTL.
//
// Call this whenever the configured browser changes: a freshly-validated
// custom path, or a browser_path/browser_type edit through Settings, is
// exactly the moment an operator is most likely to have just installed the
// browser they are pointing Moombox at, and a stale "not found" (or a stale
// list missing the new install) is most visible right then. Getting this
// wrong costs at most browserDetectCacheTTL of staleness — the same window
// that existed with no invalidation at all — so a missed call site is a
// freshness nit, not a correctness bug.
func InvalidateBrowserDetection() {
	browserDetectCache.mu.Lock()
	defer browserDetectCache.mu.Unlock()
	browserDetectCache.checked = false
	browserDetectCache.browser = nil
	browserDetectCache.availableChecked = false
	browserDetectCache.available = nil
}

// detectDefaultBrowserType returns the type of the system's default browser
// or "" if detection fails or the browser is unknown.
func detectDefaultBrowserType() string {
	if runtime.GOOS == "windows" {
		return detectDefaultBrowserWindows()
	}
	return ""
}

// detectDefaultBrowserWindows queries the Windows registry for the default HTTPS handler.
func detectDefaultBrowserWindows() string {
	out, err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`,
		"/v", "ProgId").Output()
	if err != nil {
		return ""
	}
	return parseDefaultBrowserProgID(string(out))
}

// parseDefaultBrowserProgID extracts the browser type from `reg query`
// output for the UrlAssociations\https\UserChoice key. Output looks like:
//
//	HKEY_CURRENT_USER\Software\Microsoft\Windows\...
//	    ProgId    REG_SZ    ChromeHTML
//
// Returns "" if no recognized ProgId is found. Pure function so the
// matcher table can be tested without shelling out to reg.exe.
func parseDefaultBrowserProgID(regOutput string) string {
	for line := range strings.SplitSeq(regOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ProgId") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		progID := strings.ToLower(parts[2])
		switch {
		// Firefox family. LibreWolf/Zen ahead of Firefox so a fork user
		// who has both Firefox-fork ProgIDs registered hits the fork.
		case strings.HasPrefix(progID, "librewolfurl"), strings.HasPrefix(progID, "librewolfhtml"):
			return "librewolf"
		case strings.HasPrefix(progID, "zenbrowserurl"), strings.HasPrefix(progID, "zenurl"):
			return "zen"
		case strings.HasPrefix(progID, "waterfoxhtml"):
			return "waterfox"
		case strings.HasPrefix(progID, "firefoxurl"):
			return "firefox"
		// Chromium family.
		case strings.HasPrefix(progID, "vivaldihtm"):
			return "vivaldi"
		case strings.HasPrefix(progID, "thoriumhtm"):
			return "thorium"
		case strings.HasPrefix(progID, "bravehtml"):
			return "brave"
		case strings.HasPrefix(progID, "chromehtml"):
			return "chrome"
		case strings.HasPrefix(progID, "operagxstable"), strings.HasPrefix(progID, "operastable"):
			return "opera"
		case strings.HasPrefix(progID, "msedgehtm"):
			return "edge"
		}
		return ""
	}
	return ""
}

func firefoxPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"firefox", "/Applications/Firefox.app/Contents/MacOS/firefox"}
	}
	return []string{"firefox"}
}

func waterfoxPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"waterfox", "/Applications/Waterfox.app/Contents/MacOS/waterfox"}
	}
	return []string{"waterfox"}
}

func librewolfPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"librewolf", "/Applications/LibreWolf.app/Contents/MacOS/librewolf"}
	}
	return []string{"librewolf"}
}

func zenPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"zen", "/Applications/Zen Browser.app/Contents/MacOS/zen"}
	}
	return []string{"zen", "zen-browser"}
}

func vivaldiPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"vivaldi", "/Applications/Vivaldi.app/Contents/MacOS/Vivaldi"}
	}
	return []string{"vivaldi", "vivaldi-stable"}
}

func thoriumPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"thorium", "/Applications/Thorium.app/Contents/MacOS/Thorium"}
	}
	return []string{"thorium", "thorium-browser"}
}

func edgePaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"microsoft-edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
	}
	return []string{"msedge", "microsoft-edge"}
}

func chromePaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"google-chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	}
	return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
}

func bravePaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"brave-browser", "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"}
	}
	return []string{"brave-browser", "brave"}
}

func operaPaths() []string {
	if runtime.GOOS == "darwin" {
		return []string{"opera", "/Applications/Opera GX.app/Contents/MacOS/Opera"}
	}
	return []string{"opera"}
}
