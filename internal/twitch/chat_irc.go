package twitch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// anonymousIRCPass is Twitch's documented password for an unauthenticated IRC
// session. It is load-bearing, not a placeholder: most installs hold no Twitch
// cookies at all, and PASS SCHMOOPIIE + NICK justinfan<random> is how they read
// public chat. It is not an error and is never logged as one.
const anonymousIRCPass = "PASS SCHMOOPIIE"

// ircRowBreakingChars are the bytes that stop a value being sendable as a
// single IRC parameter: a space truncates the nickname at the gap, a tab is not
// a legal nick character, and CR / LF / NUL split one websocket frame into two
// IRC commands.
const ircRowBreakingChars = " \t\r\n\x00"

// hasRowBreakingChar reports whether login cannot be spoken as one IRC
// parameter, and therefore is not a usable identity.
//
// ONE predicate with TWO callers, and that is the point rather than tidiness.
// ircHandshakeLines uses it to decide the session goes anonymous;
// ChatDownloader.noteMissingLogin uses it to decide that the decision is worth
// reporting. A second copy of the character set would drift, and the drift is
// silent in the direction that matters: a handshake that goes anonymous for a
// reason the report does not recognise produces no log line, no notification,
// and a whole job of chat with no subscriber-only messages in it.
func hasRowBreakingChar(login string) bool {
	return strings.ContainsAny(login, ircRowBreakingChars)
}

// ircHandshakeLines renders the PASS and NICK lines of one IRC handshake from
// one decision.
//
// The two lines are a PAIR, and returning both from a single function is the
// point rather than a convenience. Twitch binds an IRC session to the token's
// user through NICK, so the only two coherent handshakes are:
//
//   - authenticated: PASS oauth:<token> + NICK <login>
//   - anonymous:     PASS SCHMOOPIIE    + NICK justinfan<random>
//
// The hybrid this replaced — a real token beside the anonymous justinfan
// nickname — is refused with a `Login authentication failed` NOTICE or
// silently downgraded to an anonymous session, and nothing here parses NOTICE.
// The visible result either way is chat that connects and simply never carries
// subscriber-only messages or badges, which is why the two halves must not come
// from two conditions that can drift apart.
//
// A token with no login therefore falls all the way back to anonymous. So does
// a token beside a login that could not be sent as a single IRC parameter (see
// hasRowBreakingChar): a value that cannot be spoken as a nickname is not a
// usable identity, so it is treated as no identity at all rather than smuggled
// onto the wire. Twitch logins are ASCII word characters, so no real one is
// rejected here. Upstream shape:
// references/chatterino7/src/providers/twitch/TwitchIrcServer.cpp:303-332.
//
// BOTH of those routes are reported by the caller — see noteMissingLogin —
// because they are the routes to the anonymous pair that nothing else in the
// system can observe.
//
// The nick is lowercased: IRC nicknames are case-insensitive and Twitch
// expects the lowercase login.
//
// The returned `authenticated` says which of the two handshakes was rendered.
// It is the same decision, reported rather than re-derived: the caller needs it
// to know whether a session that never got RPL_WELCOME was a refused login (see
// noteHandshakeOutcome), and re-testing the credentials at that point could
// disagree with what was actually sent.
//
// Neither argument is ever logged.
func ircHandshakeLines(token, login string) (pass, nick string, authenticated bool) {
	if token != "" && login != "" && !hasRowBreakingChar(login) {
		return "PASS oauth:" + token, "NICK " + strings.ToLower(login), true
	}
	return anonymousIRCPass, fmt.Sprintf("NICK justinfan%d", rand.IntN(100000)), false
}

// ircCommandOf returns an IRC line's command word — "001", "NOTICE",
// "PRIVMSG" — and everything after it, skipping IRCv3 tags and the sender
// prefix. Both are "" for a line with no command.
//
// Deliberately minimal. It exists for the two facts noteHandshakeOutcome needs
// and nothing else; the message parser proper is parseLine.
func ircCommandOf(line string) (cmd, params string) {
	if after, ok := strings.CutPrefix(line, "@"); ok {
		_, rest, found := strings.Cut(after, " ")
		if !found {
			return "", ""
		}
		line = rest
	}
	if strings.HasPrefix(line, ":") {
		_, rest, found := strings.Cut(line, " ")
		if !found {
			return "", ""
		}
		line = rest
	}
	cmd, params, _ = strings.Cut(line, " ")
	return cmd, params
}

