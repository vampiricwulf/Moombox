package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

const (
	chatProgressInterval    = 100
	chatMaxConsecutiveErrs  = 20
	chatDedupMax            = 5000
	chatFlushThresholdCount = 50_000
	chatSaveInterval        = 1 * time.Second
)

// ChatDownloader connects to Twitch IRC and records live chat messages.
type ChatDownloader struct {
	mu               sync.Mutex
	channelLogin     string
	channelDisplay   string
	channelID        string
	streamID         string
	authToken        string
	recordingStartMs int64
	streamStartTime  string
	streamStartMs    int64
	messages         []TwitchChatMessage // Unwritten messages in memory
	seenIDs          map[string]struct{}
	seenOrder        []string // Insertion-order tracking for deterministic culling
	outputPath       string
	running          bool
	totalCount       int
	lastTimestampMs  int64 // Last message timestamp (epoch ms) for resume state
	flushedToDisk    bool
	emoteResolver    *EmoteResolver

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	OnProgress func(count int)
}

// ChatDownloaderOptions configures the chat downloader.
type ChatDownloaderOptions struct {
	ChannelLogin     string
	ChannelDisplay   string
	ChannelID        string
	StreamID         string
	AuthToken        string
	OutputPath       string
	StreamStartTime  string
	EmoteResolver    *EmoteResolver
}

// twitchChatResumeState persists IRC chat state for resume after reconnection.
// Matches TypeScript TwitchChatResumeState interface.
type twitchChatResumeState struct {
	MessageCount    int      `json:"messageCount"`
	LastTimestampMs int64    `json:"lastTimestampMs"`
	Timestamp       int64    `json:"timestamp"`
	StreamID        string   `json:"streamId"`
	RecentIDs       []string `json:"recentIds,omitempty"`
}

// NewChatDownloader creates a new IRC chat downloader.
func NewChatDownloader(opts ChatDownloaderOptions, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *ChatDownloader {
	var streamStartMs int64
	if opts.StreamStartTime != "" {
		if t, err := time.Parse(time.RFC3339, opts.StreamStartTime); err == nil {
			streamStartMs = t.UnixMilli()
		}
	}

	return &ChatDownloader{
		channelLogin:    opts.ChannelLogin,
		channelDisplay:  opts.ChannelDisplay,
		channelID:       opts.ChannelID,
		streamID:        opts.StreamID,
		authToken:       opts.AuthToken,
		outputPath:      opts.OutputPath,
		streamStartTime: opts.StreamStartTime,
		streamStartMs:   streamStartMs,
		seenIDs:         make(map[string]struct{}),
		emoteResolver:   opts.EmoteResolver,
		logger:          logger,
	}
}

// SetRecordingStartTime sets the recording start time for offset calculation.
// Should be called before Start() when the actual recording begins.
func (cd *ChatDownloader) SetRecordingStartTime(isoString string) {
	if t, err := time.Parse(time.RFC3339, isoString); err == nil {
		cd.mu.Lock()
		cd.recordingStartMs = t.UnixMilli()
		cd.mu.Unlock()
	}
}

// getResumeFilePath returns the path to the resume state file.
func (cd *ChatDownloader) getResumeFilePath() string {
	return cd.outputPath + ".resume.json"
}

// loadResumeState loads the resume state from disk.
// Returns nil if no valid resume state exists for the current stream.
func (cd *ChatDownloader) loadResumeState() *twitchChatResumeState {
	resumePath := cd.getResumeFilePath()
	data, err := os.ReadFile(resumePath)
	if err != nil {
		return nil
	}

	var state twitchChatResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}

	// Only use resume state if it matches the current stream
	if state.StreamID != cd.streamID {
		return nil
	}

	return &state
}

