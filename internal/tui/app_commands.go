package tui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// safeCmd wraps a tea.Cmd closure with panic recovery. If the closure panics,
// the recovery converts it into a panicRecoveryMsg that displays feedback.
func safeCmd(fn func() tea.Msg) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = panicRecoveryMsg{Text: fmt.Sprintf("unexpected error: %v", r)}
			}
		}()
		return fn()
	}
}

// apiBaseURL returns the correct scheme + host for local API calls.
func (a *App) apiBaseURL() string {
	scheme := "http"
	port := 774
	if a.configStore != nil {
		a.configStore.Read(func(c *config.MoomboxConfig) {
			if c.Network.HTTPSEnabled {
				scheme = "https"
			}
			if c.Network.Port > 0 {
				port = c.Network.Port
			}
		})
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)
}

// internalTokenTransport injects the X-Internal-Token header on every request
// so that local API calls bypass the CSRF middleware.
type internalTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t *internalTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req.Header.Set("X-Internal-Token", t.token)
	}
	return t.base.RoundTrip(req)
}

// apiClient returns an HTTP client suitable for local API calls.
// Injects the internal CSRF bypass token. When HTTPS is enabled, TLS
// verification is skipped since the server uses a self-signed certificate.
// The client is cached and rebuilt on HTTPS toggle (audit tui.md Finding 3).
func (a *App) apiClient() *http.Client {
	httpsEnabled := false
	if a.configStore != nil {
		a.configStore.Read(func(c *config.MoomboxConfig) {
			httpsEnabled = c.Network.HTTPSEnabled
		})
	}
	if a.cachedClient != nil && a.cachedClientHTTPS == httpsEnabled {
		return a.cachedClient
	}
	base := http.DefaultTransport
	if httpsEnabled {
		base = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	a.cachedClient = &http.Client{
		Transport: &internalTokenTransport{base: base, token: a.internalToken},
		// 30s is generous for a loopback call; chi has its own server-side
		// timeouts but defence-in-depth keeps a silent pipe stall from
		// hanging the TUI until the user force-quits.
		Timeout: 30 * time.Second,
	}
	a.cachedClientHTTPS = httpsEnabled
	return a.cachedClient
}

// addVideoCmd creates a job by POSTing to the local API (matches TS TUI behavior).
func (a *App) addVideoCmd(input string) tea.Cmd {
	platform := a.addVideo.GetPlatform()
	videoItag := a.addVideo.GetSelectedVideoItag()
	audioItag := a.addVideo.GetSelectedAudioItag()
	startTime := a.addVideo.GetStartTime()
	endTime := a.addVideo.GetEndTime()
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// Fire-and-forget OnAddVideo callback for logging
	if a.OnAddVideo != nil {
		a.OnAddVideo(input)
	}

	return safeCmd(func() tea.Msg {
		body := map[string]any{
			"videoId": input,
		}
		if platform == "twitch" {
			body["platform"] = "twitch"
			// Detect VOD vs channel
			if vodID, ok := strings.CutPrefix(input, "tw_v"); ok {
				body["twitchType"] = "vod"
				body["videoId"] = vodID
			} else {
				body["twitchType"] = "channel"
			}
		}
		if videoItag != nil {
			body["selectedVideoItag"] = *videoItag
		}
		if audioItag != nil {
			body["selectedAudioItag"] = *audioItag
		}
		if startTime != "" {
			body["startTime"] = startTime
		}
		if endTime != "" {
			body["endTime"] = endTime
		}

		jsonBody, _ := json.Marshal(body)
		url := fmt.Sprintf("%s/api/jobs", baseURL)
		resp, err := client.Post(url, "application/json", bytes.NewReader(jsonBody))
		if err != nil {
			return addVideoResultMsg{Feedback: "Failed to connect to server"}
		}
		defer resp.Body.Close()

		if resp.StatusCode == 409 {
			return addVideoResultMsg{Feedback: "Job already exists"}
		}
		if resp.StatusCode >= 400 {
			var errResp struct {
				Error string `json:"error"`
			}
			if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr != nil {
				return addVideoResultMsg{Feedback: fmt.Sprintf("Failed to add job (HTTP %d)", resp.StatusCode)}
			}
			msg := errResp.Error
			if msg == "" {
				msg = fmt.Sprintf("Failed to add job (HTTP %d)", resp.StatusCode)
			}
			return addVideoResultMsg{Feedback: msg}
		}

		label := "Added to queue"
		if platform == "twitch" {
			if strings.HasPrefix(input, "tw_v") {
				label = "Added Twitch VOD to queue"
			} else {
				label = "Added Twitch channel to queue"
			}
		}
		return addVideoResultMsg{Feedback: label}
	})
}

// fetchFormatsCmd fetches format options from the local API for advanced mode.
func (a *App) fetchFormatsCmd(videoID string) tea.Cmd {
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// If a callback is provided, use it directly (avoids HTTP round-trip)
	if a.OnFetchFormats != nil {
		cb := a.OnFetchFormats
		return safeCmd(func() tea.Msg {
			data, err := cb(videoID)
			if err != nil {
				return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
			}
			return fetchFormatsResultMsg{Formats: data}
		})
	}

	return safeCmd(func() tea.Msg {
		url := fmt.Sprintf("%s/api/formats/%s", baseURL, videoID)
		resp, err := client.Get(url)
		if err != nil {
			return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
		}

		var data FormatsData
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fetchFormatsResultMsg{Err: "Failed to parse format data. Proceeding with auto selection."}
		}
		return fetchFormatsResultMsg{Formats: &data}
	})
}

