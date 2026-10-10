package tui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// safeCmd wraps a tea.Cmd closure with panic recovery. If the closure panics,
// the recovery converts it into a panicRecoveryMsg that displays feedback.
func safeCmd(fn func() tea.Msg) tea.Cmd {
	return safeCmdOr(fn, func(text string) tea.Msg { return panicRecoveryMsg{Text: text} })
}

// safeCmdOr is safeCmd for a command a dialog is waiting on: a recovered panic
// is answered with onPanic's message — the command's own error result —
// rather than a generic panicRecoveryMsg. The generic one says nothing about
// WHICH command died, so the dialog that asked never heard back: the import
// overlay spun on step 2 with no key to leave it, trimInProgress stayed set
// for the session, and the install and add spinners ran forever, each with
// its explanation on a feedback line the overlay covers.
func safeCmdOr(fn func() tea.Msg, onPanic func(text string) tea.Msg) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = onPanic(fmt.Sprintf("unexpected error: %v", r))
			}
		}()
		return fn()
	}
}

// apiBaseURL returns the correct scheme + host for local API calls.
func (a *App) apiBaseURL() string {
	scheme := "http"
	if a.httpsActive() {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, a.getPort())
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
	httpsEnabled := a.httpsActive()
	if a.cachedClient != nil && a.cachedClientHTTPS == httpsEnabled {
		return a.cachedClient
	}
	// The shared transport shape lives in internal/httpx, which declares
	// itself the single source of truth for Moombox's *http.Client shapes;
	// this was the one hand-built &http.Transport{} outside it (TOOL-18).
	// A fresh transport per HTTPS toggle matches the previous behaviour (the
	// HTTPS branch already built one each time) and the client is cached, so
	// a toggle is the only thing that builds another; its idle connections
	// expire on the shared 90 s IdleConnTimeout.
	base := httpx.NewTransport(httpx.TransportOptions{})
	if httpsEnabled {
		// The web server presents a self-signed certificate on loopback.
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	a.cachedClient = httpx.ClientWithTransport(
		// 30s is generous for a loopback call; chi has its own server-side
		// timeouts but defence-in-depth keeps a silent pipe stall from
		// hanging the TUI until the user force-quits.
		30*time.Second,
		&internalTokenTransport{base: base, token: a.internalToken},
	)
	a.cachedClientHTTPS = httpsEnabled
	return a.cachedClient
}

// addVideoCmd creates a job by POSTing to the local API (matches TS TUI behavior).
func (a *App) addVideoCmd(input string) tea.Cmd {
	platform := a.addVideo.GetPlatform()
	videoItag := a.addVideo.GetSelectedVideoItag()
	audioItag := a.addVideo.GetSelectedAudioItag()
	startTime, endTime := a.addVideo.TimeRange()
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// Fire-and-forget OnAddVideo callback for logging
	if a.OnAddVideo != nil {
		a.OnAddVideo(input)
	}

	return safeCmdOr(func() tea.Msg {
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
		if startTime != nil {
			body["startTime"] = *startTime
		}
		if endTime != nil {
			body["endTime"] = *endTime
		}

		jsonBody, _ := json.Marshal(body)
		url := fmt.Sprintf("%s/api/jobs", baseURL)
		resp, err := client.Post(url, "application/json", bytes.NewReader(jsonBody))
		if err != nil {
			return addVideoResultMsg{VideoID: input, Feedback: "Failed to connect to server"}
		}
		defer resp.Body.Close()

		if resp.StatusCode == 409 {
			return addVideoResultMsg{VideoID: input, Feedback: "Job already exists"}
		}
		if resp.StatusCode >= 400 {
			var errResp struct {
				Error string `json:"error"`
			}
			if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr != nil {
				return addVideoResultMsg{VideoID: input, Feedback: fmt.Sprintf("Failed to add job (HTTP %d)", resp.StatusCode)}
			}
			msg := errResp.Error
			if msg == "" {
				msg = fmt.Sprintf("Failed to add job (HTTP %d)", resp.StatusCode)
			}
			return addVideoResultMsg{VideoID: input, Feedback: msg}
		}

		label := "Added to queue"
		if platform == "twitch" {
			if strings.HasPrefix(input, "tw_v") {
				label = "Added Twitch VOD to queue"
			} else {
				label = "Added Twitch channel to queue"
			}
		}
		return addVideoResultMsg{VideoID: input, Feedback: label}
	}, func(text string) tea.Msg { return addVideoResultMsg{VideoID: input, Feedback: text} })
}

// fetchFormatsCmd fetches format options from the local API for advanced mode.
func (a *App) fetchFormatsCmd(videoID string) tea.Cmd {
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// If a callback is provided, use it directly (avoids HTTP round-trip)
	if a.OnFetchFormats != nil {
		cb := a.OnFetchFormats
		return safeCmdOr(func() tea.Msg {
			data, err := cb(videoID)
			if err != nil {
				return fetchFormatsResultMsg{VideoID: videoID, Err: "Failed to fetch formats. Proceeding with auto selection."}
			}
			return fetchFormatsResultMsg{VideoID: videoID, Formats: data}
		}, func(text string) tea.Msg { return fetchFormatsResultMsg{VideoID: videoID, Err: text} })
	}

	return safeCmdOr(func() tea.Msg {
		url := fmt.Sprintf("%s/api/formats/%s", baseURL, videoID)
		resp, err := client.Get(url)
		if err != nil {
			return fetchFormatsResultMsg{VideoID: videoID, Err: "Failed to fetch formats. Proceeding with auto selection."}
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fetchFormatsResultMsg{VideoID: videoID, Err: "Failed to fetch formats. Proceeding with auto selection."}
		}

		var data FormatsData
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fetchFormatsResultMsg{VideoID: videoID, Err: "Failed to parse format data. Proceeding with auto selection."}
		}
		return fetchFormatsResultMsg{VideoID: videoID, Formats: &data}
	}, func(text string) tea.Msg { return fetchFormatsResultMsg{VideoID: videoID, Err: text} })
}

