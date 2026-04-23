package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	dedupKeepSize                = 5000 // Keep last N IDs for dedup
	maxConsecErrorsLive          = 20
	maxConsecErrorsVod           = 5
	maxStaleContinuationAttempts = 30
	writeInterval                = 1 * time.Second // Flush batched messages to disk at most once per interval
)

// ChatDownloaderOptions configures a ChatDownloader.
type ChatDownloaderOptions struct {
	VideoID             string
	VideoTitle          string
	ChannelName         string
	OutputFile          string
	InitialContinuation string
	ApiKey              string
	VisitorData         string
	CookieHeader        string
	GenerateAuth        func() string // Returns SAPISIDHASH Authorization header for member-gated chat
	IsReplay            bool
	IsLiveOrUpcoming    bool
	StreamStartTime     string
	ResumeFile          string
}

// ChatDownloader downloads YouTube live chat messages.
type ChatDownloader struct {
	opts          ChatDownloaderOptions
	api           *ChatAPI
	mu            sync.Mutex
	running       bool
	cancelFlag    bool
	streamEnded   bool
	messages      []ChatMessage // Unwritten messages in memory
	messageCount  int
	seenIDs       map[string]struct{}
	seenOrder     []string // Insertion order for deterministic culling
	continuation  string
	streamStartMs int64
	flushedToDisk bool
	lastWriteAt   time.Time
	lastTimestamp  string
	cancelCtx     context.CancelFunc // for aborting sleep on stop/markStreamEnded
	done          chan struct{}       // closed when Start() completes; nil if never started

	OnStart    func(messageCount int, resuming bool)
	OnProgress func(p ChatProgress)
	OnFinish   func()
	OnError    func(err error)

	// Logger is an optional diagnostic sink for non-fatal, debug-level drift
	// signals (e.g. unexpected API field shapes). nil-safe — if not set,
	// debug diagnostics are silently dropped.
	Logger interface {
		Debug(msg string, args ...any)
	}
}

// logDebug routes a debug-level diagnostic through the optional Logger.
// No-op when Logger is nil.
func (cd *ChatDownloader) logDebug(msg string, args ...any) {
	if cd.Logger != nil {
		cd.Logger.Debug(msg, args...)
	}
}

// NewChatDownloader creates a new chat downloader.
func NewChatDownloader(opts ChatDownloaderOptions) *ChatDownloader {
	api := NewChatAPI(opts.ApiKey, opts.VisitorData, opts.CookieHeader)
	api.generateAuth = opts.GenerateAuth

	if opts.ResumeFile == "" {
		opts.ResumeFile = opts.OutputFile + ".resume.json"
	}

	var streamStartMs int64
	if opts.StreamStartTime != "" {
		if t, err := time.Parse(time.RFC3339, opts.StreamStartTime); err == nil {
			streamStartMs = t.UnixMilli()
		}
	}

	return &ChatDownloader{
		opts:          opts,
		api:           api,
		seenIDs:       make(map[string]struct{}),
		continuation:  opts.InitialContinuation,
		streamStartMs: streamStartMs,
	}
}

