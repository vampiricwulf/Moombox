package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQualityLost signals that the stream is still live but the selected
// quality variant/format has become unavailable (e.g. transcode removed).
var ErrQualityLost = errors.New("stream quality became unavailable")

const (
	CatchupThreshold  = 10
	MaxSegmentRetries = 5
	ParallelDownloads       = 6  // Bounded parallel downloads during catch-up
	DefaultRetryDelayCap    = 60 // seconds
	HeadProbeInterval       = 5 * time.Second
	SegmentTimeout          = 30 * time.Second
	NoSegmentTimeout        = 10 * time.Minute
	ResumeSeqInterval       = 50 // Save resume state every N sequential segments
	ResumeCatchupInterval   = 10 // Save resume state every N catch-up segments
	DownloadChunkSize       = 5 * 1024 * 1024       // 5MB chunks for VOD downloads (matches TS)
	MaxChunkRetries         = 3                      // Per-chunk retry limit
	ProgressThrottle        = 500 * time.Millisecond // Throttle VOD progress emission

	// User-Agent strings for download requests.
	uaWeb     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	uaAndroid = "com.google.android.youtube/19.09.37 (Linux; U; Android 14; en_US) gzip"
)

// DownloaderOptions configures a SegmentDownloader.
type DownloaderOptions struct {
	BaseURL            string
	OutputFile         string
	StartSeq           int
	EndSeq             int // -1 for unlimited
	PoToken            string
	CookieHeader       string // Cookie header for authenticated downloads
	IsHls              bool
	IsDirectURL        bool // Direct URL download (not segmented)
	MaxRetries         int
	InitURL            string
	ResumeFile         string
	RetryDelayCap      int // seconds
	LiveCheckRetries   int
	CheckStreamStatus  func(ctx context.Context) (bool, error) // Returns true if stream ended
	Logger             DownloaderLogger
}

// ResumeState holds download progress for crash recovery.
type ResumeState struct {
	LastSeq      int    `json:"lastSeq"`
	BytesWritten int64  `json:"bytesWritten"`
	Timestamp    int64  `json:"timestamp"`
	BaseURL      string `json:"baseUrl"`
}

// DownloadProgress holds progress information for event callbacks.
type DownloadProgress struct {
	Seq        int
	Bytes      int64
	HeadSeq    int
	Total      int
	Percent    float64
	TotalBytes int64 // Total file size for VOD chunked downloads (0 if unknown)
	CatchingUp bool
}

// DownloadGap represents a detected gap in segments.
type DownloadGap struct {
	From   int
	To     int
	Stream string
}

// SegmentDownloader downloads DASH or HLS segments sequentially/in parallel.
// DownloaderLogger is the interface for downloader logging.
type DownloaderLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

type SegmentDownloader struct {
	opts              DownloaderOptions
	mu                sync.Mutex
	running           bool
	cancelled         atomic.Bool
	streamEnded       bool
	outputFile        *os.File
	bytesWritten      atomic.Int64
	currentSeq        int
	headSeq           int
	lastSegTime       time.Time
	lastHeadProbeTime time.Time
	logger            DownloaderLogger

	// Callbacks
	OnStart    func(seq int, resuming bool)
	OnProgress func(p DownloadProgress)
	OnGap      func(g DownloadGap)
	OnFinish   func()
}

// NewSegmentDownloader creates a new segment downloader.
func NewSegmentDownloader(opts DownloaderOptions) *SegmentDownloader {
	if opts.MaxRetries == 0 {
		opts.MaxRetries = MaxSegmentRetries
	}
	if opts.RetryDelayCap == 0 {
		opts.RetryDelayCap = DefaultRetryDelayCap
	}
	if opts.LiveCheckRetries == 0 {
		opts.LiveCheckRetries = 16
	}
	if opts.EndSeq == 0 {
		opts.EndSeq = -1
	}
	if opts.ResumeFile == "" {
		opts.ResumeFile = opts.OutputFile + ".resume.json"
	}

	logger := opts.Logger
	if logger == nil {
		logger = nopLogger{}
	}

	return &SegmentDownloader{
		opts:       opts,
		currentSeq: opts.StartSeq,
		headSeq:    -1,
		logger:     logger,
	}
}