// importTimeout bounds the whole import exchange. The shared API client's
// 30 s covers a loopback call that only answers; an import UPLOADS up to
// 500 MB and the server then extracts up to 2 GB before it responds, so on a
// slow disk a good import outlived 30 s, was reported "Import failed: context
// deadline exceeded" while the server finished it, and a retry then hit 409.
const importTimeout = 30 * time.Minute

// importClient is the API client with importTimeout in place of its own: the
// same transport (and so the same internal token and TLS handling).
func importClient(api *http.Client) *http.Client {
	c := *api
	c.Timeout = importTimeout
	return &c
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
		return safeCmdOr(func() tea.Msg {
			importedTitle, err := cb(path, title, channel)
			if err != nil {
				return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
			}
			return importResultMsg{Title: importedTitle}
		}, func(text string) tea.Msg { return importResultMsg{Err: text} })
	}

	return safeCmdOr(func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		defer f.Close()

		req, err := newImportRequest(baseURL, f, title, channel)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}

		resp, err := importClient(client).Do(req)
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

		// "import" is what became of a name already taken in imports/
		// (importOutcome, internal/web/routes/import_routes.go): a
		// byte-identical file re-adopted, or a different one left alone
		// while this archive took " (2)" — and any chat left out for
		// matching no video's name.
		var result struct {
			Title  string `json:"title"`
			Import struct {
				Renamed       []json.RawMessage `json:"renamed"`
				UnpairedChats []string          `json:"unpairedChats"`
				Note          string            `json:"note"`
			} `json:"import"`
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
		return importResultMsg{
			Title: importedTitle,
			Note:  result.Import.Note,
			Warn:  len(result.Import.Renamed) > 0 || len(result.Import.UnpairedChats) > 0,
		}
	}, func(text string) tea.Msg { return importResultMsg{Err: text} })
}

// importCookieFileCmd runs OnImportCookieFile off the UI goroutine.
//
// The PATH is the only thing that crosses this seam, in either direction: the
// callback reads the file in cmd/moombox and hands back a cookies.ImportResult,
// so no cookie byte is ever held by a tea.Msg, rendered, or logged. Only the
// path is ever shown.
//
// No HTTP fallback, unlike importFileCmd: the import writes cookies.txt and
// then verifies each platform against the live services, and there is no
// meaningful version of that against a remote dashboard the operator may not
// even be authenticated to.
func (a *App) importCookieFileCmd(path string) tea.Cmd {
	fn := a.OnImportCookieFile
	return safeCmdOr(func() tea.Msg {
		if fn == nil {
			// Unreachable from the keyboard — with no callback the chord is
			// not registered — but a nil call here would panic the command
			// goroutine rather than the UI, which is the worse failure.
			return cookieImportResultMsg{Err: errors.New("cookie import is not available in this process")}
		}
		res, err := fn(path)
		return cookieImportResultMsg{Result: res, Err: err}
	}, func(text string) tea.Msg { return cookieImportResultMsg{Err: errors.New(text)} })
}