// saveResumeState persists the current chat state for resume after crash/reconnect.
// Uses atomic write pattern with .tmp file (matches TS saveResumeState).
func (cd *ChatDownloader) saveResumeState() {
	cd.mu.Lock()
	recentIDs := make([]string, 0, len(cd.seenOrder))
	recentIDs = append(recentIDs, cd.seenOrder...)
	state := twitchChatResumeState{
		MessageCount:    cd.totalCount,
		LastTimestampMs: cd.lastTimestampMs,
		Timestamp:       time.Now().UnixMilli(),
		StreamID:        cd.streamID,
		RecentIDs:       recentIDs,
	}
	cd.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		return
	}

	resumePath := cd.getResumeFilePath()
	tmpPath := resumePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return
	}
	os.Rename(tmpPath, resumePath)
}

// clearResumeState deletes the resume state file on successful completion.
func (cd *ChatDownloader) clearResumeState() {
	os.Remove(cd.getResumeFilePath())
}

// Start connects to Twitch IRC and begins recording chat messages.
// C1: Includes reconnect logic with exponential backoff on error limit.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()

	// Try to resume from saved state (matches TS start() resume logic)
	resumeState := cd.loadResumeState()
	if resumeState != nil && len(resumeState.RecentIDs) > 0 {
		cd.mu.Lock()
		cd.totalCount = resumeState.MessageCount
		cd.lastTimestampMs = resumeState.LastTimestampMs
		cd.flushedToDisk = true
		cd.seenIDs = make(map[string]struct{}, len(resumeState.RecentIDs))
		cd.seenOrder = make([]string, 0, len(resumeState.RecentIDs))
		for _, id := range resumeState.RecentIDs {
			cd.seenIDs[id] = struct{}{}
			cd.seenOrder = append(cd.seenOrder, id)
		}
		cd.mu.Unlock()
		cd.logger.Info("[TwitchChat] Resuming from saved state", "messages", resumeState.MessageCount)
	}

	defer func() {
		cd.mu.Lock()
		cd.running = false
		cd.mu.Unlock()
		cd.flush()

		// Clear resume state on successful completion (matches TS clearResumeState)
		cd.clearResumeState()

		// Inject third-party emotes (7TV, BTTV, FFZ) after final flush
		if cd.totalCount > 0 && cd.emoteResolver != nil && cd.channelID != "" {
			cd.logger.Info("resolving emotes for Twitch chat", "channelID", cd.channelID)
			emoteData := cd.emoteResolver.Resolve(ctx, cd.channelID, cd.channelLogin)
			if emoteData != nil {
				if err := EnrichWithEmotes(cd.outputPath, emoteData); err != nil {
					cd.logger.Warn("emote injection failed", "err", err)
				}
			}
		}
	}()

	// C1: Reconnect loop — on error limit, save state and reconnect
	maxReconnects := 10
	reconnectAttempts := 0

	for reconnectAttempts <= maxReconnects {
		if ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		if reconnectAttempts > 0 {
			// Exponential backoff: 1000 * 2^attempts, capped at 30s (matches TypeScript)
			delayMs := 1000 * (1 << reconnectAttempts)
			if delayMs > 30000 {
				delayMs = 30000
			}
			delay := time.Duration(delayMs) * time.Millisecond
			cd.logger.Info("reconnecting to twitch IRC",
				"channel", cd.channelLogin, "attempt", reconnectAttempts, "max", maxReconnects, "delay", delay)
			cd.flush() // Save state before reconnect
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
		}

		err := cd.runIRCSession(ctx)
		if err == nil || ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}
		reconnectAttempts++
		cd.logger.Warn("IRC session error, will reconnect", "err", err, "channel", cd.channelLogin)
	}

	return fmt.Errorf("exceeded max IRC reconnects for %s", cd.channelLogin)
}

