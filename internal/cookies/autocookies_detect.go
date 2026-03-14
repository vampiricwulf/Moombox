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
var browserDetectCache struct {
	mu      sync.Mutex
	browser *DetectedBrowser
	checked bool
	expires time.Time
}

const browserDetectCacheTTL = 60 * time.Second

// DetectedBrowser holds info about a detected browser.
type DetectedBrowser struct {
	Type string `json:"type"` // "firefox", "waterfox", "chrome", "brave", "opera", "edge"
	Path string `json:"path"`
	Name string `json:"name"`
}

// isFirefoxBased returns true for Firefox and Firefox-family browsers (Waterfox, etc.)
// that use cookies.sqlite and the -profile flag.
func isFirefoxBased(browserType string) bool {
	return browserType == "firefox" || browserType == "waterfox"
}

// browserInfo maps a browser type to its display name and paths/candidates.
type browserInfo struct {
	typ          string
	name         string
	pathsFn      func() []string
	windowsPaths []string // relative paths under Program Files / LocalAppData
}

var knownBrowsers = []browserInfo{
	{"firefox", "Firefox", firefoxPaths, []string{`Mozilla Firefox\firefox.exe`}},
	{"waterfox", "Waterfox", waterfoxPaths, []string{`Waterfox\waterfox.exe`}},
	{"chrome", "Google Chrome", chromePaths, []string{`Google\Chrome\Application\chrome.exe`}},
	{"brave", "Brave", bravePaths, []string{`BraveSoftware\Brave-Browser\Application\brave.exe`}},
	{"opera", "Opera GX", operaPaths, []string{`Programs\Opera GX\opera.exe`, `Programs\Opera\opera.exe`}},
	{"edge", "Microsoft Edge", edgePaths, []string{`Microsoft\Edge\Application\msedge.exe`}},
}

// DetectBrowser finds the best available browser, caching the result for 60s.
// It checks the system's default browser first, then falls back to
// Firefox > Waterfox > Chrome > Brave > Opera > Edge.
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

// detectBrowserUncached performs the actual browser detection (registry + filesystem I/O).
func detectBrowserUncached() *DetectedBrowser {
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
	// Output: "    ProgId    REG_SZ    ChromeHTML\r\n"
	for line := range strings.SplitSeq(string(out), "\n") {
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
		case strings.HasPrefix(progID, "waterfoxhtml"):
			return "waterfox"
		case strings.HasPrefix(progID, "firefoxurl"):
			return "firefox"
		case strings.HasPrefix(progID, "msedgehtm"):
			return "edge"
		case strings.HasPrefix(progID, "chromehtml"):
			return "chrome"
		case strings.HasPrefix(progID, "bravehtml"):
			return "brave"
		case strings.HasPrefix(progID, "operagxstable"), strings.HasPrefix(progID, "operastable"):
			return "opera"
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