// Start begins the chat download process.
// If the downloader is already running (e.g., early chat handed to the orchestrator),
// Start blocks until the existing run completes or ctx is cancelled.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	if cd.running {
		done := cd.done
		cd.mu.Unlock()
		// Already running — wait for completion or context cancellation
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		return nil
	}
	cd.running = true
	cd.cancelFlag = false
	cd.streamEnded = false
	cd.done = make(chan struct{})
	// Propagate Logger to the API so parse-level drift diagnostics are captured.
	if cd.api != nil {
		cd.api.Logger = cd.Logger
	}
	cd.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			// Capture the stack at the point of panic so production crashes
			// are diagnosable from the OnError sink alone.
			stack := debug.Stack()
			if cd.OnError != nil {
				// Guard against a panicking OnError handler — we're already
				// in recovery, a second panic here would escape the goroutine.
				func() {
					defer func() {
						_ = recover()
					}()
					cd.OnError(fmt.Errorf("chat downloader panic: %v\n%s", r, stack))
				}()
			}
			cd.mu.Lock()
			cd.running = false
			cd.mu.Unlock()
		}
	}()

	defer func() {
		cd.mu.Lock()
		cd.running = false
		cd.cancelCtx = nil
		done := cd.done
		cd.done = nil
		cd.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	// Create a cancellable context for aborting sleep on stop/markStreamEnded
	ctx, cancel := context.WithCancel(ctx)
	cd.mu.Lock()
	cd.cancelCtx = cancel
	cd.mu.Unlock()
	defer cancel()

	// Load resume state
	resuming := false
	state, err := cd.loadResume()
	if err == nil && state != nil && state.VideoID == cd.opts.VideoID {
		cd.continuation = state.Continuation
		cd.messageCount = state.MessageCount
		cd.messages = nil // Start fresh — old messages already on disk
		if len(state.RecentIDs) > 0 {
			cd.seenIDs = make(map[string]struct{}, len(state.RecentIDs))
			cd.seenOrder = make([]string, 0, len(state.RecentIDs))
			for _, id := range state.RecentIDs {
				cd.seenIDs[id] = struct{}{}
				cd.seenOrder = append(cd.seenOrder, id)
			}
		}
		cd.flushedToDisk = cd.messageCount > 0
		resuming = true
	}

	if cd.OnStart != nil {
		cd.OnStart(cd.messageCount, resuming)
	}

	// Run chat loop
	cd.runChatLoop(ctx, resuming)

	// Write remaining messages
	if len(cd.messages) > 0 {
		cd.writeChatFile()
	}

	// Update header metadata if we flushed earlier
	if cd.flushedToDisk && cd.messageCount > 0 {
		cd.updateChatFileHeader()
	}

	// Clear resume state on clean completion (not cancelled).
	if !cd.wasCancelledOrShutdown(ctx) {
		cd.clearResume()
	}

	if cd.OnFinish != nil {
		cd.OnFinish()
	}

	return nil
}

// MarkStreamEnded signals that the stream has ended naturally.
// This exits the polling loop promptly, writes the final chat file,
// and clears resume state (successful completion).
// Distinct from Stop() which is for cancellation/shutdown.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.mu.Lock()
	cd.streamEnded = true
	if cd.cancelCtx != nil {
		cd.cancelCtx() // Wake up any sleeping poll
	}
	cd.mu.Unlock()
}

// Stop cancels the chat download (for shutdown/cancellation).
func (cd *ChatDownloader) Stop() {
	cd.mu.Lock()
	cd.running = false
	cd.cancelFlag = true
	if cd.cancelCtx != nil {
		cd.cancelCtx() // Wake up any sleeping poll
	}
	cd.mu.Unlock()
}

// MessageCount returns the total number of messages downloaded.
func (cd *ChatDownloader) MessageCount() int {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.messageCount
}

// IsRunning returns whether the downloader is currently running.
func (cd *ChatDownloader) IsRunning() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.running
}

// isStreamActive returns whether the stream is still live (not ended).
func (cd *ChatDownloader) isStreamActive() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.opts.IsLiveOrUpcoming && !cd.streamEnded
}

func (cd *ChatDownloader) shouldStop() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return !cd.running || cd.cancelFlag || cd.streamEnded
}

// wasCancelledOrShutdown returns true if the download was stopped by user
// cancellation or application shutdown. Context cancellation without Stop()
// is a shutdown race — the parent context was cancelled but Stop() hasn't
// been called yet. MarkStreamEnded sets streamEnded (clean completion) so
// we distinguish it from external context cancellation.
func (cd *ChatDownloader) wasCancelledOrShutdown(ctx context.Context) bool {
	cd.mu.Lock()
	cancelled := cd.cancelFlag
	ended := cd.streamEnded
	cd.mu.Unlock()
	if !cancelled && !ended && ctx.Err() != nil {
		return true
	}
	return cancelled
}