// runIRCSession runs a single IRC connection session.
func (cd *ChatDownloader) runIRCSession(ctx context.Context) error {
	cd.logger.Info("connecting to twitch IRC", "channel", cd.channelLogin)

	conn, _, err := websocket.Dial(ctx, constants.TwitchURLs.IRCWS, nil)
	if err != nil {
		return fmt.Errorf("connect IRC: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Authenticate
	if cd.authToken != "" {
		conn.Write(ctx, websocket.MessageText, []byte("PASS oauth:"+cd.authToken))
	} else {
		conn.Write(ctx, websocket.MessageText, []byte("PASS SCHMOOPIIE"))
	}
	nick := fmt.Sprintf("justinfan%d", rand.Intn(100000))
	conn.Write(ctx, websocket.MessageText, []byte("NICK "+nick))

	// Request capabilities
	conn.Write(ctx, websocket.MessageText, []byte("CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership"))

	// Join channel
	conn.Write(ctx, websocket.MessageText, []byte("JOIN #"+strings.ToLower(cd.channelLogin)))

	cd.logger.Info("joined twitch IRC", "channel", cd.channelLogin)

	saveTicker := time.NewTicker(chatSaveInterval)
	defer saveTicker.Stop()

	consecutiveErrors := 0

	for cd.IsRunning() {
		select {
		case <-ctx.Done():
			return nil
		case <-saveTicker.C:
			cd.flush()
		default:
		}

		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			consecutiveErrors++
			if consecutiveErrors >= chatMaxConsecutiveErrs {
				// Return error to trigger reconnect
				return fmt.Errorf("too many IRC errors: %w", err)
			}
			continue
		}
		consecutiveErrors = 0

		lines := strings.Split(string(data), "\r\n")
		for _, line := range lines {
			if line == "" {
				continue
			}

			// Handle PING
			if strings.HasPrefix(line, "PING") {
				conn.Write(ctx, websocket.MessageText, []byte("PONG :tmi.twitch.tv"))
				continue
			}

			msg := cd.parseLine(line)
			if msg == nil {
				continue
			}

			cd.addMessage(msg)
		}
	}
	return nil
}

func (cd *ChatDownloader) parseLine(line string) *TwitchChatMessage {
	// Parse IRC tags
	var tagsStr string
	rest := line

	if strings.HasPrefix(line, "@") {
		idx := strings.Index(line, " ")
		if idx < 0 {
			return nil
		}
		tagsStr = line[1:idx]
		rest = line[idx+1:]
	}

	tags := parseIRCTags(tagsStr)

	// Parse command
	parts := strings.SplitN(rest, " ", 4)
	if len(parts) < 3 {
		return nil
	}

	command := parts[1]

	switch command {
	case "PRIVMSG":
		return cd.parsePrivmsg(tags, parts, line)
	case "USERNOTICE":
		return cd.parseUsernotice(tags, parts, line)
	default:
		return nil
	}
}

func (cd *ChatDownloader) parsePrivmsg(tags map[string]string, parts []string, rawLine string) *TwitchChatMessage {
	id := tags["id"]
	if id == "" {
		return nil
	}

	tmiSentTs, _ := strconv.ParseInt(tags["tmi-sent-ts"], 10, 64)
	if tmiSentTs == 0 {
		tmiSentTs = time.Now().UnixMilli()
	}
	bits, _ := strconv.Atoi(tags["bits"])

	msgType := "chat"
	if bits > 0 {
		msgType = "bits"
	}

	// Extract message text (after the last ':')
	var messageText string
	if len(parts) >= 4 {
		messageText = parts[3]
		if strings.HasPrefix(messageText, ":") {
			messageText = messageText[1:]
		}
	}

	// Author name fallback chain
	authorName := tags["display-name"]
	if authorName == "" {
		authorName = tags["login"]
	}
	if authorName == "" {
		authorName = "Anonymous"
	}

	baseMs := cd.recordingStartMs
	if baseMs == 0 {
		baseMs = cd.streamStartMs
	}
	var offsetMs int64
	if baseMs > 0 {
		offsetMs = tmiSentTs - baseMs
		if offsetMs < 0 {
			offsetMs = 0
		}
	}

	msg := &TwitchChatMessage{
		ID:           id,
		TimestampMs:  tmiSentTs,
		OffsetMs:     offsetMs,
		AuthorName:   authorName,
		AuthorID:     tags["user-id"],
		AuthorBadges: parseBadges(tags["badges"]),
		AuthorColor:  tags["color"],
		Message:      messageText,
		Emotes:       parseEmoteTags(tags["emotes"], messageText),
		Bits:         bits,
		MessageType:  msgType,
		Raw:          rawLine,
	}

	return msg
}

func (cd *ChatDownloader) parseUsernotice(tags map[string]string, parts []string, rawLine string) *TwitchChatMessage {
	id := tags["id"]
	if id == "" {
		return nil
	}

	tmiSentTs, _ := strconv.ParseInt(tags["tmi-sent-ts"], 10, 64)
	if tmiSentTs == 0 {
		tmiSentTs = time.Now().UnixMilli()
	}
	msgID := tags["msg-id"] // "sub", "resub", "subgift", "raid", etc.

	// Normalize message type like TS does
	normalizedType := "system"
	switch msgID {
	case "sub":
		normalizedType = "sub"
	case "resub":
		normalizedType = "resub"
	case "subgift", "submysterygift":
		normalizedType = "subgift"
	case "raid":
		normalizedType = "raid"
	}

	// System message (unescape \s to space)
	systemMsg := strings.ReplaceAll(tags["system-msg"], `\s`, " ")

	var messageText string
	if len(parts) >= 4 {
		messageText = parts[3]
		if strings.HasPrefix(messageText, ":") {
			messageText = messageText[1:]
		}
	}
	// If no trailing message, fall back to system message
	if messageText == "" {
		messageText = systemMsg
	}

	// Author name fallback chain
	authorName := tags["display-name"]
	if authorName == "" {
		authorName = tags["login"]
	}
	if authorName == "" {
		authorName = "System"
	}

	baseMs := cd.recordingStartMs
	if baseMs == 0 {
		baseMs = cd.streamStartMs
	}
	var offsetMs int64
	if baseMs > 0 {
		offsetMs = tmiSentTs - baseMs
		if offsetMs < 0 {
			offsetMs = 0
		}
	}

	msg := &TwitchChatMessage{
		ID:           id,
		TimestampMs:  tmiSentTs,
		OffsetMs:     offsetMs,
		AuthorName:   authorName,
		AuthorID:     tags["user-id"],
		AuthorBadges: parseBadges(tags["badges"]),
		AuthorColor:  tags["color"],
		Message:      messageText,
		Emotes:       parseEmoteTags(tags["emotes"], messageText),
		MessageType:  normalizedType,
		SystemMsg:    systemMsg,
		Raw:          rawLine,
	}

	// C1: Extract additional USERNOTICE-specific fields
	if v := tags["msg-param-sub-plan"]; v != "" {
		msg.SubPlan = v
	}
	if v := tags["msg-param-recipient-display-name"]; v != "" {
		msg.GiftRecipient = v
	}
	if v, err := strconv.Atoi(tags["msg-param-viewerCount"]); err == nil && v > 0 {
		msg.ViewerCount = v
	}

	return msg
}

func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()
	defer cd.mu.Unlock()

	// Dedup
	if _, seen := cd.seenIDs[msg.ID]; seen {
		return
	}
	cd.seenIDs[msg.ID] = struct{}{}
	cd.seenOrder = append(cd.seenOrder, msg.ID)

	// Prune dedup map — keep most recent chatDedupMax entries by insertion order
	if len(cd.seenOrder) > chatDedupMax*2 {
		removeIDs := cd.seenOrder[:len(cd.seenOrder)-chatDedupMax]
		for _, id := range removeIDs {
			delete(cd.seenIDs, id)
		}
		cd.seenOrder = cd.seenOrder[len(cd.seenOrder)-chatDedupMax:]
	}

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}

	if cd.OnProgress != nil && cd.totalCount%chatProgressInterval == 0 {
		cd.OnProgress(cd.totalCount)
	}
}