// Start begins the download process.
func (d *SegmentDownloader) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return fmt.Errorf("already running")
	}
	d.running = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
		if d.OnFinish != nil {
			d.OnFinish()
		}
	}()

	// Check for resume state
	resuming := false
	state, err := d.loadResume()
	if err == nil && state != nil {
		// Validate resume state: base URL must match current download
		if state.BaseURL != "" && state.BaseURL != d.opts.BaseURL {
			d.logger.Warn("[Downloader] Resume state URL mismatch, starting fresh",
				"saved", state.BaseURL, "current", d.opts.BaseURL)
			state = nil
		}
		// Validate resume state: file must exist and be at least as large as saved position
		if state != nil && state.BytesWritten > 0 {
			if info, statErr := os.Stat(d.opts.OutputFile); statErr != nil || info.Size() < state.BytesWritten {
				d.logger.Warn("[Downloader] Resume state invalid, starting fresh",
					"savedBytes", state.BytesWritten,
					"fileExists", statErr == nil)
				state = nil
			}
		}
		if state != nil {
			d.currentSeq = state.LastSeq + 1
			d.bytesWritten.Store(state.BytesWritten)
			resuming = true
		}
	}

	// DB-level fallback: no resume file but StartSeq > 0 means the database has
	// a last-downloaded sequence from a previous session. Use the output file's
	// current size as the byte position. This is a best-effort recovery for cases
	// where the resume file was lost (e.g., crash without graceful shutdown).
	if !resuming && d.currentSeq > 0 && !d.opts.IsHls {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			d.bytesWritten.Store(info.Size())
			d.currentSeq++ // StartSeq is the last downloaded; advance to the next
			resuming = true
			d.logger.Info("[Downloader] Resuming from database state (no resume file)",
				"seq", d.currentSeq, "fileSize", info.Size())
		} else {
			// Output file missing — can't resume, start fresh from segment 0
			d.logger.Info("[Downloader] No output file for resume, starting fresh")
			d.currentSeq = 0
		}
	}

	// Open output file
	flags := os.O_CREATE | os.O_WRONLY
	if resuming {
		flags |= os.O_APPEND
		// Truncate to known good size (only when we have a precise byte position from resume file)
		if state != nil && state.BytesWritten > 0 {
			if info, err := os.Stat(d.opts.OutputFile); err == nil && info.Size() > state.BytesWritten {
				d.logger.Info("[Downloader] Truncating file for resume",
					"from", info.Size(), "to", state.BytesWritten)
			}
			if err := os.Truncate(d.opts.OutputFile, state.BytesWritten); err != nil {
				d.logger.Warn("[Downloader] Failed to truncate for resume", "err", err)
			}
		}
	} else {
		flags |= os.O_TRUNC
	}

	d.outputFile, err = os.OpenFile(d.opts.OutputFile, flags, 0o644)
	if err != nil {
		return fmt.Errorf("open output file: %w", err)
	}
	defer d.outputFile.Close()

	// Download init segment first (only if not resuming and not HLS)
	// Non-fatal: TypeScript just warns and continues if init segment fails
	if !resuming && d.opts.InitURL != "" && !d.opts.IsHls {
		if err := d.downloadInitSegment(ctx); err != nil {
			d.logger.Warn("[Downloader] Failed to download init segment", "error", err)
		}
	}

	if d.OnStart != nil {
		d.OnStart(d.currentSeq, resuming)
	}

	// Run download loop
	if d.opts.IsDirectURL {
		return d.runDirectDownload(ctx)
	}
	if d.opts.IsHls {
		return d.runHlsLoop(ctx)
	}
	return d.runDashLoop(ctx)
}

// Cancel cancels the download.
func (d *SegmentDownloader) Cancel() {
	d.cancelled.Store(true)
}

// LastSeq returns the last successfully downloaded sequence number.
func (d *SegmentDownloader) LastSeq() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.currentSeq - 1
}

// BytesWritten returns total bytes written (lock-free).
func (d *SegmentDownloader) BytesWritten() int64 {
	return d.bytesWritten.Load()
}

func (d *SegmentDownloader) isCancelled() bool {
	return d.cancelled.Load()
}

// cancelErr returns context.Canceled when the user-initiated cancel flag is set
// but the context hasn't been cancelled yet. This ensures callers always get a
// non-nil error when the download was cancelled.
func (d *SegmentDownloader) cancelErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.isCancelled() {
		return context.Canceled
	}
	return nil
}

// setCommonHeaders applies User-Agent and Cookie headers to a request.
func (d *SegmentDownloader) setCommonHeaders(req *http.Request, ua string) {
	req.Header.Set("User-Agent", ua)
	if d.opts.CookieHeader != "" {
		req.Header.Set("Cookie", d.opts.CookieHeader)
	}
}