func (cd *ChatDownloader) runChatLoop(ctx context.Context, resuming bool) {
	consecutiveErrors := 0
	switchedToAllChat := resuming // Skip All Chat switch when resuming — continuation is already mid-stream

	for !cd.shouldStop() {
		if cd.continuation == "" {
			return
		}

		// Fetch chat
		var resp *ChatApiResponse
		var err error
		if cd.opts.IsReplay {
			resp, err = cd.api.FetchChatReplay(ctx, cd.continuation)
		} else {
			resp, err = cd.api.FetchLiveChat(ctx, cd.continuation)
		}

		if err != nil {
			// If context cancelled (stop/markStreamEnded), exit without counting as error
			if ctx.Err() != nil {
				break
			}

			consecutiveErrors++
			if cd.OnError != nil {
				cd.OnError(err)
			}

			// Higher tolerance for live streams (> not >=, matching TypeScript)
			maxErrors := maxConsecErrorsVod
			if cd.isStreamActive() {
				maxErrors = maxConsecErrorsLive
			}
			if consecutiveErrors > maxErrors {
				if cd.OnError != nil {
					cd.OnError(fmt.Errorf("too many consecutive chat API errors"))
				}
				break
			}

			// Exponential backoff (cap at 30s for VOD, 60s for live)
			maxBackoff := 30000
			if cd.isStreamActive() {
				maxBackoff = 60000
			}
			backoffMs := min(5000*consecutiveErrors, maxBackoff)
			cd.sleep(ctx, time.Duration(backoffMs)*time.Millisecond)
			continue
		}

		consecutiveErrors = 0

		// Switch from "Top Chat" (filtered) to "All Chat" (unfiltered) on the first response.
		// YouTube defaults to Top Chat which can aggressively filter messages to zero.
		// Only mark the switch as complete when we actually upgraded the continuation —
		// if the AllChatContinuation header wasn't present (e.g. early-preroll response),
		// leave switchedToAllChat == false so a subsequent poll can retry the upgrade.
		if !switchedToAllChat && resp.AllChatContinuation != "" {
			cd.continuation = resp.AllChatContinuation
			switchedToAllChat = true
			continue // Re-poll immediately with the unfiltered continuation
		}

		// Process messages
		newInBatch := 0
		for i := range resp.Messages {
			msg := &resp.Messages[i]

			// Calculate offsetMs if not already set from replay wrapper.
			// Pre-stream waiting-room chat can produce legitimate negative offsets,
			// so HasOffset is the sentinel rather than OffsetMs == 0.
			if !msg.HasOffset && cd.streamStartMs > 0 && msg.TimestampUsec != "" {
				usec, err := strconv.ParseInt(msg.TimestampUsec, 10, 64)
				if err != nil {
					cd.logDebug("chat: timestampUsec parse failed", "videoID", cd.opts.VideoID, "value", msg.TimestampUsec, "err", err)
				} else if usec > 0 {
					msg.OffsetMs = usec/1000 - cd.streamStartMs
					msg.HasOffset = true
				}
			}

			// Dedup by ID (skip empty IDs to avoid silent dedup of malformed messages)
			if msg.ID != "" {
				if _, seen := cd.seenIDs[msg.ID]; seen {
					continue
				}
			}

			if msg.ID != "" {
				cd.seenIDs[msg.ID] = struct{}{}
				cd.seenOrder = append(cd.seenOrder, msg.ID)
			}
			cd.messages = append(cd.messages, *msg)
			cd.messageCount++
			cd.lastTimestamp = msg.TimestampUsec
			newInBatch++
		}

		// Emit progress on every batch with new messages (not throttled)
		if newInBatch > 0 && cd.OnProgress != nil {
			lastTs := ""
			if len(resp.Messages) > 0 {
				lastTs = resp.Messages[len(resp.Messages)-1].TimestampText
			}
			cd.OnProgress(ChatProgress{
				MessageCount: cd.messageCount,
				LastTimestamp: lastTs,
			})
		}

		// Write to disk at most once per writeInterval
		if newInBatch > 0 {
			now := time.Now()
			if cd.lastWriteAt.IsZero() || now.Sub(cd.lastWriteAt) >= writeInterval {
				cd.writeChatFile()
				cd.lastWriteAt = now

				// Update header to keep messageCount accurate after incremental flushes
				cd.updateChatFileHeader()

				cd.saveResume()
			}
		}

		// Bound seenIDs on every successful fetch — pinned announcements and
		// already-seen IDs can accumulate via newly-seen batches even when
		// newInBatch == 0 (resp replayed same IDs), so the cull check must
		// live outside the write-interval gate to prevent unbounded growth.
		if len(cd.seenIDs) > dedupKeepSize {
			cd.cullDedup()
		}

		// Check if chat has ended
		if resp.IsComplete || resp.NextContinuation == "" {
			if cd.isStreamActive() {
				// Stream is still live — continuation went stale.
				// Fetch a fresh continuation token from the watch page.
				fresh, _, freshErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
				if freshErr == nil && fresh != "" {
					cd.continuation = fresh
					switchedToAllChat = false // Fresh token defaults to Top Chat — re-trigger switch
					continue
				}

				// Initial fresh-continuation fetch failed — retry with exponential
				// backoff. Sleep *between* attempts (after a failure), not before
				// the first retry. maxStaleContinuationAttempts counts the
				// failed attempts so far (the initial fetch above is attempt #1).
				contRetryDelay := 10 * time.Second
				contRetries := 1 // the initial failed call above
				gotFresh := false

				for !cd.shouldStop() && contRetries < maxStaleContinuationAttempts {
					cd.sleep(ctx, contRetryDelay)
					if cd.shouldStop() {
						break
					}
					retry, _, retryErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
					if retryErr == nil && retry != "" {
						cd.continuation = retry
						switchedToAllChat = false // Fresh token defaults to Top Chat — re-trigger switch
						gotFresh = true
						break
					}
					contRetries++
					// Exponential backoff: 10s, 20s, 40s, 80s, cap at 5min
					contRetryDelay = min(contRetryDelay*2, 5*time.Minute)
				}
				if !gotFresh {
					break
				}
				continue
			}
			// VOD/replay: chat is complete
			break
		}

		// Update continuation for next request
		cd.continuation = resp.NextContinuation

		// Wait before next poll (matches TypeScript: timeoutMs || (isReplay ? 0 : 5000))
		// TS uses || (not ??) so timeoutMs=0 also falls back to the default
		waitMs := resp.TimeoutMs
		if waitMs <= 0 {
			// Not set or zero from API — use replay=0, live=5000
			if cd.opts.IsReplay {
				waitMs = 0
			} else {
				waitMs = 5000
			}
		}
		if waitMs > 0 {
			cd.sleep(ctx, time.Duration(waitMs)*time.Millisecond)
		}
	}

	// Save resume state when cancelled or context cancelled (shutdown race).
	// We save if there are unflushed messages OR any disk-flushed state exists
	// (dedup IDs, continuation token) so a resume can pick up where we left off.
	if cd.wasCancelledOrShutdown(ctx) && (len(cd.messages) > 0 || cd.flushedToDisk) {
		cd.saveResume()
	}
}