// importFileCmd reads a ZIP file and uploads it to the import API.
func (a *App) importFileCmd(path string) tea.Cmd {
	title := a.importDlg.GetImportTitle()
	channel := a.importDlg.GetImportChannel()
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// If a callback is provided, use it directly
	if a.OnImportFile != nil {
		cb := a.OnImportFile
		return safeCmd(func() tea.Msg {
			importedTitle, err := cb(path, title, channel)
			if err != nil {
				return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
			}
			return importResultMsg{Title: importedTitle}
		})
	}

	return safeCmd(func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		defer f.Close()

		url := fmt.Sprintf("%s/api/import", baseURL)
		req, err := http.NewRequest("POST", url, f)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		if title != "" {
			req.Header.Set("X-Import-Title", strings.TrimSpace(title))
		}
		if channel != "" {
			req.Header.Set("X-Import-Channel", strings.TrimSpace(channel))
		}

		resp, err := client.Do(req)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			var errResp struct {
				Error string `json:"error"`
			}
			if unmarshalErr := json.Unmarshal(body, &errResp); unmarshalErr != nil {
				return importResultMsg{Err: fmt.Sprintf("Import failed (HTTP %d)", resp.StatusCode)}
			}
			msg := errResp.Error
			if msg == "" {
				msg = fmt.Sprintf("Import failed (HTTP %d)", resp.StatusCode)
			}
			return importResultMsg{Err: msg}
		}

		var result struct {
			Title string `json:"title"`
		}
		if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
			// Non-fatal: we got a 2xx, just can't parse the title
			return importResultMsg{Title: "archive"}
		}
		importedTitle := result.Title
		if importedTitle == "" {
			importedTitle = strings.TrimSpace(title)
		}
		if importedTitle == "" {
			importedTitle = "archive"
		}
		return importResultMsg{Title: importedTitle}
	})
}

func (a *App) createTrimCmd(jobID string, startSec, endSec float64) tea.Cmd {
	createFn := a.OnCreateTrim
	progressMu := &a.trimProgressMu
	progressPct := &a.trimProgressPct
	return safeCmd(func() tea.Msg {
		if createFn == nil {
			return createTrimResultMsg{Err: "Create trim not available"}
		}
		onProgress := func(pct float64) {
			progressMu.Lock()
			*progressPct = pct
			progressMu.Unlock()
		}
		filename, errMsg := createFn(jobID, startSec, endSec, onProgress)
		if errMsg != "" {
			return createTrimResultMsg{Err: errMsg}
		}
		return createTrimResultMsg{Filename: filename}
	})
}

func (a *App) deleteTrimCmd(jobID, trimID string) tea.Cmd {
	// Capture state on the main goroutine to avoid data races in the closure.
	deleteFn := a.OnDeleteTrim
	var filename string
	for _, t := range a.trimDlg.trims {
		if t.ID == trimID {
			filename = t.Filename
			break
		}
	}
	return safeCmd(func() tea.Msg {
		if deleteFn == nil {
			return deleteTrimResultMsg{Err: "Delete trim not available"}
		}
		if err := deleteFn(jobID, trimID); err != nil {
			return deleteTrimResultMsg{Err: err.Error()}
		}
		return deleteTrimResultMsg{TrimID: trimID, Filename: filename}
	})
}

func (a *App) fetchOrphansCmd() tea.Cmd {
	listFn := a.OnListOrphans
	return safeCmd(func() tea.Msg {
		if listFn == nil {
			return fetchOrphansResultMsg{Err: "Not available"}
		}
		files, err := listFn()
		if err != nil {
			return fetchOrphansResultMsg{Err: err.Error()}
		}
		return fetchOrphansResultMsg{Files: files}
	})
}

func (a *App) fetchClientTokensCmd() tea.Cmd {
	listFn := a.OnListClientTokens
	return safeCmd(func() tea.Msg {
		if listFn == nil {
			return fetchClientTokensResultMsg{Err: "Not available"}
		}
		tokens, err := listFn()
		if err != nil {
			return fetchClientTokensResultMsg{Err: err.Error()}
		}
		return fetchClientTokensResultMsg{Tokens: tokens}
	})
}