// runDirectDownload downloads a complete file from a direct URL (for VODs).
// Uses 5MB chunked Range requests with per-chunk retry and percentage progress (matches TS).
// Falls back to streaming download if the server doesn't support Range requests.
func (d *SegmentDownloader) runDirectDownload(ctx context.Context) error {
	// Probe total file size via Range: bytes=0-0
	totalSize := d.probeFileSize(ctx)

	if totalSize <= 0 {
		// Server doesn't support Range requests — fall back to streaming download
		return d.runDirectDownloadFallback(ctx)
	}

	// Chunked download with 5MB Range requests
	var offset int64
	lastProgressTime := time.Time{}

	for offset < totalSize {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		end := offset + DownloadChunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}

		data, statusCode, err := d.fetchChunkWithRetry(ctx, offset, end)
		if err != nil {
			if statusCode == http.StatusRequestedRangeNotSatisfiable {
				break // Past end of file
			}
			return fmt.Errorf("chunk download failed: %w", err)
		}

		if len(data) == 0 {
			break
		}

		n, err := d.outputFile.Write(data)
		if err != nil {
			return fmt.Errorf("write chunk: %w", err)
		}
		offset += int64(n)
		d.bytesWritten.Store(offset)

		// Throttled progress emission
		now := time.Now()
		if d.OnProgress != nil && (now.Sub(lastProgressTime) >= ProgressThrottle || offset >= totalSize) {
			lastProgressTime = now
			pct := float64(offset) / float64(totalSize) * 100
			d.OnProgress(DownloadProgress{
				Bytes:      offset,
				TotalBytes: totalSize,
				Percent:    pct,
			})
		}
	}

	// Final 100% progress callback
	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Bytes:      d.bytesWritten.Load(),
			TotalBytes: totalSize,
			Percent:    100,
		})
	}

	return nil
}

// probeFileSize discovers the total file size using a Range: bytes=0-0 request.
// Returns 0 if the server doesn't support Range requests or the size is unknown.
func (d *SegmentDownloader) probeFileSize(ctx context.Context) int64 {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return 0
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0 // Server doesn't support range requests
	}

	// Parse Content-Range: bytes 0-0/TOTAL
	contentRange := resp.Header.Get("Content-Range")
	if idx := strings.LastIndex(contentRange, "/"); idx >= 0 {
		sizeStr := contentRange[idx+1:]
		if sizeStr != "*" {
			size, _ := strconv.ParseInt(sizeStr, 10, 64)
			return size
		}
	}

	return 0
}

// fetchChunkWithRetry downloads a byte range with exponential backoff retry.
func (d *SegmentDownloader) fetchChunkWithRetry(ctx context.Context, start, end int64) ([]byte, int, error) {
	for attempt := 0; attempt < MaxChunkRetries; attempt++ {
		if d.isCancelled() || ctx.Err() != nil {
			return nil, 0, d.cancelErr(ctx)
		}

		data, status, err := d.fetchChunk(ctx, start, end)
		if err == nil {
			return data, status, nil
		}

		if status == http.StatusRequestedRangeNotSatisfiable {
			return nil, status, err
		}

		// Retry on 5xx or network errors with exponential backoff (capped at 60s)
		if status >= 500 || status == 0 {
			delay := time.Duration(1<<uint(attempt)) * time.Second
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
			sleepCtx(ctx, delay)
			continue
		}

		return nil, status, err
	}

	return nil, 0, fmt.Errorf("chunk download failed after %d retries", MaxChunkRetries)
}

// fetchChunk downloads a single byte range from the direct URL.
func (d *SegmentDownloader) fetchChunk(ctx context.Context, start, end int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaAndroid)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return nil, resp.StatusCode, fmt.Errorf("range not satisfiable")
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored Range header — read entire response
		data, err := io.ReadAll(resp.Body)
		return data, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// runDirectDownloadFallback is the streaming fallback when Range requests are not supported.
func (d *SegmentDownloader) runDirectDownloadFallback(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.opts.BaseURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	d.setCommonHeaders(req, uaAndroid)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read partial body for diagnostics
		bodySnippet := make([]byte, 1024)
		n, _ := resp.Body.Read(bodySnippet)
		d.logger.Debug("[Downloader] direct URL failed",
			"status", resp.StatusCode,
			"url_prefix", truncateURL(d.opts.BaseURL, 120),
			"body_snippet", string(bodySnippet[:n]),
		)
		return fmt.Errorf("HTTP %d downloading direct URL", resp.StatusCode)
	}

	buf := make([]byte, 64*1024) // 64KB buffer
	var lastProgressTime time.Time
	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, writeErr := d.outputFile.Write(buf[:n])
			if writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			d.bytesWritten.Add(int64(written))

			if d.OnProgress != nil && time.Since(lastProgressTime) >= ProgressThrottle {
				lastProgressTime = time.Now()
				d.OnProgress(DownloadProgress{
					Bytes: d.bytesWritten.Load(),
				})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}

	return nil
}

// truncateURL returns the first maxLen characters of a URL for logging.
func truncateURL(u string, maxLen int) string {
	if len(u) <= maxLen {
		return u
	}
	return u[:maxLen] + "..."
}

