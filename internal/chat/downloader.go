package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dedupKeepSize        = 5000 // Keep last N IDs for dedup
	maxConsecErrorsLive  = 20
	maxConsecErrorsVod   = 5
	maxStaleContinuation = 30
	writeIntervalMs      = 1000 // Flush to disk within 1 second
	resumeSaveInterval   = 10 * time.Second
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
	lastWriteMs   int64
	lastTimestamp  string
	cancelCtx     context.CancelFunc // for aborting sleep on stop/markStreamEnded

	OnStart    func(messageCount int, resuming bool)
	OnProgress func(p ChatProgress)
	OnFinish   func()
	OnError    func(err error)
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
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	if cd.running {
		cd.mu.Unlock()
		return nil
	}
	cd.running = true
	cd.cancelFlag = false
	cd.streamEnded = false
	cd.mu.Unlock()

	defer func() {
		cd.mu.Lock()
		cd.running = false
		cd.cancelCtx = nil
		cd.mu.Unlock()
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

	// Clear resume state on clean completion (not cancelled)
	cd.mu.Lock()
	wasCancelled := cd.cancelFlag
	cd.mu.Unlock()
	if !wasCancelled {
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
		if !switchedToAllChat && resp.AllChatContinuation != "" {
			cd.continuation = resp.AllChatContinuation
			switchedToAllChat = true
			continue // Re-poll immediately with the unfiltered continuation
		}
		switchedToAllChat = true // Don't check again even if header wasn't present

		// Process messages
		newInBatch := 0
		for i := range resp.Messages {
			msg := &resp.Messages[i]

			// Calculate offsetMs if not already set
			if msg.OffsetMs == 0 && cd.streamStartMs > 0 && msg.TimestampUsec != "" {
				usec, _ := strconv.ParseInt(msg.TimestampUsec, 10, 64)
				if usec > 0 {
					msg.OffsetMs = usec/1000 - cd.streamStartMs
				}
			}

			// Dedup by ID
			if _, seen := cd.seenIDs[msg.ID]; seen {
				continue
			}

			cd.seenIDs[msg.ID] = struct{}{}
			cd.seenOrder = append(cd.seenOrder, msg.ID)
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

		// Write to disk within 1-second batching window
		if newInBatch > 0 {
			now := time.Now().UnixMilli()
			if now-cd.lastWriteMs >= writeIntervalMs {
				cd.writeChatFile()
				cd.lastWriteMs = now
				cd.messages = nil // All written to disk, free memory
				cd.flushedToDisk = true

				// Bound seenIDs to prevent unbounded growth
				if len(cd.seenIDs) > dedupKeepSize {
					cd.cullDedup()
				}

				cd.saveResume()
			}
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
					cd.sleep(ctx, 10*time.Second)
					continue
				}

				// No fresh continuation — retry with exponential backoff
				contRetryDelay := 10 * time.Second
				maxContRetries := maxStaleContinuation
				contRetries := 0

				for !cd.shouldStop() && contRetries < maxContRetries {
					cd.sleep(ctx, contRetryDelay)
					contRetries++
					retry, _, retryErr := cd.api.FetchFreshContinuation(ctx, cd.opts.VideoID)
					if retryErr == nil && retry != "" {
						cd.continuation = retry
						switchedToAllChat = false // Fresh token defaults to Top Chat — re-trigger switch
						break
					}
					// Exponential backoff: 10s, 20s, 40s, 80s, cap at 5min
					contRetryDelay = min(contRetryDelay*2, 5*time.Minute)
				}
				if contRetries >= maxContRetries {
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

	// Save resume state only when cancelled (resume needed)
	cd.mu.Lock()
	wasCancelled := cd.cancelFlag
	cd.mu.Unlock()
	if len(cd.messages) > 0 && wasCancelled {
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

// writeChatFile writes chat data to the output file.
// Two paths:
// - Not flushed: all messages are in memory, write complete JSON atomically.
// - Flushed: the on-disk file already has old messages. Use incremental append:
//   read only the last bytes to locate ']', truncate there, then append new
//   messages + closing structure. Memory cost: O(new messages) not O(file size).
func (cd *ChatDownloader) writeChatFile() {
	if cd.opts.OutputFile == "" {
		return // No output file set yet (early chat), buffer in memory
	}
	if err := os.MkdirAll(filepath.Dir(cd.opts.OutputFile), 0o755); err != nil {
		return
	}

	if !cd.flushedToDisk {
		// All messages in memory — write complete file atomically
		cd.writeFullChatFile()
		return
	}

	// Incremental append: open existing file and append new messages
	newMessages := cd.messages
	if len(newMessages) == 0 {
		return
	}

	// Check if output file exists
	info, err := os.Stat(cd.opts.OutputFile)
	if err != nil || info.Size() < 10 {
		// File missing or too small — fall back to full write
		cd.writeFullChatFile()
		return
	}

	f, err := os.OpenFile(cd.opts.OutputFile, os.O_RDWR, 0o644)
	if err != nil {
		cd.writeFullChatFile()
		return
	}
	defer f.Close()

	fileSize := info.Size()

	// Read last 10 bytes to find ']'
	tailSize := int64(10)
	if tailSize > fileSize {
		tailSize = fileSize
	}
	tailBuf := make([]byte, tailSize)
	_, err = f.ReadAt(tailBuf, fileSize-tailSize)
	if err != nil {
		f.Close()
		cd.writeFullChatFile()
		return
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
		f.Close()
		cd.writeFullChatFile()
		return
	}

	bracketBytePos := fileSize - tailSize + int64(bracketOffset)

	// Check if there are existing messages (look for '}' before ']')
	hasExisting := false
	if bracketBytePos > 5 {
		checkSize := int64(5)
		if checkSize > bracketBytePos {
			checkSize = bracketBytePos
		}
		checkBuf := make([]byte, checkSize)
		f.ReadAt(checkBuf, bracketBytePos-checkSize)
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
		f.Close()
		cd.writeFullChatFile()
		return
	}
	f.WriteAt([]byte(appendStr), bracketBytePos)
}

func (cd *ChatDownloader) writeFullChatFile() {
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
		return
	}

	tmpFile := cd.opts.OutputFile + ".tmp"
	if err := os.WriteFile(tmpFile, jsonBytes, 0o644); err != nil {
		return
	}
	os.Rename(tmpFile, cd.opts.OutputFile)
}

// updateChatFileHeader updates messageCount and downloadedAt in the JSON header
// without rewriting the entire file. Reads only the first 1KB.
func (cd *ChatDownloader) updateChatFileHeader() {
	info, err := os.Stat(cd.opts.OutputFile)
	if err != nil || info.Size() < 50 {
		return
	}

	f, err := os.OpenFile(cd.opts.OutputFile, os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	headerSize := int64(1024)
	if headerSize > info.Size() {
		headerSize = info.Size()
	}

	headerBuf := make([]byte, headerSize)
	n, err := f.ReadAt(headerBuf, 0)
	if err != nil && n == 0 {
		return
	}
	header := string(headerBuf[:n])

	// Simple string replacement approach
	updated := header
	// Replace messageCount value
	mcStart := strings.Index(updated, `"messageCount":`)
	if mcStart >= 0 {
		mcValStart := mcStart + len(`"messageCount":`)
		// Skip whitespace
		for mcValStart < len(updated) && (updated[mcValStart] == ' ' || updated[mcValStart] == '\t') {
			mcValStart++
		}
		mcValEnd := mcValStart
		for mcValEnd < len(updated) && updated[mcValEnd] >= '0' && updated[mcValEnd] <= '9' {
			mcValEnd++
		}
		if mcValEnd > mcValStart {
			newVal := strconv.Itoa(cd.messageCount)
			updated = updated[:mcValStart] + newVal + updated[mcValEnd:]
		}
	}

	daStart := strings.Index(updated, `"downloadedAt":`)
	if daStart >= 0 {
		daValStart := daStart + len(`"downloadedAt":`)
		// Skip whitespace and opening quote
		for daValStart < len(updated) && (updated[daValStart] == ' ' || updated[daValStart] == '\t') {
			daValStart++
		}
		if daValStart < len(updated) && updated[daValStart] == '"' {
			daValStart++ // skip opening quote
			daValEnd := daValStart
			for daValEnd < len(updated) && updated[daValEnd] != '"' {
				daValEnd++
			}
			newVal := time.Now().UTC().Format(time.RFC3339)
			updated = updated[:daValStart] + newVal + updated[daValEnd:]
		}
	}

	// Only write if byte length matches (safe in-place update)
	updatedBytes := []byte(updated)
	if len(updatedBytes) == n {
		f.WriteAt(updatedBytes, 0)
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
	if cd.opts.ResumeFile == "" || cd.opts.OutputFile == "" {
		return // No output path yet (early chat)
	}
	// Use seenOrder (insertion order) for deterministic resume state,
	// not map iteration which is non-deterministic in Go.
	recentIDs := make([]string, len(cd.seenOrder))
	copy(recentIDs, cd.seenOrder)

	state := ChatResumeState{
		MessageCount:      cd.messageCount,
		Continuation:      cd.continuation,
		Timestamp:         time.Now().Unix(),
		VideoID:           cd.opts.VideoID,
		RecentIDs:         recentIDs,
		LastTimestampUsec: cd.lastTimestamp,
	}

	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmpFile := cd.opts.ResumeFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return
	}
	os.Rename(tmpFile, cd.opts.ResumeFile)
}

func (cd *ChatDownloader) clearResume() {
	os.Remove(cd.opts.ResumeFile)
}

// sleep waits for the given duration, but returns early if the context is cancelled
// (which happens on Stop() or MarkStreamEnded()).
func (cd *ChatDownloader) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