func (cd *ChatDownloader) cullDedup() {
	// Cull seenIDs using insertion order (seenOrder slice) to keep the most
	// recent entries, matching TypeScript's Set insertion-order behavior.
	if len(cd.seenOrder) <= dedupKeepSize {
		return
	}
	// Keep only the last dedupKeepSize entries from the ordered slice
	recentIDs := cd.seenOrder[len(cd.seenOrder)-dedupKeepSize:]
	trimmed := make(map[string]struct{}, dedupKeepSize)
	for _, id := range recentIDs {
		trimmed[id] = struct{}{}
	}
	cd.seenIDs = trimmed
	// Copy to a fresh slice to release old memory
	cd.seenOrder = append([]string(nil), recentIDs...)
}

// SetOutputFile updates the output file path. Used when early chat is started
// before the staging directory exists — the path is set later when staging is created.
func (cd *ChatDownloader) SetOutputFile(path string) {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	cd.opts.OutputFile = path
	if cd.opts.ResumeFile == "" || cd.opts.ResumeFile == ".resume.json" {
		cd.opts.ResumeFile = path + ".resume.json"
	}
}

// getOutputPaths copies the output and resume file paths under lock.
// Callers running outside the main loop (or after SetOutputFile may be called)
// should use these copies instead of reading cd.opts directly.
func (cd *ChatDownloader) getOutputPaths() (outputFile, resumeFile string) {
	cd.mu.Lock()
	outputFile = cd.opts.OutputFile
	resumeFile = cd.opts.ResumeFile
	cd.mu.Unlock()
	return
}