// runDashLoop is the main DASH download loop.
func (d *SegmentDownloader) runDashLoop(ctx context.Context) error {
	// Save resume state on exit; only clear on clean stream completion.
	// On shutdown (context cancel) or user cancel, keep the resume file
	// so the download can continue where it left off on restart.
	defer func() {
		// Snapshot fields under lock so we don't race with LastSeq() callers
		d.mu.Lock()
		seq := d.currentSeq
		head := d.headSeq
		ended := d.streamEnded
		d.mu.Unlock()

		// Emit final progress so the UI reflects the definitive last state
		if d.OnProgress != nil && seq > 0 {
			d.OnProgress(DownloadProgress{
				Seq:     seq - 1,
				Bytes:   d.bytesWritten.Load(),
				HeadSeq: head,
			})
		}
		d.saveResume()
		if ended {
			d.ClearResume()
		}
	}()

	consecutiveGoneErrors := 0
	hasStartedDownloading := false
	segsSinceResume := 0
	d.lastSegTime = time.Now() // Initialize to avoid premature NoSegmentTimeout

	// Same-segment retry tracking with exponential backoff (matches TS handleFailedSegmentDownload)
	sameSegRetries := 0
	lastRetrySeq := -1
	sameHeadRetryDelay := 0
	lastConfirmedHead := -1

	delayCap := d.opts.RetryDelayCap
	if delayCap <= 0 {
		delayCap = DefaultRetryDelayCap
	}
	liveCheckThreshold := d.opts.LiveCheckRetries
	if liveCheckThreshold <= 0 {
		liveCheckThreshold = 16
	}

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Check end sequence
		if d.opts.EndSeq >= 0 && d.currentSeq > d.opts.EndSeq {
			return nil
		}

		// Probe head sequence (use dedicated timer, not lastSegTime — matches TS lastHeadProbeTime)
		if d.headSeq < 0 || time.Since(d.lastHeadProbeTime) > HeadProbeInterval {
			if headSeq, err := d.probeHeadSequence(ctx); err == nil {
				d.headSeq = headSeq
			}
			d.lastHeadProbeTime = time.Now()
		}

		// Parallel catch-up if far behind
		if d.headSeq > 0 {
			segsBehind := d.headSeq - d.currentSeq
			if segsBehind >= CatchupThreshold {
				nextSeq, err := d.runParallelCatchUp(ctx)
				if err != nil {
					return err
				}
				d.currentSeq = nextSeq
				hasStartedDownloading = true
				// Re-probe head after catch-up (matches TS behavior)
				if headSeq, err := d.probeHeadSequence(ctx); err == nil {
					d.headSeq = headSeq
				}
				d.lastHeadProbeTime = time.Now()
				// Only re-enter loop if catch-up closed the gap (TS: returns false if stillFarBehind)
				stillFarBehind := d.headSeq > 0 && (d.headSeq-d.currentSeq) >= CatchupThreshold
				if !stillFarBehind {
					continue
				}
				// Still far behind — fall through to sequential download to avoid infinite catch-up loop
			}
		}

		// Download single segment
		segURL := d.buildSegmentURL(d.currentSeq)
		data, statusCode, err := d.fetchSegment(ctx, segURL)

		if err != nil || statusCode >= 400 {
			if statusCode == 403 || statusCode == 410 {
				// Segment gone/expired — matches TypeScript handleGoneErrors logic
				consecutiveGoneErrors++

				if hasStartedDownloading && consecutiveGoneErrors > 10 {
					// Check if stream is actually ended, or if our format just disappeared
					if d.opts.CheckStreamStatus != nil {
						ended, checkErr := d.opts.CheckStreamStatus(ctx)
						if checkErr != nil {
							d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
						} else if !ended {
							return ErrQualityLost
						}
					}
					d.streamEnded = true
					return nil // Stream ended
				}
				if !hasStartedDownloading && consecutiveGoneErrors <= 20 {
					d.currentSeq++
					sleepCtx(ctx, 100*time.Millisecond)
					continue
				}
				if !hasStartedDownloading && consecutiveGoneErrors > 20 {
					return nil // Failed to find valid starting segment
				}
				// Single GONE while downloading — small delay before retry (matches TS)
				sleepCtx(ctx, 500*time.Millisecond)
				continue
			}

			if statusCode == 429 {
				// Rate limited — use longer exponential backoff before retrying
				consecutiveGoneErrors = 0
				sameHeadRetryDelay++
				if sameHeadRetryDelay > delayCap {
					sameHeadRetryDelay = delayCap
				}
				backoff := time.Duration(sameHeadRetryDelay*2) * time.Second
				d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq, "delay", backoff)
				sleepCtx(ctx, backoff)
				continue
			}

			if statusCode >= 400 {
				// HTTP error (404, 5xx, etc.) — backoff with status checks
				if d.currentSeq == lastRetrySeq {
					sameSegRetries++
				} else {
					sameSegRetries = 1
					lastRetrySeq = d.currentSeq
				}

				// Re-probe head
				if headSeq, probeErr := d.probeHeadSequence(ctx); probeErr == nil {
					d.headSeq = headSeq
				}

				behindHead := d.headSeq > 0 && d.currentSeq < d.headSeq
				stuckOnSegment := sameSegRetries >= MaxSegmentRetries

				if behindHead && !stuckOnSegment {
					// Transient failure while behind head — retry with small delay
					sleepCtx(ctx, 1*time.Second)
					continue
				}

				// At/past live edge or stuck — backoff with status checks

				// Reset backoff if head moved
				if d.headSeq > 0 && d.headSeq != lastConfirmedHead {
					lastConfirmedHead = d.headSeq
					sameHeadRetryDelay = 0
				}

				sameHeadRetryDelay++
				if sameHeadRetryDelay > delayCap {
					sameHeadRetryDelay = delayCap
				}

				// Check stream status at threshold
				if sameHeadRetryDelay == liveCheckThreshold && d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						return nil
					}
				}

				// Check status on every probe at cap
				if sameHeadRetryDelay >= delayCap && d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						return nil
					}
					// Stream still live but we can't get segments — format may have changed
					if hasStartedDownloading {
						return ErrQualityLost
					}
				}

				// Also check no-segment timeout
				if time.Since(d.lastSegTime) > NoSegmentTimeout {
					if d.opts.CheckStreamStatus != nil && hasStartedDownloading {
						ended, checkErr := d.opts.CheckStreamStatus(ctx)
						if checkErr != nil {
							d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
						} else if !ended {
							return ErrQualityLost
						}
					}
					return nil
				}

				sleepCtx(ctx, time.Duration(sameHeadRetryDelay)*time.Second)
				continue
			}

			// Generic non-HTTP error (timeout, network, etc.) — simple 2s retry (matches TS)
			consecutiveGoneErrors = 0
			sleepCtx(ctx, 2*time.Second)
			continue
		}

		// Write segment
		n, err := d.outputFile.Write(data)
		if err != nil {
			return fmt.Errorf("write segment %d: %w", d.currentSeq, err)
		}
		d.bytesWritten.Add(int64(n))
		d.lastSegTime = time.Now()
		consecutiveGoneErrors = 0
		sameHeadRetryDelay = 0
		sameSegRetries = 0
		hasStartedDownloading = true

		// Emit progress
		if d.OnProgress != nil {
			d.OnProgress(DownloadProgress{
				Seq:     d.currentSeq,
				Bytes:   d.bytesWritten.Load(),
				HeadSeq: d.headSeq,
			})
		}

		d.currentSeq++
		segsSinceResume++

		// Save resume state periodically
		if segsSinceResume >= ResumeSeqInterval {
			d.saveResume()
			segsSinceResume = 0
		}
	}
}