func (a *App) deleteClientTokenCmd(id string) tea.Cmd {
	deleteFn := a.OnDeleteClientToken
	return safeCmd(func() tea.Msg {
		if deleteFn == nil {
			return deleteClientTokenResultMsg{ID: id, Err: "Not available"}
		}
		if err := deleteFn(id); err != nil {
			return deleteClientTokenResultMsg{ID: id, Err: err.Error()}
		}
		return deleteClientTokenResultMsg{ID: id}
	})
}

func (a *App) deleteOrphanCmd(path string) tea.Cmd {
	deleteFn := a.OnDeleteOrphan
	return safeCmd(func() tea.Msg {
		if deleteFn == nil {
			return deleteOrphanResultMsg{Path: path, Err: "Not available"}
		}
		if err := deleteFn(path); err != nil {
			return deleteOrphanResultMsg{Path: path, Err: err.Error()}
		}
		return deleteOrphanResultMsg{Path: path}
	})
}

// ffmpegCheckCmd runs FFmpeg path validation asynchronously via tea.Cmd.
func (a *App) ffmpegCheckCmd(path string) tea.Cmd {
	checkFn := a.OnCheckFFmpeg
	return safeCmd(func() tea.Msg {
		if checkFn == nil {
			return ffmpegCheckResultMsg{Valid: false, Path: path}
		}
		if path == "" {
			path = "ffmpeg"
		}
		valid, ver, warn := checkFn(path)
		return ffmpegCheckResultMsg{Valid: valid, Version: ver, Warning: warn, Path: path}
	})
}

// ffmpegPrepareCmd checks elevation and either installs directly or returns
// a script for review.
func (a *App) ffmpegPrepareCmd(method string) tea.Cmd {
	prepareFn := a.OnPrepareInstall
	return safeCmd(func() tea.Msg {
		if prepareFn == nil {
			return ffmpegPrepareResultMsg{Err: "install not available"}
		}
		needsElev, script, token, err := prepareFn(method)
		if err != nil {
			return ffmpegPrepareResultMsg{Err: err.Error()}
		}
		return ffmpegPrepareResultMsg{
			NeedsElevation: needsElev,
			Script:         script,
			Token:          token,
		}
	})
}

// ffmpegConfirmCmd executes a reviewed elevated install.
func (a *App) ffmpegConfirmCmd(token string) tea.Cmd {
	confirmFn := a.OnConfirmInstall
	return safeCmd(func() tea.Msg {
		if confirmFn == nil {
			return ffmpegConfirmResultMsg{Err: "confirm not available"}
		}
		if err := confirmFn(token); err != nil {
			return ffmpegConfirmResultMsg{Err: err.Error()}
		}
		return ffmpegConfirmResultMsg{}
	})
}

// testNotificationCmd delivers a test embed to url via the local API
// (POST /api/notifications/test) and reports the outcome back to the
// settings overlay through testNotificationResultMsg.
func (a *App) testNotificationCmd(url string) tea.Cmd {
	baseURL := a.apiBaseURL()
	client := a.apiClient()
	return safeCmd(func() tea.Msg {
		if url == "" {
			return testNotificationResultMsg{Err: "no notification selected"}
		}
		body, _ := json.Marshal(map[string]string{"url": url})
		resp, err := client.Post(baseURL+"/api/notifications/test", "application/json", bytes.NewReader(body))
		if err != nil {
			return testNotificationResultMsg{Err: "failed to connect to server"}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			var errResp struct {
				Error string `json:"error"`
			}
			if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr == nil && errResp.Error != "" {
				return testNotificationResultMsg{Err: errResp.Error}
			}
			return testNotificationResultMsg{Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return testNotificationResultMsg{}
	})
}

// resolveChannelCmd resolves a channel URL asynchronously via tea.Cmd.
func (a *App) resolveChannelCmd(input string) tea.Cmd {
	return safeCmd(func() tea.Msg {
		resolved, err := utils.ResolveChannelInput(context.Background(), input)
		if err != nil {
			return channelResolvedMsg{Err: err}
		}
		if resolved == nil {
			// Not a recognized URL — return input as-is
			return channelResolvedMsg{ID: input}
		}
		return channelResolvedMsg{
			ID:       resolved.ID,
			Name:     resolved.Name,
			Platform: resolved.Platform,
		}
	})
}

// openBrowser launches the default browser for the given URL using the
// platform-appropriate opener (mirrors OnOpenFolder in cmd/moombox/tui_wiring.go).
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// Run starts the TUI program.
func Run(app *App) error {
	p := tea.NewProgram(app)
	app.program.Store(p)
	_, err := p.Run()
	return err
}

// QuitTUI programmatically exits the TUI (used by restart to unblock Run).
func (a *App) QuitTUI() {
	if p := a.program.Load(); p != nil {
		p.Quit()
	}
}

// Send delivers an external message into the TUI's Update loop.
// Safe to call from any goroutine. No-op if the program hasn't started yet.
func (a *App) Send(msg tea.Msg) {
	if p := a.program.Load(); p != nil {
		p.Send(msg)
	}
}