func (a *App) createTrimCmd(jobID string, startSec, endSec float64) tea.Cmd {
	createFn := a.OnCreateTrim
	progressMu := &a.trimProgressMu
	progressPct := &a.trimProgressPct
	return safeCmdOr(func() tea.Msg {
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
	}, func(text string) tea.Msg { return createTrimResultMsg{Err: text} })
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
	return safeCmdOr(func() tea.Msg {
		if deleteFn == nil {
			return deleteTrimResultMsg{JobID: jobID, Err: "Delete trim not available"}
		}
		if err := deleteFn(jobID, trimID); err != nil {
			return deleteTrimResultMsg{JobID: jobID, Err: err.Error()}
		}
		return deleteTrimResultMsg{JobID: jobID, TrimID: trimID, Filename: filename}
	}, func(text string) tea.Msg { return deleteTrimResultMsg{JobID: jobID, Err: text} })
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

// ytdlpStatusCmd reads the yt-dlp plugin's state off the UI goroutine — the
// E Y overlay's open, its R key, and the reload that follows a successful
// install all go through it.
func (a *App) ytdlpStatusCmd() tea.Cmd {
	statusFn := a.OnYtdlpPluginStatus
	return safeCmd(func() tea.Msg {
		if statusFn == nil {
			// Unreachable from the keyboard — with no callback the chord is
			// not registered — but a nil call here would take down the
			// command goroutine rather than report anything.
			return ytdlpStatusMsg{Err: errors.New("yt-dlp plugin status is not available in this process")}
		}
		info, err := statusFn()
		return ytdlpStatusMsg{Info: info, Err: err}
	})
}

// ytdlpInstallCmd rewrites the plugin for the live port. The keypress arm has
// already refused the nil case with a message on the overlay; this guard is
// for a direct caller.
func (a *App) ytdlpInstallCmd() tea.Cmd {
	installFn := a.OnInstallYtdlpPlugin
	return safeCmdOr(func() tea.Msg {
		if installFn == nil {
			return ytdlpInstallResultMsg{Err: errors.New("yt-dlp plugin install is not available in this process")}
		}
		return ytdlpInstallResultMsg{Err: installFn()}
	}, func(text string) tea.Msg { return ytdlpInstallResultMsg{Err: errors.New(text)} })
}

// fetchStatsCmd runs OnGetStats off the UI goroutine, tagging the result with
// the overlay session (App.statsEpoch) that asked for it.
func (a *App) fetchStatsCmd(epoch int) tea.Cmd {
	fn := a.OnGetStats
	return safeCmd(func() tea.Msg {
		snap, err := fn()
		return statsSnapshotMsg{Epoch: epoch, Snap: snap, Err: err}
	})
}

// statsRefreshInterval is the E T overlay's refresh cadence while open — the
// Web Stats tab's own poll interval.
const statsRefreshInterval = 60 * time.Second

// statsRefreshTick schedules the overlay's 60 s refresh (the Web's poll) for
// one overlay session. Only two places call it: the E T open, which starts
// the session's single chain, and the tick arm, which re-arms that same
// chain. A fetch result never does — that is what multiplied the chains.
func statsRefreshTick(epoch int) tea.Cmd {
	return tea.Tick(statsRefreshInterval, func(time.Time) tea.Msg { return statsRefreshTickMsg{Epoch: epoch} })
}

// fetchJobLogCmd reads one job's log buffer through OnGetJobLogs off the UI
// goroutine, tagging the lines with the O L session (App.jobLogEpoch) that
// asked for them. The ID and the callback are captured here, on the update
// goroutine; the closure touches no App field.
func (a *App) fetchJobLogCmd(epoch int, jobID string) tea.Cmd {
	fn := a.OnGetJobLogs
	return safeCmd(func() tea.Msg {
		return jobLogLinesMsg{Epoch: epoch, Lines: fn(jobID)}
	})
}

// jobLogRefreshTick schedules the O L overlay's next read for one session.
// As with statsRefreshTick only the open and the tick arm call it, so a
// session has one chain however many reads land.
func jobLogRefreshTick(epoch int) tea.Cmd {
	return tea.Tick(jobLogRefreshInterval, func(time.Time) tea.Msg { return jobLogRefreshTickMsg{Epoch: epoch} })
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

func (a *App) fetchOrphanedHistoryCmd() tea.Cmd {
	listFn := a.OnListOrphanedHistory
	return safeCmd(func() tea.Msg {
		if listFn == nil {
			return fetchOrphanedHistoryResultMsg{Err: "Not available"}
		}
		entries, err := listFn()
		if err != nil {
			return fetchOrphanedHistoryResultMsg{Err: err.Error()}
		}
		return fetchOrphanedHistoryResultMsg{Entries: entries}
	})
}

func (a *App) deleteHistoryEntryCmd(videoID string) tea.Cmd {
	deleteFn := a.OnDeleteHistoryEntry
	return safeCmd(func() tea.Msg {
		if deleteFn == nil {
			return deleteHistoryEntryResultMsg{VideoID: videoID, Err: "Not available"}
		}
		if err := deleteFn(videoID); err != nil {
			return deleteHistoryEntryResultMsg{VideoID: videoID, Err: err.Error()}
		}
		return deleteHistoryEntryResultMsg{VideoID: videoID}
	})
}

// deleteAllOrphansCmd loops the per-item callback over every orphaned-file
// path in the section, collecting failures instead of stopping the sweep at
// the first one so the dialog can name exactly what didn't go. The list is
// refreshed afterward by the bulkOrphanResultMsg arm.
func (a *App) deleteAllOrphansCmd(paths []string) tea.Cmd {
	fn := a.OnDeleteOrphan
	return safeCmd(func() tea.Msg {
		if fn == nil {
			failures := make([]string, 0, len(paths))
			for _, p := range paths {
				failures = append(failures, filepath.Base(p)+": Not available")
			}
			return bulkOrphanResultMsg{Failures: failures}
		}
		var failures []string
		deleted := 0
		for _, p := range paths {
			if err := fn(p); err != nil {
				failures = append(failures, filepath.Base(p)+": "+err.Error())
				continue
			}
			deleted++
		}
		return bulkOrphanResultMsg{Deleted: deleted, Failures: failures}
	})
}

// deleteAllHistoryCmd is the history-half twin of deleteAllOrphansCmd, over
// OnDeleteHistoryEntry. Failures are named by video ID rather than a
// filesystem path.
func (a *App) deleteAllHistoryCmd(ids []string) tea.Cmd {
	fn := a.OnDeleteHistoryEntry
	return safeCmd(func() tea.Msg {
		if fn == nil {
			failures := make([]string, 0, len(ids))
			for _, id := range ids {
				failures = append(failures, id+": Not available")
			}
			return bulkOrphanResultMsg{Failures: failures}
		}
		var failures []string
		deleted := 0
		for _, id := range ids {
			if err := fn(id); err != nil {
				failures = append(failures, id+": "+err.Error())
				continue
			}
			deleted++
		}
		return bulkOrphanResultMsg{Deleted: deleted, Failures: failures}
	})
}

// ffmpegCheckCmd runs FFmpeg path validation asynchronously via tea.Cmd.
func (a *App) ffmpegCheckCmd(path string) tea.Cmd {
	checkFn := a.OnCheckFFmpeg
	return safeCmdOr(func() tea.Msg {
		if checkFn == nil {
			return ffmpegCheckResultMsg{Valid: false, Path: path}
		}
		if path == "" {
			path = "ffmpeg"
		}
		valid, ver, warn := checkFn(path)
		return ffmpegCheckResultMsg{Valid: valid, Version: ver, Warning: warn, Path: path}
	}, func(text string) tea.Msg { return ffmpegCheckResultMsg{Valid: false, Path: path} })
}

// ffmpegPrepareCmd checks elevation and either installs directly or returns
// a script for review.
func (a *App) ffmpegPrepareCmd(method string) tea.Cmd {
	prepareFn := a.OnPrepareInstall
	return safeCmdOr(func() tea.Msg {
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
	}, func(text string) tea.Msg { return ffmpegPrepareResultMsg{Err: text} })
}

// ffmpegConfirmCmd executes a reviewed elevated install.
func (a *App) ffmpegConfirmCmd(token string) tea.Cmd {
	confirmFn := a.OnConfirmInstall
	return safeCmdOr(func() tea.Msg {
		if confirmFn == nil {
			return ffmpegConfirmResultMsg{Err: "confirm not available"}
		}
		if err := confirmFn(token); err != nil {
			return ffmpegConfirmResultMsg{Err: err.Error()}
		}
		return ffmpegConfirmResultMsg{}
	}, func(text string) tea.Msg { return ffmpegConfirmResultMsg{Err: text} })
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

// normalizeChannelID is utils.NormalizeChannelID behind a variable, so the
// TUI tests can answer a handle's lookup without reaching youtube.com.
var normalizeChannelID = utils.NormalizeChannelID

// resolveChannelCmd runs a channel editor's ID — a URL or a bare @handle —
// through utils.NormalizeChannelID, the normaliser every channel writer
// shares, off the update loop: a handle is a page fetch with retries. Both
// editors (Settings and the setup wizard) receive the answer and the one
// waiting takes it. An input that names no channel comes back as
// ErrNotChannelURL; it used to come back as its own ID, and was saved — a
// watch URL stored as a channel the monitors polled forever. A panic answers
// the editor too, so it is not left resolving.
func (a *App) resolveChannelCmd(input string) tea.Cmd {
	return safeCmdOr(func() tea.Msg {
		resolved, err := normalizeChannelID(context.Background(), input)
		if err != nil {
			return channelResolvedMsg{Input: input, Err: err}
		}
		return channelResolvedMsg{
			Input:    input,
			ID:       resolved.ID,
			Name:     resolved.Name,
			Platform: resolved.Platform,
		}
	}, func(text string) tea.Msg { return channelResolvedMsg{Input: input, Err: errors.New(text)} })
}

// openBrowser launches the default browser for the given URL using the
// platform-appropriate opener (mirrors OnOpenFolder in cmd/moombox/tui_wiring.go).
func openBrowser(url string) {
	// Platform-split (openbrowser_windows.go / openbrowser_other.go):
	// Windows needs explorer.exe with a forced-quoted command line so the
	// browser escapes the launcher's Job Object AND query-string URLs
	// survive explorer's legacy argument parser.
	cmd := openBrowserCmd(url)
	if err := cmd.Start(); err != nil {
		return
	}
	releaseOpener(runtime.GOOS, openerProcess{cmd})
}

// opener is the half of a started opener command releaseOpener uses: an
// interface so both arms can be tested without opening a browser.
type opener interface {
	Wait() error
	Release() error
}

// openerProcess adapts *exec.Cmd to opener.
type openerProcess struct{ cmd *exec.Cmd }

func (p openerProcess) Wait() error    { return p.cmd.Wait() }
func (p openerProcess) Release() error { return p.cmd.Process.Release() }

// releaseOpener hands a started opener back to the OS — the rule
// web.StartDetached applies to the dashboard's opens, which the import fence
// keeps this package from calling. On Windows it releases the process handle:
// there is nothing to reap, and each O S / O W / O G press leaked one for the
// life of the process. Elsewhere it reaps the child in a goroutine, since an
// unwaited child stays a zombie until Moombox exits.
func releaseOpener(goos string, p opener) {
	if goos == "windows" {
		_ = p.Release()
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				_ = r // nothing to report to: a backstop, as in web.detachStarted
			}
		}()
		_ = p.Wait()
	}()
}

// newImportRequest builds the archive-import POST. The metadata headers are
// percent-encoded (url.PathEscape) because HTTP headers are Latin-1 and the
// server PathUnescapes them — compatible with the Web UI's encodeURIComponent
// (the server's url.PathUnescape decodes both; PathEscape additionally escapes
// !'()*). Blank values set no header.
func newImportRequest(baseURL string, body io.Reader, title, channel string) (*http.Request, error) {
	req, err := http.NewRequest("POST", baseURL+"/api/import", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if t := strings.TrimSpace(title); t != "" {
		req.Header.Set("X-Import-Title", url.PathEscape(t))
	}
	if c := strings.TrimSpace(channel); c != "" {
		req.Header.Set("X-Import-Channel", url.PathEscape(c))
	}
	return req, nil
}

// Run starts the TUI program.
//
// tea.WithFPS raises the renderer from bubbletea's default 60 to tuiTargetFPS
// (120, its maximum). This is the package's ONLY tea.NewProgram site, and
// TestTheOneProgramIsBuiltWithTheTargetFPS is what keeps it that way.
func Run(app *App) error {
	p := tea.NewProgram(app, tea.WithFPS(tuiTargetFPS))
	app.program.Store(p)
	_, err := p.Run()
	// A SIGINT delivered from outside (kill -INT) surfaces as ErrInterrupted;
	// it is a request to quit like any other, not a TUI failure.
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
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
