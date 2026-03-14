package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQualityLost signals that the stream is still live but the selected
// quality variant/format has become unavailable (e.g. transcode removed).
var ErrQualityLost = errors.New("stream quality became unavailable")

const (
	CatchupThreshold    = 10
	MaxSegmentRetries   = 5
	ParallelDownloads   = 6  // Bounded parallel downloads during catch-up
	DefaultRetryDelayCap = 60 // seconds
	HeadProbeInterval   = 5 * time.Second
	SegmentTimeout      = 30 * time.Second
	NoSegmentTimeout    = 10 * time.Minute
	ResumeSeqInterval   = 50 // Save resume state every N sequential segments
	ResumeCatchupInterval = 10 // Save resume state every N catch-up segments
	DownloadChunkSize   = 5 * 1024 * 1024       // 5MB chunks for VOD downloads (matches TS)
	MaxChunkRetries     = 3                      // Per-chunk retry limit
	ProgressThrottle    = 500 * time.Millisecond // Throttle VOD progress emission

	// stayBehindSegments is the number of segments to stay behind the live edge
	// during parallel catch-up, avoiding download of in-flight segments.
	stayBehindSegments = 30

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

// SegmentDownloader downloads DASH or HLS segments sequentially/in parallel.
type SegmentDownloader struct {
	opts              DownloaderOptions
	mu                sync.Mutex
	running           bool
	cancelled         atomic.Bool
	streamEnded       atomic.Bool
	outputFile        *os.File
	bytesWritten      atomic.Int64
	currentSeq        atomic.Int64
	headSeq           atomic.Int64
	lastSegTime       time.Time
	lastHeadProbeTime  time.Time
	logger             DownloaderLogger
	cipherFailureFired bool

	// Callbacks
	OnStart          func(seq int, resuming bool)
	OnProgress       func(p DownloadProgress)
	OnGap            func(g DownloadGap)
	OnFinish         func()
	OnCipherFailure  func() // Called once on first 403 before any bytes written (likely cipher issue)
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

	d := &SegmentDownloader{
		opts:   opts,
		logger: logger,
	}
	d.currentSeq.Store(int64(opts.StartSeq))
	d.headSeq.Store(-1)
	return d
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
			d.currentSeq.Store(int64(state.LastSeq + 1))
			d.bytesWritten.Store(state.BytesWritten)
			resuming = true
		}
	}

	// DB-level fallback: no resume file but StartSeq > 0 means the database has
	// a last-downloaded sequence from a previous session. Use the output file's
	// current size as the byte position. This is a best-effort recovery for cases
	// where the resume file was lost (e.g., crash without graceful shutdown).
	if !resuming && d.currentSeq.Load() > 0 && !d.opts.IsHls {
		if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > 0 {
			d.bytesWritten.Store(info.Size())
			d.currentSeq.Add(1) // StartSeq is the last downloaded; advance to the next
			resuming = true
			d.logger.Info("[Downloader] Resuming from database state (no resume file)",
				"seq", d.currentSeq.Load(), "fileSize", info.Size())
		} else {
			// Output file missing -- can't resume, start fresh from segment 0
			d.logger.Info("[Downloader] No output file for resume, starting fresh")
			d.currentSeq.Store(0)
		}
	}

	// Open output file
	flags := os.O_CREATE | os.O_WRONLY
	if resuming {
		flags |= os.O_APPEND
		// Truncate to known good size (only when we have a precise byte position from resume file)
		if state != nil && state.BytesWritten > 0 {
			if info, statErr := os.Stat(d.opts.OutputFile); statErr == nil && info.Size() > state.BytesWritten {
				d.logger.Info("[Downloader] Truncating file for resume",
					"from", info.Size(), "to", state.BytesWritten)
			}
			if truncErr := os.Truncate(d.opts.OutputFile, state.BytesWritten); truncErr != nil {
				d.logger.Warn("[Downloader] Failed to truncate for resume", "err", truncErr)
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
		if initErr := d.downloadInitSegment(ctx); initErr != nil {
			d.logger.Warn("[Downloader] Failed to download init segment", "error", initErr)
		}
	}

	if d.OnStart != nil {
		d.OnStart(int(d.currentSeq.Load()), resuming)
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
	return int(d.currentSeq.Load()) - 1
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

func (d *SegmentDownloader) buildSegmentURL(seq int) string {
	if strings.Contains(d.opts.BaseURL, "$Number$") {
		return SegmentURL(d.opts.BaseURL, seq)
	}
	// Append /sq/{seq} for YouTube-style URLs
	base := strings.TrimRight(d.opts.BaseURL, "/")
	return fmt.Sprintf("%s/sq/%d", base, seq)
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

// truncateURL returns the first maxLen characters of a URL for logging.
func truncateURL(u string, maxLen int) string {
	if len(u) <= maxLen {
		return u
	}
	return u[:maxLen] + "..."
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