func (cd *ChatDownloader) flush() {
	cd.mu.Lock()
	msgs := make([]TwitchChatMessage, len(cd.messages))
	copy(msgs, cd.messages)
	count := cd.totalCount
	flushed := cd.flushedToDisk
	cd.mu.Unlock()

	if len(msgs) == 0 || cd.outputPath == "" {
		return
	}

	dir := filepath.Dir(cd.outputPath)
	os.MkdirAll(dir, 0o755)

	var writeErr error
	if !flushed {
		// First flush: write complete file
		writeErr = cd.writeFullChatFile(msgs, count)
	} else {
		// Subsequent flushes: append new messages to existing file
		writeErr = cd.appendToFile(msgs)
		if writeErr != nil {
			// Fallback to full rewrite on append error
			cd.logger.Debug("append failed, falling back to full write", "err", writeErr)
			writeErr = cd.writeFullChatFile(msgs, count)
		}
	}

	if writeErr != nil {
		cd.logger.Error("write chat file", "err", writeErr)
		return
	}

	// Clear messages from memory after successful write
	cd.mu.Lock()
	cd.messages = cd.messages[:0]
	cd.flushedToDisk = true
	cd.mu.Unlock()

	// Prune dedup set to prevent unbounded memory growth
	cd.pruneDedup()

	// Save resume state after each flush (matches TS periodicSave → saveResumeState)
	cd.saveResumeState()
}

