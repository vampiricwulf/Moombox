package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// installMu prevents concurrent FFmpeg install operations.
var installMu sync.Mutex

// ffmpegCheckCache caches the result of checkFFmpeg to avoid spawning
// ffmpeg -version on every request (e.g., during 2s polling in pollForRestart).
var (
	ffmpegCacheMu     sync.Mutex
	ffmpegCacheValid  bool
	ffmpegCacheResult bool
	ffmpegCacheVer    string
	ffmpegCachePath   string
	ffmpegCacheTime   time.Time
)

const ffmpegCacheTTL = 10 * time.Second

// CheckFFmpegCached returns a cached result of checkFFmpeg, refreshing
// only if the cache is stale or the path has changed.
func CheckFFmpegCached(path string) (valid bool, version string) {
	ffmpegCacheMu.Lock()
	defer ffmpegCacheMu.Unlock()

	now := time.Now()
	if ffmpegCacheValid && ffmpegCachePath == path && now.Sub(ffmpegCacheTime) < ffmpegCacheTTL {
		return ffmpegCacheResult, ffmpegCacheVer
	}

	valid, version = checkFFmpeg(path)
	ffmpegCacheValid = true
	ffmpegCacheResult = valid
	ffmpegCacheVer = version
	ffmpegCachePath = path
	ffmpegCacheTime = now
	return valid, version
}

// InvalidateFFmpegCache clears the cached FFmpeg check result, forcing
// the next call to CheckFFmpegCached to re-probe.
func InvalidateFFmpegCache() {
	ffmpegCacheMu.Lock()
	ffmpegCacheValid = false
	ffmpegCacheMu.Unlock()
}

// FFmpegDeps holds dependencies for FFmpeg validation routes.
type FFmpegDeps struct {
	Cfg        *config.MoomboxConfig
	SaveConfig func(*config.MoomboxConfig) error
	Logger     interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// FFmpegRoutes registers FFmpeg validation and installation endpoints.
func FFmpegRoutes(r chi.Router, deps *FFmpegDeps) {
	// GET /api/v1/ffmpeg/check — check if ffmpeg is available on PATH or configured path
	r.Get("/api/v1/ffmpeg/check", func(rw http.ResponseWriter, req *http.Request) {
		path := deps.Cfg.Paths.FfmpegPath
		if path == "" {
			path = "ffmpeg"
		}
		valid, version := checkFFmpeg(path)
		resolvedPath := path
		if valid {
			if abs, err := exec.LookPath(path); err == nil {
				resolvedPath = abs
			}
		}
		jsonResponse(rw, map[string]any{
			"valid":   valid,
			"version": version,
			"path":    resolvedPath,
		})
	})

	// POST /api/v1/ffmpeg/check — check a custom ffmpeg path, save to config if valid
	r.Post("/api/v1/ffmpeg/check", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		path := strings.TrimSpace(body.Path)
		if path == "" {
			jsonError(rw, "path is required", http.StatusBadRequest)
			return
		}

		valid, version := checkFFmpeg(path)
		if valid && deps.SaveConfig != nil {
			deps.Cfg.Paths.FfmpegPath = path
			if err := deps.SaveConfig(deps.Cfg); err != nil {
				deps.Logger.Error("Failed to save ffmpeg path to config", "error", err.Error())
			}
		}

		jsonResponse(rw, map[string]any{
			"valid":   valid,
			"version": version,
			"path":    path,
		})
	})

	// GET /api/v1/ffmpeg/install-options — check which package managers are available
	r.Get("/api/v1/ffmpeg/install-options", func(rw http.ResponseWriter, req *http.Request) {
		chocoAvail := false
		wingetAvail := false

		if runtime.GOOS == "windows" {
			if _, err := exec.LookPath("choco"); err == nil {
				chocoAvail = true
			}
			if _, err := exec.LookPath("winget"); err == nil {
				wingetAvail = true
			}
		}

		jsonResponse(rw, map[string]any{
			"chocoAvailable":  chocoAvail,
			"wingetAvailable": wingetAvail,
			"platform":        runtime.GOOS,
		})
	})

	// POST /api/v1/ffmpeg/install — install ffmpeg via a package manager
	r.Post("/api/v1/ffmpeg/install", func(rw http.ResponseWriter, req *http.Request) {
		if runtime.GOOS != "windows" {
			jsonError(rw, "automatic installation is only supported on Windows", http.StatusBadRequest)
			return
		}

		var body struct {
			Method string `json:"method"` // "choco", "choco-install", "winget"
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		deps.Logger.Info("Installing FFmpeg", "method", body.Method)
		if err := InstallFFmpeg(body.Method); err != nil {
			jsonError(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		// Invalidate cache and verify FFmpeg is now available
		InvalidateFFmpegCache()
		valid, version := checkFFmpeg("ffmpeg")
		if !valid {
			jsonError(rw, "FFmpeg installed but not found on PATH. You may need to restart.", http.StatusInternalServerError)
			return
		}

		deps.Logger.Info("FFmpeg installed successfully", "version", version)
		jsonResponse(rw, map[string]any{
			"success": true,
			"version": version,
		})
	})
}

// checkFFmpeg runs "ffmpeg -version" with a 10-second timeout and returns
// whether it succeeded and the version string.
func checkFFmpeg(path string) (valid bool, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	out, err := cmd.Output()
	if err != nil {
		return false, ""
	}
	// Extract first line which contains version info
	output := string(out)
	if idx := strings.IndexByte(output, '\n'); idx > 0 {
		output = output[:idx]
	}
	return true, strings.TrimSpace(output)
}

// InstallFFmpeg runs a package manager to install FFmpeg. Supported methods:
// "choco" (existing Chocolatey), "choco-install" (install Chocolatey first),
// "winget". Refreshes the Windows PATH from registry after installation.
// Serialized via installMu to prevent concurrent package manager operations.
func InstallFFmpeg(method string) error {
	installMu.Lock()
	defer installMu.Unlock()
	switch method {
	case "choco":
		cmd := exec.Command("choco", "install", "ffmpeg-shared", "-y")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("choco install failed: %s", string(out))
		}

	case "choco-install":
		chocoCmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
			"Set-ExecutionPolicy Bypass -Scope Process -Force; "+
				"[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072; "+
				"iex ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))")
		if out, err := chocoCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("chocolatey install failed: %s", string(out))
		}
		RefreshWindowsPath()
		ffmpegCmd := exec.Command("choco", "install", "ffmpeg-shared", "-y")
		if out, err := ffmpegCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg install via choco failed: %s", string(out))
		}

	case "winget":
		cmd := exec.Command("winget", "install", "Gyan.FFmpeg.Shared", "--accept-package-agreements", "--accept-source-agreements")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("winget install failed: %s", string(out))
		}

	default:
		return fmt.Errorf("unknown install method: %s", method)
	}

	RefreshWindowsPath()
	return nil
}

