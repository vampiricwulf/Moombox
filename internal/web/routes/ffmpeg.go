package routes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/web"
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
	ffmpegCacheWarn   string
	ffmpegCachePath   string
	ffmpegCacheTime   time.Time
)

const ffmpegCacheTTL = 10 * time.Second

// CheckFFmpegCached returns a cached result of checkFFmpeg, refreshing
// only if the cache is stale or the path has changed.
func CheckFFmpegCached(path string) (valid bool, version string, warning string) {
	ffmpegCacheMu.Lock()
	defer ffmpegCacheMu.Unlock()

	now := time.Now()
	if ffmpegCacheValid && ffmpegCachePath == path && now.Sub(ffmpegCacheTime) < ffmpegCacheTTL {
		return ffmpegCacheResult, ffmpegCacheVer, ffmpegCacheWarn
	}

	valid, version, warning = checkFFmpeg(path)
	ffmpegCacheValid = true
	ffmpegCacheResult = valid
	ffmpegCacheVer = version
	ffmpegCacheWarn = warning
	ffmpegCachePath = path
	ffmpegCacheTime = now
	return valid, version, warning
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
	RateLimit  *web.RateLimiter
	Logger     interface {
		Info(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// FFmpegRoutes registers FFmpeg validation and installation endpoints.
func FFmpegRoutes(r chi.Router, deps *FFmpegDeps) {
	// GET /api/ffmpeg/check — check if ffmpeg is available on PATH or configured path
	r.Get("/api/ffmpeg/check", func(rw http.ResponseWriter, req *http.Request) {
		path := deps.Cfg.Paths.FfmpegPath
		if path == "" {
			path = "ffmpeg"
		}
		valid, version, warning := CheckFFmpegCached(path)
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
			"warning": warning,
		})
	})

	// Rate-limit mutating ffmpeg endpoints (spawns processes)
	rl := r.With()
	if deps.RateLimit != nil {
		rl = r.With(deps.RateLimit.Middleware)
	}

	// POST /api/ffmpeg/check — check a custom ffmpeg path, save to config if valid
	rl.Post("/api/ffmpeg/check", func(rw http.ResponseWriter, req *http.Request) {
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

		valid, version, warning := checkFFmpeg(path)
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
			"warning": warning,
		})
	})

	// GET /api/ffmpeg/install-options — check which package managers are available
	r.Get("/api/ffmpeg/install-options", func(rw http.ResponseWriter, req *http.Request) {
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

	// POST /api/ffmpeg/install — install ffmpeg via a package manager.
	// If the process is not elevated, returns a script for user review instead
	// of running directly.
	rl.Post("/api/ffmpeg/install", func(rw http.ResponseWriter, req *http.Request) {
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

		needsElevation, script, token, err := PrepareInstall(body.Method)
		if err != nil {
			jsonError(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		if needsElevation {
			jsonResponse(rw, map[string]any{
				"needsElevation": true,
				"script":         script,
				"token":          token,
			})
			return
		}

		// Ran directly (already elevated) — verify
		InvalidateFFmpegCache()
		valid, version, warning := checkFFmpeg("ffmpeg")
		if !valid {
			jsonError(rw, "FFmpeg installed but not found on PATH. You may need to restart.", http.StatusInternalServerError)
			return
		}

		deps.Logger.Info("FFmpeg installed successfully", "version", version)
		jsonResponse(rw, map[string]any{
			"success": true,
			"version": version,
			"warning": warning,
		})
	})

	// POST /api/ffmpeg/install/confirm — execute a previously reviewed install script
	rl.Post("/api/ffmpeg/install/confirm", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		tokenPreview := body.Token
		if len(tokenPreview) > 8 {
			tokenPreview = tokenPreview[:8] + "..."
		}
		deps.Logger.Info("Confirming elevated FFmpeg install", "token", tokenPreview)
		if err := ConfirmInstall(body.Token); err != nil {
			jsonError(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		InvalidateFFmpegCache()
		valid, version, warning := checkFFmpeg("ffmpeg")
		if !valid {
			jsonError(rw, "FFmpeg installed but not found on PATH. You may need to restart.", http.StatusInternalServerError)
			return
		}

		deps.Logger.Info("FFmpeg installed successfully via elevation", "version", version)
		jsonResponse(rw, map[string]any{
			"success": true,
			"version": version,
			"warning": warning,
		})
	})

	// POST /api/ffmpeg/install/reject — decline a pending elevated install
	rl.Post("/api/ffmpeg/install/reject", func(rw http.ResponseWriter, req *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		RejectInstall(body.Token)
		jsonResponse(rw, map[string]any{"success": true})
	})
}

// ffmpegVersionRe matches "ffmpeg version N.N" in the version output line.
var ffmpegVersionRe = regexp.MustCompile(`ffmpeg version (\d+)\.(\d+)`)

// parseFFmpegVersion extracts major.minor from an ffmpeg version line.
// Returns ok=false for unparseable formats (e.g. git builds).
func parseFFmpegVersion(versionLine string) (major, minor int, ok bool) {
	m := ffmpegVersionRe.FindStringSubmatch(versionLine)
	if m == nil {
		return 0, 0, false
	}
	fmt.Sscanf(m[1], "%d", &major)
	fmt.Sscanf(m[2], "%d", &minor)
	return major, minor, true
}

// ffmpegVersionWarning returns a warning if the ffmpeg version is below 4.0.
// Returns empty string for acceptable versions or unparseable formats (git builds).
func ffmpegVersionWarning(versionLine string) string {
	major, _, ok := parseFFmpegVersion(versionLine)
	if !ok {
		return "" // can't parse → don't warn (git builds, custom builds)
	}
	if major < 4 {
		return fmt.Sprintf("FFmpeg %d.x is outdated — version 4.0+ is recommended for reliable muxing", major)
	}
	return ""
}

// checkFFmpeg runs "ffmpeg -version" with a 10-second timeout and returns
// whether it succeeded, the version string, and an optional version warning.
func checkFFmpeg(path string) (valid bool, version string, warning string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	out, err := cmd.Output()
	if err != nil {
		return false, "", ""
	}
	// Extract first line which contains version info
	output := string(out)
	if idx := strings.IndexByte(output, '\n'); idx > 0 {
		output = output[:idx]
	}
	version = strings.TrimSpace(output)
	return true, version, ffmpegVersionWarning(version)
}

// truncateOutput keeps the last maxBytes of output, prepending a truncation
// marker if the output was trimmed. Used to cap error messages from package
// manager commands that can emit megabytes of log output.
func truncateOutput(out []byte, maxBytes int) string {
	if len(out) <= maxBytes {
		return string(out)
	}
	return "[...truncated...]\n" + string(out[len(out)-maxBytes:])
}

const installTimeout = 5 * time.Minute

// Install method constants — used in switch statements, API validation, and
// generateInstallScript. Frontend sends these as the wire format.
const (
	MethodChoco        = "choco"
	MethodChocoInstall = "choco-install"
	MethodWinget       = "winget"
)

// InstallFFmpeg runs a package manager to install FFmpeg. Supported methods:
// "choco" (existing Chocolatey), "choco-install" (install Chocolatey first),
// "winget". Refreshes the Windows PATH from registry after installation.
// Serialized via installMu to prevent concurrent package manager operations.
// All commands share a single 5-minute deadline.
func InstallFFmpeg(method string) error {
	installMu.Lock()
	defer installMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()

	switch method {
	case MethodChoco:
		cmd := exec.CommandContext(ctx, "choco", "install", "ffmpeg-shared", "-y")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("choco install failed: %s", truncateOutput(out, 500))
		}

	case MethodChocoInstall:
		chocoCmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
			"Set-ExecutionPolicy Bypass -Scope Process -Force; "+
				"[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072; "+
				"iex ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))")
		if out, err := chocoCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("chocolatey install failed: %s", truncateOutput(out, 500))
		}
		RefreshWindowsPath()
		ffmpegCmd := exec.CommandContext(ctx, "choco", "install", "ffmpeg-shared", "-y")
		if out, err := ffmpegCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg install via choco failed: %s", truncateOutput(out, 500))
		}

	case MethodWinget:
		cmd := exec.CommandContext(ctx, "winget", "install", "Gyan.FFmpeg.Shared", "--accept-package-agreements", "--accept-source-agreements")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("winget install failed: %s", truncateOutput(out, 500))
		}

	default:
		return fmt.Errorf("unknown install method: %s", method)
	}

	RefreshWindowsPath()
	return nil
}