// writeFullChatFile writes all messages as a complete JSON file atomically.
func (cd *ChatDownloader) writeFullChatFile(msgs []TwitchChatMessage, count int) error {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       cd.channelLogin,
		ChannelDisplayName: cd.channelDisplay,
		StreamID:           cd.streamID,
		StreamStartTime:    cd.streamStartTime,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       count,
		Messages:           msgs,
	}
	if cd.recordingStartMs > 0 {
		chatData.RecordingStartTime = time.UnixMilli(cd.recordingStartMs).UTC().Format(time.RFC3339)
	}

	data, err := json.MarshalIndent(chatData, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := cd.outputPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, cd.outputPath)
}

// appendToFile appends new messages to the existing JSON file by finding the
// closing ']' of the messages array and appending new entries. This avoids
// re-serializing the entire file on each flush, keeping memory O(new messages).
func (cd *ChatDownloader) appendToFile(msgs []TwitchChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	info, err := os.Stat(cd.outputPath)
	if err != nil || info.Size() < 10 {
		return fmt.Errorf("file missing or too small")
	}

	f, err := os.OpenFile(cd.outputPath, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	fileSize := info.Size()

	// Read last 10 bytes to find ']'
	tailSize := int64(10)
	if tailSize > fileSize {
		tailSize = fileSize
	}
	tailBuf := make([]byte, tailSize)
	if _, err := f.ReadAt(tailBuf, fileSize-tailSize); err != nil {
		return err
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
		return fmt.Errorf("no closing bracket found")
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
	var appendStr string
	if hasExisting {
		appendStr = ",\n"
	}
	for i, msg := range msgs {
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		appendStr += "    " + string(msgBytes)
		if i < len(msgs)-1 {
			appendStr += ",\n"
		}
	}
	appendStr += "\n  ]\n}"

	// Truncate at ']' position, then write new content
	if err := f.Truncate(bracketBytePos); err != nil {
		return err
	}
	_, err = f.WriteAt([]byte(appendStr), bracketBytePos)

	// Update messageCount in file header
	cd.updateChatFileHeader()

	return err
}

// updateChatFileHeader updates messageCount and downloadedAt in the JSON header without rewriting.
func (cd *ChatDownloader) updateChatFileHeader() {
	info, err := os.Stat(cd.outputPath)
	if err != nil || info.Size() < 50 {
		return
	}

	f, err := os.OpenFile(cd.outputPath, os.O_RDWR, 0o644)
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

	// Replace messageCount value
	mcStart := strings.Index(header, `"messageCount":`)
	if mcStart >= 0 {
		mcValStart := mcStart + len(`"messageCount":`)
		for mcValStart < len(header) && (header[mcValStart] == ' ' || header[mcValStart] == '\t') {
			mcValStart++
		}
		mcValEnd := mcValStart
		for mcValEnd < len(header) && header[mcValEnd] >= '0' && header[mcValEnd] <= '9' {
			mcValEnd++
		}
		if mcValEnd > mcValStart {
			newVal := strconv.Itoa(cd.totalCount)
			header = header[:mcValStart] + newVal + header[mcValEnd:]
		}
	}

	// Replace downloadedAt value (matches TS: header.replace(/"downloadedAt": "[^"]*"/, ...))
	replaceQuotedField(&header, `"downloadedAt":`, time.Now().UTC().Format(time.RFC3339))

	updatedBytes := []byte(header)
	if len(updatedBytes) == n {
		f.WriteAt(updatedBytes, 0)
	}
}

// replaceQuotedField replaces a JSON string value in a header buffer.
// Finds `"key": "old_value"` and replaces with `"key": "new_value"`.
func replaceQuotedField(header *string, key, newValue string) {
	h := *header
	keyIdx := strings.Index(h, key)
	if keyIdx < 0 {
		return
	}
	// Find opening quote after key
	valStart := keyIdx + len(key)
	for valStart < len(h) && h[valStart] != '"' {
		valStart++
	}
	if valStart >= len(h) {
		return
	}
	valStart++ // skip opening quote
	// Find closing quote
	valEnd := valStart
	for valEnd < len(h) && h[valEnd] != '"' {
		valEnd++
	}
	if valEnd >= len(h) {
		return
	}
	*header = h[:valStart] + newValue + h[valEnd:]
}

// pruneDedup trims the seenIDs set to keep only the most recent entries.
func (cd *ChatDownloader) pruneDedup() {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.seenOrder) <= chatDedupMax {
		return
	}
	keep := chatDedupMax
	removeIDs := cd.seenOrder[:len(cd.seenOrder)-keep]
	for _, id := range removeIDs {
		delete(cd.seenIDs, id)
	}
	cd.seenOrder = cd.seenOrder[len(cd.seenOrder)-keep:]
}

// MessageCount returns the total number of messages collected.
func (cd *ChatDownloader) MessageCount() int {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.totalCount
}

// IsRunning returns whether the downloader is currently running.
func (cd *ChatDownloader) IsRunning() bool {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.running
}

// Stop cancels the chat download.
func (cd *ChatDownloader) Stop() {
	cd.mu.Lock()
	cd.running = false
	cd.mu.Unlock()
}

// MarkStreamEnded signals that the stream has ended.
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.Stop()
}