// RefreshWindowsPath reads the system and user PATH from the Windows registry
// and merges new entries into the current process's PATH environment variable.
// Existing process-specific entries are preserved to avoid losing paths added
// by earlier operations in the same session.
func RefreshWindowsPath() {
	if runtime.GOOS != "windows" {
		return
	}

	// Collect all registry PATH entries into a set for deduplication.
	seen := make(map[string]bool)
	var registryParts []string

	addParts := func(raw string) {
		for _, p := range strings.Split(raw, ";") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			lower := strings.ToLower(p)
			if !seen[lower] {
				seen[lower] = true
				registryParts = append(registryParts, p)
			}
		}
	}

	// System PATH: HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment
	if out, err := exec.Command("reg", "query",
		`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
		"/v", "Path").Output(); err == nil {
		if p := extractRegValue(string(out)); p != "" {
			addParts(p)
		}
	}

	// User PATH: HKCU\Environment
	if out, err := exec.Command("reg", "query",
		`HKCU\Environment`, "/v", "Path").Output(); err == nil {
		if p := extractRegValue(string(out)); p != "" {
			addParts(p)
		}
	}

	if len(registryParts) == 0 {
		return
	}

	// Append any process-specific PATH entries not already in the registry set.
	for _, p := range strings.Split(os.Getenv("PATH"), ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		lower := strings.ToLower(p)
		if !seen[lower] {
			seen[lower] = true
			registryParts = append(registryParts, p)
		}
	}

	os.Setenv("PATH", strings.Join(registryParts, ";"))
}

// extractRegValue extracts the value from a "reg query" output line.
// Format: "    Path    REG_SZ    C:\Windows;..."  or  "    Path    REG_EXPAND_SZ    %SystemRoot%..."
func extractRegValue(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "path") {
			// Split on REG_SZ or REG_EXPAND_SZ
			for _, regType := range []string{"REG_EXPAND_SZ", "REG_SZ"} {
				if idx := strings.Index(line, regType); idx >= 0 {
					val := strings.TrimSpace(line[idx+len(regType):])
					return expandWindowsEnv(val)
				}
			}
		}
	}
	return ""
}

// windowsEnvVarRe matches Windows-style %VAR% environment variable references.
var windowsEnvVarRe = regexp.MustCompile(`%([^%]+)%`)

// expandWindowsEnv expands both Windows-style %VAR% and Unix-style $VAR references.
// Go's os.ExpandEnv only handles $VAR/${VAR}, so we first convert %VAR% to ${VAR}.
func expandWindowsEnv(s string) string {
	// Expand %VAR% by looking up each variable
	s = windowsEnvVarRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[1 : len(match)-1]
		if v := os.Getenv(name); v != "" {
			return v
		}
		return match // keep unexpanded if not found
	})
	return s
}