// runHlsLoop is the main HLS download loop.
func (d *SegmentDownloader) runHlsLoop(ctx context.Context) error {
	// Save resume state on exit so interrupted downloads can continue on restart.
	// Only clear the resume file when the stream ends naturally.
	defer func() {
		d.saveResume()
		d.mu.Lock()
		ended := d.streamEnded
		d.mu.Unlock()
		if ended {
			d.ClearResume()
		}
	}()

	staleCount := 0
	consecutiveErrors := 0

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Fetch playlist
		data, plStatus, err := d.fetchSegment(ctx, d.opts.BaseURL)
		if err != nil {
			// 404/410 on playlist fetch — variant may have been removed
			if plStatus == 404 || plStatus == 410 {
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				d.streamEnded = true
				return nil
			}
			consecutiveErrors++
			if consecutiveErrors > 5 {
				// Before giving up, check if stream is still live (quality may have changed)
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				return fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", consecutiveErrors, err)
			}
			sleepCtx(ctx, 5*time.Second)
			continue
		}

		result := ParseHls(string(data), d.opts.BaseURL)
		if result == nil || result.Playlist == nil {
			consecutiveErrors++
			if consecutiveErrors > 5 {
				return fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", consecutiveErrors)
			}
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		pl := result.Playlist

		// Initialize currentSeq if needed
		if d.currentSeq < 0 {
			d.currentSeq = pl.MediaSequence
		}

		// Handle gap: if currentSeq < mediaSequence, segments expired from CDN
		if d.currentSeq < pl.MediaSequence {
			if d.OnGap != nil {
				d.OnGap(DownloadGap{From: d.currentSeq, To: pl.MediaSequence - 1})
			}
			d.currentSeq = pl.MediaSequence
		}

		// Identify new segments (only those >= currentSeq)
		var newSegments []HlsSegment
		for i, seg := range pl.Segments {
			segSeq := pl.MediaSequence + i
			if segSeq >= d.currentSeq {
				newSegments = append(newSegments, seg)
			}
		}

		// VOD: parallel download only the filtered segments (not already downloaded)
		if pl.EndList && len(newSegments) > 0 {
			filteredPl := &HlsPlaylist{
				Segments:      newSegments,
				MediaSequence: d.currentSeq,
				EndList:       pl.EndList,
				TargetDuration: pl.TargetDuration,
			}
			return d.runHlsVodParallel(ctx, filteredPl)
		}

		// Live: download available segments
		segFailed := false
		for _, seg := range newSegments {
			if d.isCancelled() || ctx.Err() != nil {
				return d.cancelErr(ctx)
			}

			segData, _, err := d.fetchSegment(ctx, seg.URL)
			if err != nil {
				// Don't skip — break to re-fetch playlist and retry.
				// If CDN purged it, gap detection handles it next iteration.
				d.logger.Debug("[Downloader] HLS segment failed, will retry after playlist refresh",
					"seq", d.currentSeq, "error", err)
				sleepCtx(ctx, 2*time.Second)
				segFailed = true
				break
			}
			n, writeErr := d.outputFile.Write(segData)
			if writeErr != nil {
				return fmt.Errorf("write HLS segment %d: %w", d.currentSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq++
			d.lastSegTime = time.Now()

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   d.currentSeq,
					Bytes: d.bytesWritten.Load(),
				})
			}
		}

		if segFailed {
			continue // Retry from playlist refresh
		}

		// Stale detection: no new segments available
		if len(newSegments) == 0 {
			staleCount++
			if staleCount >= 5 && d.opts.CheckStreamStatus != nil {
				ended, _ := d.opts.CheckStreamStatus(ctx)
				if ended {
					d.streamEnded = true
					return nil
				}
			}
		} else {
			staleCount = 0
		}

		// Reset consecutive errors on successful iteration
		consecutiveErrors = 0

		d.saveResume()

		// Check if stream ended (EXT-X-ENDLIST present)
		if pl.EndList {
			return nil
		}

		// Wait before next refresh
		targetDur := pl.TargetDuration
		if targetDur <= 0 {
			targetDur = 2.0
		}
		sleepCtx(ctx, time.Duration(targetDur*float64(time.Second)))
	}
}