// writeChatFile writes chat data to the output file.
// Two paths:
// - Not flushed: all messages are in memory, write complete JSON atomically.
// - Flushed: the on-disk file already has old messages. Use incremental append:
//   read only the last bytes to locate ']', truncate there, then append new
//   messages + closing structure. Memory cost: O(new messages) not O(file size).
//
// On success, clears the in-memory buffer and marks flushedToDisk = true.
func (cd *ChatDownloader) writeChatFile() {
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return // No output file set yet (early chat), buffer in memory
	}
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o755); err != nil {
		return
	}

	if !cd.flushedToDisk {
		// All messages in memory — write complete file atomically
		cd.writeFullChatFile()
		cd.messages = nil // All written to disk, free memory
		cd.flushedToDisk = true
		return
	}

	// Incremental append: open existing file and append new messages.
	// If any step fails, fall back to a full rewrite that prepends on-disk
	// messages — both paths clear the in-memory buffer on success.
	if cd.incrementalAppend(outputFile) {
		cd.messages = nil
	} else {
		cd.prependExistingMessages(outputFile)
		cd.writeFullChatFile()
		cd.messages = nil
	}
}

// incrementalAppend performs an in-place append of cd.messages to the existing
// chat file on disk. Returns true on success, false if a full rewrite is needed.
func (cd *ChatDownloader) incrementalAppend(outputFile string) bool {
	newMessages := cd.messages
	if len(newMessages) == 0 {
		return true
	}

	// Check if output file exists
	info, err := os.Stat(outputFile)
	if err != nil || info.Size() < 10 {
		// File missing or too small — fall back to full write
		if cd.OnError != nil {
			if err != nil {
				cd.OnError(fmt.Errorf("chat file stat failed, rewriting: %w", err))
			} else {
				cd.OnError(fmt.Errorf("chat file too small (%d bytes), rewriting", info.Size()))
			}
		}
		return false
	}

	f, err := os.OpenFile(outputFile, os.O_RDWR, 0o644)
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("chat file open failed, rewriting: %w", err))
		}
		return false
	}
	defer f.Close()

	fileSize := info.Size()

	// Read last 10 bytes to find ']'
	tailSize := min(int64(10), fileSize)
	tailBuf := make([]byte, tailSize)
	_, err = f.ReadAt(tailBuf, fileSize-tailSize)
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("chat file read failed, rewriting: %w", err))
		}
		return false
	}

	tail := string(tailBuf)
	bracketOffset := -1
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i] == ']' {
			bracketOffset = i
			break
		}
	}
	if bracketOffset == -1 {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("chat file missing closing bracket, rewriting"))
		}
		return false
	}

	bracketBytePos := fileSize - tailSize + int64(bracketOffset)

	// Check if there are existing messages (look for '}' before ']')
	hasExisting := false
	if bracketBytePos > 5 {
		checkSize := min(int64(5), bracketBytePos)
		checkBuf := make([]byte, checkSize)
		if _, err := f.ReadAt(checkBuf, bracketBytePos-checkSize); err != nil {
			if cd.OnError != nil {
				cd.OnError(fmt.Errorf("chat file check-read failed, rewriting: %w", err))
			}
			return false
		}
		for i := len(checkBuf) - 1; i >= 0; i-- {
			if checkBuf[i] == '}' {
				hasExisting = true
				break
			} else if checkBuf[i] != ' ' && checkBuf[i] != '\n' && checkBuf[i] != '\r' && checkBuf[i] != '\t' {
				break
			}
		}
	}

	// Build append content
	var sb strings.Builder
	if hasExisting {
		sb.WriteString(",\n")
	}
	for i, msg := range newMessages {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		sb.WriteString("    ")
		sb.Write(msgBytes)
		if i < len(newMessages)-1 {
			sb.WriteString(",\n")
		}
	}
	sb.WriteString("\n  ]\n}")
	appendStr := sb.String()

	// Truncate at ']' position, then write new content
	if err := f.Truncate(bracketBytePos); err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("chat file truncate failed, rewriting: %w", err))
		}
		return false
	}
	// WriteAt after truncation — log if it fails since data may be lost.
	if _, err := f.WriteAt([]byte(appendStr), bracketBytePos); err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("write appended chat messages: %w", err))
		}
		// Data partially written but caller should not re-buffer — treat as success.
	}
	return true
}