// ircIsWelcome reports whether a line is RPL_WELCOME, the numeric Twitch sends
// once it has accepted the login. Its arrival is the only positive proof the
// handshake was taken; its absence is what noteHandshakeOutcome acts on.
func ircIsWelcome(line string) bool {
	cmd, _ := ircCommandOf(line)
	return cmd == "001"
}

// ircIsLoginFailureNotice recognises the two NOTICE texts Twitch sends when it
// refuses a login. This is NOT a general NOTICE parser and must not become one:
// it exists only so the fallback's single Warn can say Twitch spoke, rather
// than merely that nothing arrived. The command is checked so a chat message
// quoting either phrase cannot masquerade as one.
func ircIsLoginFailureNotice(line string) bool {
	cmd, params := ircCommandOf(line)
	if cmd != "NOTICE" {
		return false
	}
	return strings.Contains(params, "Login authentication failed") ||
		strings.Contains(params, "Login unsuccessful")
}

// runIRCSession runs a single IRC connection session.
func (cd *ChatDownloader) runIRCSession(ctx context.Context) error {
	cd.logger.Info("connecting to twitch IRC", "channel", cd.channelLogin)

	// sessionCtx scopes ALL of this session's I/O — dial, reads, and writes
	// (handshake + PONG) — so Stop/MarkStreamEnded can abort a blocked
	// operation immediately via interruptSession, not just the read loop.
	// The PARENT ctx checks below stay on ctx — a session cancel is not a
	// caller cancel; the loop notices it through IsRunning and exits cleanly
	// into the drain path.
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	cd.mu.Lock()
	cd.sessionCancel = sessionCancel
	cd.mu.Unlock()
	defer func() {
		cd.mu.Lock()
		cd.sessionCancel = nil
		cd.mu.Unlock()
	}()

	conn, _, err := websocket.Dial(sessionCtx, constants.TwitchURLs.IRCWS, nil)
	if err != nil {
		return fmt.Errorf("connect IRC: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	conn.SetReadLimit(512 * 1024) // 512KB cap on incoming IRC messages

	// Authenticate. The credentials are read HERE, per session, so a reconnect
	// that happens hours into a stream presents whatever the jar holds now
	// rather than what was captured at construction — and they arrive from ONE
	// call, so the token and the login always describe the same session even if
	// a Reload lands mid-handshake on another goroutine. ONE more call turns
	// them into both wire lines, so the two can never disagree about whether
	// this session is authenticated.
	token, login := cd.sessionCredentials()
	pass, nick, authenticated := ircHandshakeLines(token, login)
	// Beside the handshake decision, never inside it: ircHandshakeLines is pure
	// and logs nothing, and the anonymous pair it just rendered is correct for
	// this input. What is worth saying is that ONE of the ways to reach that
	// pair is invisible everywhere else. See noteMissingLogin.
	cd.noteMissingLogin(token, login)
	if err := conn.Write(sessionCtx, websocket.MessageText, []byte(pass)); err != nil {
		return fmt.Errorf("IRC PASS failed: %w", err)
	}
	if err := conn.Write(sessionCtx, websocket.MessageText, []byte(nick)); err != nil {
		return fmt.Errorf("IRC NICK failed: %w", err)
	}

	// A credentialed session that ends having HEARD from Twitch but never
	// received RPL_WELCOME had its login refused; fall back to anonymous for the
	// rest of the job rather than spending the reconnect budget on a handshake
	// that cannot succeed. A session that read nothing at all is a dropped
	// socket, not a verdict on the credentials — see noteHandshakeOutcome, which
	// owns both halves of that rule. Skipped entirely when WE ended the session
	// — a caller cancel or a Stop/MarkStreamEnded is not a refusal, and neither
	// is the immediate return below when the downloader was never started.
	welcomed := false
	heardFromServer := false
	sawLoginFailure := false
	if authenticated {
		defer func() {
			if ctx.Err() != nil || !cd.IsRunning() {
				return
			}
			cd.noteHandshakeOutcome(welcomed, heardFromServer, sawLoginFailure)
		}()
	}

	// Request capabilities
	if err := conn.Write(sessionCtx, websocket.MessageText, []byte("CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership")); err != nil {
		return fmt.Errorf("IRC CAP REQ failed: %w", err)
	}

	// Join channel
	if err := conn.Write(sessionCtx, websocket.MessageText, []byte("JOIN #"+strings.ToLower(cd.channelLogin))); err != nil {
		return fmt.Errorf("IRC JOIN failed: %w", err)
	}

	cd.logger.Info("joined twitch IRC", "channel", cd.channelLogin)

	// Flush on a dedicated ticker goroutine: the read loop below blocks in
	// conn.Read for up to ircReadDeadline (6 minutes), so a flush serviced
	// from the loop itself (the previous design) left pending chat unflushed
	// — and the resume state unsaved — for the whole quiet period in a slow
	// channel. flush() is serialized via cd.flushMu, so the ticker is safe
	// alongside the reconnect/exit-path flush calls. No I/O happens on ticks
	// with nothing pending.
	flusherDone := make(chan struct{})
	defer close(flusherDone)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cd.logger.Error("chat flusher panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(chatSaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-flusherDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				cd.mu.Lock()
				hasPending := len(cd.messages) > 0
				cd.mu.Unlock()
				if hasPending {
					cd.flush()
				}
			}
		}
	}()

	consecutiveErrors := 0

	for cd.IsRunning() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Read with a per-read deadline so a silent socket (e.g. NAT
		// dropping the connection mid-stream) triggers a reconnect
		// instead of blocking until the parent context cancels. Twitch
		// IRC sends a PING every ~5 minutes; readDeadline covers two
		// missed PINGs before we give up on the session. Derived from
		// sessionCtx so Stop/MarkStreamEnded unblock the read at once.
		readCtx, readCancel := context.WithTimeout(sessionCtx, ircReadDeadline)
		_, data, err := conn.Read(readCtx)
		readCancel()
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

			// The server said SOMETHING. Recorded for every line and
			// before any of them is interpreted, because the question this
			// answers is only "did Twitch talk to us at all" — a session
			// that heard nothing cannot have been refused, it was dropped.
			// Deliberately not narrowed to the refusal NOTICE: a wording
			// change there would silently turn every refusal back into a
			// whole-job chat loss.
			heardFromServer = true

			// Handshake outcome. Only tracked for a credentialed session —
			// the anonymous one is always accepted and has no fallback to
			// take — and only until 001 lands, after which nothing can
			// change the verdict.
			if authenticated && !welcomed {
				if ircIsWelcome(line) {
					welcomed = true
				} else if ircIsLoginFailureNotice(line) {
					sawLoginFailure = true
				}
			}

			// Handle PING
			if strings.HasPrefix(line, "PING") {
				if ctx.Err() != nil {
					// Caller is shutting us down — skip the write
					// rather than racing on a cancelled conn.
					return nil
				}
				if err := conn.Write(sessionCtx, websocket.MessageText, []byte("PONG :tmi.twitch.tv")); err != nil {
					cd.logger.Warn("IRC PONG write failed", "err", err)
				}
				continue
			}

			// Twitch occasionally issues RECONNECT to request clients
			// drop and reconnect. Return nil so the outer loop does a
			// clean reconnect without incrementing the error counter.
			if strings.HasPrefix(line, "RECONNECT") {
				cd.logger.Info("twitch IRC RECONNECT received; reconnecting", "channel", cd.channelLogin)
				return nil
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

	// OffsetMs is computed in addMessage under cd.mu (atomic with RollFile's
	// part-boundary base swap), not here.
	msg := &TwitchChatMessage{
		ID:           id,
		TimestampMs:  tmiSentTs,
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
	case "announcement":
		normalizedType = "announcement"
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

	// OffsetMs is computed in addMessage under cd.mu (atomic with RollFile's
	// part-boundary base swap), not here.
	msg := &TwitchChatMessage{
		ID:           id,
		TimestampMs:  tmiSentTs,
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
	if msgID == "announcement" {
		msg.AnnouncementColor = strings.ToLower(tags["msg-param-color"])
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
// Twitch's IRC emote-tag offsets count UTF-16 code units (mirroring JavaScript's
// string indexing), NOT Go runes. For characters outside the Basic Multilingual
// Plane (U+10000+, e.g. the 🎉 emoji) a single rune is two UTF-16 code units, so
// indexing message runes by the Twitch offsets drifts by one per non-BMP char
// already seen. The VOD path (api.go) already uses utf16Len for the same reason.
func parseEmoteTags(emotesStr, message string) []TwitchEmoteRef {
	if emotesStr == "" {
		return nil
	}

	var refs []TwitchEmoteRef
	msgUnits := utf16.Encode([]rune(message))

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
			if start >= 0 && end < len(msgUnits) {
				name = string(utf16.Decode(msgUnits[start : end+1]))
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