// pendingInstall holds state for a two-phase elevated install: phase 1 generates
// the script and returns it for review, phase 2 executes it after user approval.
type pendingInstall struct {
	method     string
	scriptPath string
	resultPath string
	script     string
	createdAt  time.Time
}

const pendingInstallTTL = 5 * time.Minute

var (
	pendingInstallsMu sync.Mutex
	pendingInstalls   = make(map[string]*pendingInstall)
)

// cleanExpiredPending removes expired pending installs. Must be called with
// pendingInstallsMu held.
func cleanExpiredPending() {
	now := time.Now()
	for token, pi := range pendingInstalls {
		if now.Sub(pi.createdAt) > pendingInstallTTL {
			os.Remove(pi.scriptPath)
			os.Remove(pi.resultPath)
			delete(pendingInstalls, token)
		}
	}
}

// generateInstallScript creates a PowerShell script that runs the package
// manager command and writes a JSON result file with exit code and output.
func generateInstallScript(method, resultPath string) (string, error) {
	var installCmd string
	switch method {
	case MethodChoco:
		installCmd = `$output += (& choco install ffmpeg-shared -y 2>&1 | Out-String)`
	case MethodChocoInstall:
		installCmd = `$output += (& powershell -NoProfile -ExecutionPolicy Bypass -Command "Set-ExecutionPolicy Bypass -Scope Process -Force; [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072; iex ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))" 2>&1 | Out-String)` + "\n" +
			`$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [System.Environment]::GetEnvironmentVariable('Path', 'User')` + "\n" +
			`$output += (& choco install ffmpeg-shared -y 2>&1 | Out-String)`
	case MethodWinget:
		installCmd = `$output += (& winget install Gyan.FFmpeg.Shared --accept-package-agreements --accept-source-agreements 2>&1 | Out-String)`
	default:
		return "", fmt.Errorf("unknown install method: %s", method)
	}

	// PowerShell double-quoted strings treat backslashes literally (not as
	// escape characters), so no escaping needed for Windows paths.
	script := `$ErrorActionPreference = 'Continue'
$output = ""
try {
` + installCmd + `
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) { $exitCode = 0 }
} catch {
    $output += $_.Exception.Message
    $exitCode = 1
}
$result = @{ exitCode = $exitCode; output = $output } | ConvertTo-Json
[IO.File]::WriteAllText('` + resultPath + `', $result)
`
	return script, nil
}