func (cd *ChatDownloader) writeFullChatFile() {
	outputFile, _ := cd.getOutputPaths()

	data := ChatData{
		VideoID:         cd.opts.VideoID,
		VideoTitle:      cd.opts.VideoTitle,
		ChannelName:     cd.opts.ChannelName,
		StreamStartTime: cd.opts.StreamStartTime,
		DownloadedAt:    time.Now().UTC().Format(time.RFC3339),
		MessageCount:    cd.messageCount,
		Messages:        cd.messages,
	}

	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("marshal chat data: %w", err))
		}
		return
	}
	jsonBytes = utils.PadMessageCountJSON(jsonBytes)

	tmpFile := outputFile + ".tmp"
	// Write + fsync + close before rename. A crash between write and rename
	// must not leave an empty/truncated tmp file that could then be renamed
	// over a good chat.json, since the rename is the atomicity guarantee.
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("open chat file: %w", err))
		}
		return
	}
	if _, err := f.Write(jsonBytes); err != nil {
		f.Close()
		os.Remove(tmpFile)
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("write chat file: %w", err))
		}
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpFile)
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("fsync chat file: %w", err))
		}
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpFile)
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("close chat file: %w", err))
		}
		return
	}
	if err := os.Rename(tmpFile, outputFile); err != nil {
		os.Remove(tmpFile)
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("rename chat file: %w", err))
		}
	}
}

// readExistingMessages attempts to read previously-flushed messages from the
// chat file on disk. Returns nil on any error (caller should log a warning).
func (cd *ChatDownloader) readExistingMessages(path string) []ChatMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var chatData ChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return nil
	}
	return chatData.Messages
}

// prependExistingMessages reads previously-flushed messages from disk and prepends
// them to cd.messages. It also registers their IDs in seenIDs to prevent duplicates
// on subsequent API responses that may overlap with the recovered messages.
func (cd *ChatDownloader) prependExistingMessages(outputFile string) {
	existing := cd.readExistingMessages(outputFile)
	if existing == nil {
		return
	}
	cd.messages = append(existing, cd.messages...)
	cd.messageCount = len(cd.messages)
	// Register recovered message IDs in dedup maps to prevent duplicates
	for _, msg := range existing {
		if msg.ID != "" {
			if _, seen := cd.seenIDs[msg.ID]; !seen {
				cd.seenIDs[msg.ID] = struct{}{}
				cd.seenOrder = append(cd.seenOrder, msg.ID)
			}
		}
	}
}