// runHlsVodParallel downloads all VOD HLS segments in parallel with bounded concurrency.
// Uses a worker pool pattern: ParallelDownloads workers pull from a work channel,
// avoiding N goroutines sitting in memory for large VODs.
func (d *SegmentDownloader) runHlsVodParallel(ctx context.Context, pl *HlsPlaylist) error {
	totalSegs := len(pl.Segments)
	if totalSegs == 0 {
		return nil
	}

	type segWork struct {
		idx    int
		segURL string
	}
	type segResult struct {
		idx  int
		data []byte
	}

	work := make(chan segWork, ParallelDownloads)
	results := make(chan segResult, ParallelDownloads*3)
	var wg sync.WaitGroup

	// Spawn fixed worker pool
	for w := 0; w < ParallelDownloads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil && d.logger != nil {
					d.logger.Error("VOD parallel download worker panic", "panic", r)
				}
			}()
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				if data := d.fetchSegmentWithRetry(ctx, item.segURL); data != nil {
					results <- segResult{idx: item.idx, data: data}
				}
			}
		}()
	}

	// Feed work to workers
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("VOD feeder goroutine panic", "panic", r)
			}
		}()
		for i, seg := range pl.Segments {
			if d.isCancelled() || ctx.Err() != nil {
				break
			}
			work <- segWork{idx: i, segURL: seg.URL}
		}
		close(work)
	}()

	// Close results when all workers done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Stream write: buffer out-of-order segments, write in order as they arrive.
	// Buffer size is bounded by the number of in-flight workers (ParallelDownloads);
	// segments are flushed in order so the map typically holds only a few entries.
	buffer := make(map[int][]byte)
	nextIdx := 0

	for r := range results {
		buffer[r.idx] = r.data

		// Flush consecutive segments from buffer to disk
		for {
			data, ok := buffer[nextIdx]
			if !ok {
				break
			}
			delete(buffer, nextIdx) // Free memory immediately

			n, err := d.outputFile.Write(data)
			if err != nil {
				return fmt.Errorf("write HLS VOD segment %d: %w", nextIdx, err)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq++
			nextIdx++

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   d.currentSeq,
					Bytes: d.bytesWritten.Load(),
					Total: totalSegs,
				})
			}

			// Save resume state periodically (matching TS: every 50 segments)
			if d.currentSeq%50 == 0 {
				d.saveResume()
			}
		}
	}

	// Flush remaining buffered segments + detect gaps
	gapStart := -1
	for nextIdx < totalSegs {
		if data, ok := buffer[nextIdx]; ok {
			// Close any open gap
			if gapStart >= 0 && d.OnGap != nil {
				d.OnGap(DownloadGap{From: gapStart, To: nextIdx})
				gapStart = -1
			}
			n, writeErr := d.outputFile.Write(data)
			if writeErr != nil {
				return fmt.Errorf("write error during HLS VOD gap flush (segment %d): %w", nextIdx, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq++
			delete(buffer, nextIdx)
		} else {
			if gapStart < 0 {
				gapStart = nextIdx
			}
		}
		nextIdx++
	}
	// Close final gap
	if gapStart >= 0 && d.OnGap != nil {
		d.OnGap(DownloadGap{From: gapStart, To: totalSegs})
	}

	d.saveResume()
	return nil
}

// runParallelCatchUp downloads segments in parallel to catch up to the live edge.
func (d *SegmentDownloader) runParallelCatchUp(ctx context.Context) (int, error) {
	targetSeq := d.headSeq - 30 // Stay 30 segments behind live edge
	if targetSeq < d.currentSeq+1 {
		targetSeq = d.currentSeq + 1 // At least catch up 1 segment
	}
	// Respect endSeq limit (for timestamp-based trimming)
	if d.opts.EndSeq >= 0 && targetSeq > d.opts.EndSeq+1 {
		targetSeq = d.opts.EndSeq + 1
	}
	if targetSeq <= d.currentSeq {
		return d.currentSeq, nil
	}

	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Seq:        d.currentSeq,
			Bytes:      d.bytesWritten.Load(),
			HeadSeq:    d.headSeq,
			CatchingUp: true,
		})
	}

	type segWork struct {
		seq int
	}
	type segResult struct {
		seq  int
		data []byte
	}

	bufferCap := ParallelDownloads * 3
	work := make(chan segWork, ParallelDownloads)
	results := make(chan segResult, bufferCap)
	var wg sync.WaitGroup

	// Spawn fixed worker pool
	for w := 0; w < ParallelDownloads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil && d.logger != nil {
					d.logger.Error("catch-up parallel download worker panic", "panic", r)
				}
			}()
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				segURL := d.buildSegmentURL(item.seq)
				if data := d.fetchSegmentWithRetry(ctx, segURL); data != nil {
					results <- segResult{seq: item.seq, data: data}
				}
			}
		}()
	}

	// Feed work to workers
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("catch-up feeder goroutine panic", "panic", r)
			}
		}()
		for seq := d.currentSeq; seq <= targetSeq; seq++ {
			if d.isCancelled() || ctx.Err() != nil {
				break
			}
			work <- segWork{seq: seq}
		}
		close(work)
	}()

	// Close results when all workers complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Stream write: buffer out-of-order segments, write in order as they arrive
	buffer := make(map[int][]byte)
	nextSeq := d.currentSeq
	segsSinceResume := 0
	for r := range results {
		buffer[r.seq] = r.data

		// Flush consecutive segments from buffer to disk
		for {
			data, ok := buffer[nextSeq]
			if !ok {
				break
			}
			delete(buffer, nextSeq) // Free memory immediately after write

			n, err := d.outputFile.Write(data)
			if err != nil {
				return nextSeq, fmt.Errorf("write segment %d: %w", nextSeq, err)
			}
			d.bytesWritten.Add(int64(n))
			d.lastSegTime = time.Now()

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:        nextSeq,
					Bytes:      d.bytesWritten.Load(),
					HeadSeq:    d.headSeq,
					CatchingUp: true,
				})
			}

			nextSeq++
			segsSinceResume++
			if segsSinceResume >= ResumeCatchupInterval {
				d.currentSeq = nextSeq
				d.saveResume()
				segsSinceResume = 0
			}
		}
	}

	// Final progress emission — CatchingUp: false signals catch-up is complete
	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Seq:        nextSeq - 1,
			Bytes:      d.bytesWritten.Load(),
			HeadSeq:    d.headSeq,
			CatchingUp: false,
		})
	}

	// Handle any remaining gaps (use range-based gap detection)
	catchupGapStart := -1
	for nextSeq <= targetSeq {
		if _, ok := buffer[nextSeq]; ok {
			// Close any open gap
			if catchupGapStart >= 0 && d.OnGap != nil {
				d.OnGap(DownloadGap{From: catchupGapStart, To: nextSeq})
				catchupGapStart = -1
			}
			n, writeErr := d.outputFile.Write(buffer[nextSeq])
			if writeErr != nil {
				return 0, fmt.Errorf("write error during catch-up gap flush (segment %d): %w", nextSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			delete(buffer, nextSeq)
		} else {
			if catchupGapStart < 0 {
				catchupGapStart = nextSeq
			}
		}
		nextSeq++
	}
	// Close final gap range
	if catchupGapStart >= 0 && d.OnGap != nil {
		d.OnGap(DownloadGap{From: catchupGapStart, To: nextSeq})
	}

	return nextSeq, nil
}

// probeHeadSequence discovers the current live head segment using a high sequence GET probe.
// YouTube returns the X-Head-Seqnum header on GET requests to a non-existent segment.
func (d *SegmentDownloader) probeHeadSequence(ctx context.Context) (int, error) {
	probeURL := d.buildSegmentURL(999999999)
	// Apply GVS PO token
	if d.opts.PoToken != "" {
		sep := "?"
		if strings.Contains(probeURL, "?") {
			sep = "&"
		}
		probeURL = probeURL + sep + "pot=" + d.opts.PoToken
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return -1, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	headSeqStr := resp.Header.Get("X-Head-Seqnum")
	if headSeqStr == "" {
		return -1, fmt.Errorf("no X-Head-Seqnum header")
	}

	headSeq, err := strconv.Atoi(headSeqStr)
	if err != nil {
		return -1, fmt.Errorf("parse X-Head-Seqnum: %w", err)
	}

	return headSeq, nil
}

func (d *SegmentDownloader) buildSegmentURL(seq int) string {
	if strings.Contains(d.opts.BaseURL, "$Number$") {
		return SegmentURL(d.opts.BaseURL, seq)
	}
	// Append /sq/{seq} for YouTube-style URLs
	base := strings.TrimRight(d.opts.BaseURL, "/")
	return fmt.Sprintf("%s/sq/%d", base, seq)
}

func (d *SegmentDownloader) fetchSegment(ctx context.Context, segURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, SegmentTimeout)
	defer cancel()

	// Apply GVS PO token to segment URL (query mode: ?pot=token)
	if d.opts.PoToken != "" {
		sep := "?"
		if strings.Contains(segURL, "?") {
			sep = "&"
		}
		segURL = segURL + sep + "pot=" + d.opts.PoToken
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, 0, err
	}
	d.setCommonHeaders(req, uaWeb)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return data, resp.StatusCode, nil
}

// fetchSegmentWithRetry attempts to fetch a segment with retries and exponential backoff.
// Returns the data on success, or nil if all retries failed or the segment is permanently gone.
func (d *SegmentDownloader) fetchSegmentWithRetry(ctx context.Context, segURL string) []byte {
	for attempt := 0; attempt < MaxSegmentRetries; attempt++ {
		if d.isCancelled() || ctx.Err() != nil {
			return nil
		}
		data, status, err := d.fetchSegment(ctx, segURL)
		if err == nil && status < 400 {
			return data
		}
		if status == 403 || status == 410 {
			return nil // Segment gone permanently
		}
		sleepCtx(ctx, time.Duration(5*(attempt+1))*time.Second)
	}
	return nil
}

func (d *SegmentDownloader) downloadInitSegment(ctx context.Context) error {
	data, status, err := d.fetchSegment(ctx, d.opts.InitURL)
	if err != nil || status >= 400 {
		return fmt.Errorf("init segment: status=%d err=%v", status, err)
	}
	n, err := d.outputFile.Write(data)
	if err != nil {
		return err
	}
	d.bytesWritten.Add(int64(n))
	return nil
}

func (d *SegmentDownloader) loadResume() (*ResumeState, error) {
	data, err := os.ReadFile(d.opts.ResumeFile)
	if err != nil {
		return nil, err
	}
	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (d *SegmentDownloader) saveResume() {
	d.mu.Lock()
	seq := d.currentSeq
	d.mu.Unlock()
	if seq <= 0 {
		return // Nothing downloaded yet (matching TS guard)
	}
	state := ResumeState{
		LastSeq:      seq - 1,
		BytesWritten: d.bytesWritten.Load(),
		Timestamp:    time.Now().Unix(),
		BaseURL:      d.opts.BaseURL,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmpFile := d.opts.ResumeFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		d.logger.Warn("[Downloader] Failed to write resume file", "file", tmpFile, "error", err)
		return
	}
	if err := os.Rename(tmpFile, d.opts.ResumeFile); err != nil {
		d.logger.Warn("[Downloader] Failed to rename resume file", "from", tmpFile, "to", d.opts.ResumeFile, "error", err)
	}
}

// ClearResume removes the resume state file.
func (d *SegmentDownloader) ClearResume() {
	os.Remove(d.opts.ResumeFile)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