// generateToken creates a random 16-byte hex token for pending install tracking.
func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate secure token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// PrepareInstall checks elevation and either runs the install directly or
// returns a pending install for user review. Used by both the API handler and
// the TUI callback.
func PrepareInstall(method string) (needsElevation bool, script string, token string, err error) {
	if runtime.GOOS != "windows" {
		return false, "", "", fmt.Errorf("automatic installation is only supported on Windows")
	}

	switch method {
	case MethodChoco, MethodChocoInstall, MethodWinget:
	default:
		return false, "", "", fmt.Errorf("unknown install method: %s", method)
	}

	if isElevated() {
		// Already elevated — run directly
		if err := InstallFFmpeg(method); err != nil {
			return false, "", "", err
		}
		return false, "", "", nil
	}

	// Not elevated — generate script for review
	resultToken, err := generateToken()
	if err != nil {
		return false, "", "", err
	}
	resultPath := filepath.Join(os.TempDir(), "moombox-ffmpeg-result-"+resultToken+".json")
	scriptContent, err := generateInstallScript(method, resultPath)
	if err != nil {
		return false, "", "", err
	}

	token, err = generateToken()
	if err != nil {
		return false, "", "", err
	}
	scriptPath := filepath.Join(os.TempDir(), "moombox-ffmpeg-install-"+token+".ps1")

	pendingInstallsMu.Lock()
	cleanExpiredPending()
	pendingInstalls[token] = &pendingInstall{
		method:     method,
		scriptPath: scriptPath,
		resultPath: resultPath,
		script:     scriptContent,
		createdAt:  time.Now(),
	}
	pendingInstallsMu.Unlock()

	return true, scriptContent, token, nil
}

// ConfirmInstall executes a previously prepared elevated install after user
// approval. Writes the script to a temp file, launches it via UAC, waits for
// completion, and reads the result.
func ConfirmInstall(token string) error {
	installMu.Lock()
	defer installMu.Unlock()

	pendingInstallsMu.Lock()
	cleanExpiredPending()
	pi, ok := pendingInstalls[token]
	if !ok {
		pendingInstallsMu.Unlock()
		return fmt.Errorf("install token not found or expired")
	}
	delete(pendingInstalls, token)
	pendingInstallsMu.Unlock()

	// Write script to temp file
	if err := os.WriteFile(pi.scriptPath, []byte(pi.script), 0600); err != nil {
		return fmt.Errorf("failed to write install script: %w", err)
	}
	defer os.Remove(pi.scriptPath)
	defer os.Remove(pi.resultPath)

	// Execute with elevation
	handle, err := runElevated(pi.scriptPath)
	if err != nil {
		return fmt.Errorf("elevation failed: %w", err)
	}

	// Wait for the elevated process to finish
	if err := waitForProcess(handle, installTimeout); err != nil {
		return fmt.Errorf("install process: %w", err)
	}

	// Read result JSON
	resultData, err := os.ReadFile(pi.resultPath)
	if err != nil {
		return fmt.Errorf("install may have succeeded but result file not found — restart to check")
	}

	var result struct {
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(resultData, &result); err != nil {
		return fmt.Errorf("failed to parse install result: %w", err)
	}

	if result.ExitCode != 0 {
		return fmt.Errorf("install failed (exit %d): %s", result.ExitCode, truncateOutput([]byte(result.Output), 500))
	}

	RefreshWindowsPath()
	return nil
}

// RejectInstall cleans up a pending install that the user declined.
func RejectInstall(token string) {
	pendingInstallsMu.Lock()
	if pi, ok := pendingInstalls[token]; ok {
		os.Remove(pi.scriptPath)
		os.Remove(pi.resultPath)
		delete(pendingInstalls, token)
	}
	pendingInstallsMu.Unlock()
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