// updateChatFileHeader updates messageCount and downloadedAt in the JSON header
// without rewriting the entire file. Reads only the first 1KB.
func (cd *ChatDownloader) updateChatFileHeader() {
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return // No output file set yet (early chat)
	}

	info, err := os.Stat(outputFile)
	if err != nil {
		if !os.IsNotExist(err) && cd.OnError != nil {
			cd.OnError(fmt.Errorf("update chat header: stat: %w", err))
		}
		return
	}
	if info.Size() < 50 {
		return // File too small to have a meaningful header yet
	}

	f, err := os.OpenFile(outputFile, os.O_RDWR, 0o644)
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("update chat header: open: %w", err))
		}
		return
	}
	defer f.Close()

	headerSize := min(int64(1024), info.Size())

	headerBuf := make([]byte, headerSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n == 0 {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("update chat header: read: %w", err))
		}
		return
	}
	header := string(headerBuf[:n])

	// Replace messageCount value (padded to fixed width)
	header = utils.ReplaceMessageCount(header, cd.messageCount)

	// Replace downloadedAt value
	utils.ReplaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	// With padded messageCount, the header size should be constant.
	// Fallback handles legacy files written before padding was added.
	updatedBytes := []byte(header)
	if len(updatedBytes) == n {
		if _, err := f.WriteAt(updatedBytes, 0); err != nil && cd.OnError != nil {
			cd.OnError(fmt.Errorf("update chat header: write: %w", err))
		}
	} else if len(updatedBytes) > n {
		restSize := info.Size() - int64(n)
		var restBuf []byte
		if restSize > 0 {
			restBuf = make([]byte, restSize)
			nRest, _ := f.ReadAt(restBuf, int64(n))
			restBuf = restBuf[:nRest]
		}
		if _, err := f.WriteAt(updatedBytes, 0); err != nil {
			if cd.OnError != nil {
				cd.OnError(fmt.Errorf("update chat header: write expanded: %w", err))
			}
			return
		}
		if len(restBuf) > 0 {
			if _, err := f.WriteAt(restBuf, int64(len(updatedBytes))); err != nil {
				if cd.OnError != nil {
					cd.OnError(fmt.Errorf("update chat header: write rest: %w", err))
				}
				return
			}
		}
		if err := f.Truncate(int64(len(updatedBytes)) + restSize); err != nil && cd.OnError != nil {
			cd.OnError(fmt.Errorf("update chat header: truncate: %w", err))
		}
	}
}

func (cd *ChatDownloader) loadResume() (*ChatResumeState, error) {
	data, err := os.ReadFile(cd.opts.ResumeFile)
	if err != nil {
		return nil, err
	}
	var state ChatResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (cd *ChatDownloader) saveResume() {
	// Snapshot the fields under lock, then release before the ~200 KB marshal
	// + file write. Holding cd.mu for the entire write would block concurrent
	// MessageCount() / IsRunning() callers (TUI/web UI refreshers) on every
	// batched flush.
	cd.mu.Lock()
	outputFile := cd.opts.OutputFile
	resumeFile := cd.opts.ResumeFile
	if resumeFile == "" || outputFile == "" {
		cd.mu.Unlock()
		return // No output path yet (early chat)
	}
	// Use seenOrder (insertion order) for deterministic resume state,
	// not map iteration which is non-deterministic in Go.
	// Cap to dedupKeepSize to prevent resume files from growing unbounded.
	src := cd.seenOrder
	if len(src) > dedupKeepSize {
		src = src[len(src)-dedupKeepSize:]
	}
	recentIDs := make([]string, len(src))
	copy(recentIDs, src)

	state := ChatResumeState{
		MessageCount:      cd.messageCount,
		Continuation:      cd.continuation,
		Timestamp:         time.Now().Unix(),
		VideoID:           cd.opts.VideoID,
		RecentIDs:         recentIDs,
		LastTimestampUsec: cd.lastTimestamp,
	}
	cd.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("marshal resume state: %w", err))
		}
		return
	}
	tmpFile := resumeFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("write resume state: %w", err))
		}
		return
	}
	if err := os.Rename(tmpFile, resumeFile); err != nil {
		if cd.OnError != nil {
			cd.OnError(fmt.Errorf("rename resume state: %w", err))
		}
	}
}

func (cd *ChatDownloader) clearResume() {
	_, resumeFile := cd.getOutputPaths()
	os.Remove(resumeFile)
}

// sleep waits for the given duration, but returns early if the context is cancelled
// (which happens on Stop() or MarkStreamEnded()).
func (cd *ChatDownloader) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
