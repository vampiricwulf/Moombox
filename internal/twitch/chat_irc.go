package twitch

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// runIRCSession runs a single IRC connection session.
func (cd *ChatDownloader) runIRCSession(ctx context.Context) error {
	cd.logger.Info("connecting to twitch IRC", "channel", cd.channelLogin)

	conn, _, err := websocket.Dial(ctx, constants.TwitchURLs.IRCWS, nil)
	if err != nil {
		return fmt.Errorf("connect IRC: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	conn.SetReadLimit(512 * 1024) // 512KB cap on incoming IRC messages

	// Authenticate
	if cd.authToken != "" {
		if err := conn.Write(ctx, websocket.MessageText, []byte("PASS oauth:"+cd.authToken)); err != nil {
			return fmt.Errorf("IRC PASS failed: %w", err)
		}
	} else {
		if err := conn.Write(ctx, websocket.MessageText, []byte("PASS SCHMOOPIIE")); err != nil {
			return fmt.Errorf("IRC PASS failed: %w", err)
		}
	}
	nick := fmt.Sprintf("justinfan%d", rand.Intn(100000))
	if err := conn.Write(ctx, websocket.MessageText, []byte("NICK "+nick)); err != nil {
		return fmt.Errorf("IRC NICK failed: %w", err)
	}

	// Request capabilities
	if err := conn.Write(ctx, websocket.MessageText, []byte("CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership")); err != nil {
		return fmt.Errorf("IRC CAP REQ failed: %w", err)
	}

	// Join channel
	if err := conn.Write(ctx, websocket.MessageText, []byte("JOIN #"+strings.ToLower(cd.channelLogin))); err != nil {
		return fmt.Errorf("IRC JOIN failed: %w", err)
	}

	cd.logger.Info("joined twitch IRC", "channel", cd.channelLogin)

	// Message-triggered flush: idle until a message arrives, then collect
	// for chatSaveInterval before flushing. No I/O during quiet periods.
	var flushTimer *time.Timer
	var flushCh <-chan time.Time // nil channel is never ready in select
	defer func() {
		if flushTimer != nil {
			flushTimer.Stop()
		}
	}()

	consecutiveErrors := 0

	for cd.IsRunning() {
		select {
		case <-ctx.Done():
			return nil
		case <-flushCh:
			cd.flush()
			flushTimer = nil
			flushCh = nil
		default:
		}

		// Start flush timer when the first pending message arrives.
		// Subsequent messages within chatSaveInterval are batched together.
		// Placed before conn.Read so error-path `continue` still triggers it.
		if flushTimer == nil {
			cd.mu.Lock()
			hasPending := len(cd.messages) > 0
			cd.mu.Unlock()
			if hasPending {
				flushTimer = time.NewTimer(chatSaveInterval)
				flushCh = flushTimer.C
			}
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

		lines := strings.SplitSeq(string(data), "\r\n")
		for line := range lines {
			if line == "" {
				continue
			}

			// Handle PING
			if strings.HasPrefix(line, "PING") {
				if err := conn.Write(ctx, websocket.MessageText, []byte("PONG :tmi.twitch.tv")); err != nil {
					cd.logger.Warn("IRC PONG write failed", "err", err)
				}
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
		var ok bool
		tagsStr, rest, ok = strings.Cut(line[1:], " ")
		if !ok {
			return nil
		}
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
		messageText = strings.TrimPrefix(messageText, ":")
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
		offsetMs = max(tmiSentTs-baseMs, 0)
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
		messageText = strings.TrimPrefix(messageText, ":")
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
		offsetMs = max(tmiSentTs-baseMs, 0)
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

// parseIRCTags parses IRC tags from a string like "key=value;key2=value2".
func parseIRCTags(s string) map[string]string {
	tags := make(map[string]string, 16)
	if s == "" {
		return tags
	}
	for pair := range strings.SplitSeq(s, ";") {
		key, value, ok := strings.Cut(pair, "=")
		if ok {
			tags[key] = value
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

	for group := range strings.SplitSeq(emotesStr, "/") {
		emoteID, positions, ok := strings.Cut(group, ":")
		if !ok {
			continue
		}

		for pos := range strings.SplitSeq(positions, ",") {
			startStr, endStr, ok := strings.Cut(pos, "-")
			if !ok {
				continue
			}
			start, err1 := strconv.Atoi(startStr)
			end, err2 := strconv.Atoi(endStr)
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