// parseIRCTags parses IRC tags from a string like "key=value;key2=value2".
func parseIRCTags(s string) map[string]string {
	tags := make(map[string]string)
	if s == "" {
		return tags
	}
	for _, pair := range strings.Split(s, ";") {
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			tags[pair[:idx]] = pair[idx+1:]
		} else {
			tags[pair] = ""
		}
	}
	return tags
}

// parseBadges parses badge strings like "subscriber/12,moderator/1".
func parseBadges(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// parseEmoteTags parses IRC emote tags like "id:start-end,start-end/id:start-end".
func parseEmoteTags(emotesStr, message string) []TwitchEmoteRef {
	if emotesStr == "" {
		return nil
	}

	var refs []TwitchEmoteRef
	msgRunes := []rune(message)

	for _, group := range strings.Split(emotesStr, "/") {
		colonIdx := strings.IndexByte(group, ':')
		if colonIdx < 0 {
			continue
		}
		emoteID := group[:colonIdx]
		positions := group[colonIdx+1:]

		for _, pos := range strings.Split(positions, ",") {
			dashIdx := strings.IndexByte(pos, '-')
			if dashIdx < 0 {
				continue
			}
			start, err1 := strconv.Atoi(pos[:dashIdx])
			end, err2 := strconv.Atoi(pos[dashIdx+1:])
			if err1 != nil || err2 != nil {
				continue
			}

			name := ""
			if start >= 0 && end < len(msgRunes) {
				name = string(msgRunes[start : end+1])
			}

			refs = append(refs, TwitchEmoteRef{
				ID:    emoteID,
				Name:  name,
				Start: start,
				End:   end,
			})
		}
	}

	return refs
}
