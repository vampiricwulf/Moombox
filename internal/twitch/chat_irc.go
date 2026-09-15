package twitch

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// ircFrameWriter sends one IRC line as a single websocket text frame, bounded
// by ctx.
//
// It is a type so ChatDownloader.keepaliveWrite can name it without chat.go
// having to import the websocket package for one field.
type ircFrameWriter func(ctx context.Context, conn *websocket.Conn, line string) error

// writeIRCFrame is the production keepaliveWrite, and the only implementation
// outside tests.
func writeIRCFrame(ctx context.Context, conn *websocket.Conn, line string) error {
	return conn.Write(ctx, websocket.MessageText, []byte(line))
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
			// reauthPending: WE cancelled this session to present new
			// credentials, so its missing 001 is not Twitch's verdict on the
			// login. Without it, a Reauthenticate landing between the CAP ACK
			// and the 001 would latch the anonymous fallback and demote the
			// very session it was trying to upgrade.
			if ctx.Err() != nil || !cd.IsRunning() || cd.reauthPending.Load() {
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

	// Client-initiated keepalive. ircReadDeadline alone leaves a HALF-OPEN
	// socket — one the OS still believes is connected — parked for six
	// minutes, and Twitch IRC has NO replay: every message in that window is
	// simply absent from the archive. So this session speaks first, the way
	// chatterino7 does
	// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
	//
	// A goroutine rather than a shorter per-read deadline, and that is a
	// property of the library rather than a preference: coder/websocket
	// installs a read context as a context.AfterFunc that CLOSES the
	// connection when it fires (setupReadTimeout, conn.go), so a 15-second
	// read deadline would kill the socket on every quiet fifteen seconds.
	// Writes are serialized inside the library, so this goroutine's PING
	// cannot interleave with the read loop's PONG.
	//
	// Both clocks below are ELAPSED MONOTONIC durations since sessionStart,
	// not wall-clock instants. The inbound stamp has to cross goroutines
	// through an atomic int64, and a time.Time that makes that trip loses its
	// monotonic reading (time.Unix carries none), so a comparison against one
	// silently falls back to the WALL clock — two different frames of
	// reference for one question, and a system clock step part-way through a
	// stream would skew the answer. Elapsed durations give one frame of
	// reference and are immune to the step.
	//
	// What they do NOT give is resolution: Windows delivers monotonic readings
	// at roughly the same ~0.5 ms granularity as its wall clock, so a PING and
	// the PONG answering it microseconds later still carry the SAME value.
	// Treating an indistinguishable frame as an answer is the `>=` below, and
	// that is what keeps a healthy connection alive.
	sessionStart := time.Now()
	var lastInbound atomic.Int64 // time.Since(sessionStart), in nanoseconds
	keepaliveFailed := make(chan struct{})
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cd.logger.Error("chat keepalive panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(cd.delays.keepaliveCheck)
		defer ticker.Stop()
		// declareDead is the ONE way this goroutine ends a session: publish the
		// verdict the read loop turns into errKeepaliveTimeout, unblock that
		// read, and only then say why. Its callers below are the same fact —
		// the IRC layer is not serving us — reached by different evidence.
		//
		// The ORDER inside is the guarantee, not a style choice. Everything
		// that ends this session also CLOSES the socket, and the read loop
		// wakes from a closed socket instantly: its next iterations are a
		// context.WithTimeout and an immediate net.ErrClosed read, so it can
		// burn all chatMaxConsecutiveErrs of them in the time it takes to
		// format one log line. It would then return "too many IRC errors",
		// which is NOT errors.Is-able to errKeepaliveTimeout — and the
		// reconnect would be charged to the budget after all, inverting the
		// one guarantee this whole mechanism exists to make. So the channel
		// closes first, the cancel second, the Warn last.
		//
		// sync.Once because two arms can reach this — the ticker's verdict and
		// the write timer's — and a second close(keepaliveFailed) would panic.
		var deadOnce sync.Once
		declareDead := func(reason string) {
			deadOnce.Do(func() {
				close(keepaliveFailed)
				sessionCancel()
				cd.logger.Warn("twitch IRC keepalive failed; reconnecting",
					"channel", cd.channelLogin, "reason", reason, "pongWait", cd.delays.keepalivePongWait)
			})
		}
		// pingSentAt is when the outstanding PING was written, or -1 when none
		// is outstanding. -1 rather than 0 because 0 is a legal elapsed value.
		pingSentAt := time.Duration(-1)
		for {
			select {
			case <-keepaliveDone:
				return
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				// time.Since rather than the tick's own timestamp: a tick
				// delivered late reports when it was SCHEDULED to fire, which
				// understates how long we have actually been waiting — and
				// waiting is the entire measurement here.
				elapsed := time.Since(sessionStart)
				last := time.Duration(lastInbound.Load())
				if pingSentAt >= 0 {
					// ANY inbound frame answers — a PONG, a chat line, a
					// server PING. The question is whether the IRC layer is
					// still serving us, not whether it used the right verb.
					//
					// >= rather than >: two events the clock cannot separate
					// are not evidence of silence, and the safe reading of an
					// ambiguous frame is that the connection is alive. A false
					// "dead" costs a reconnect and the chat in flight; a false
					// "alive" costs one more check tick.
					if last >= pingSentAt {
						pingSentAt = -1
						continue
					}
					if elapsed-pingSentAt < cd.delays.keepalivePongWait {
						continue
					}
					declareDead("no inbound frame after the keepalive PING")
					return
				}
				if elapsed-last < cd.delays.keepaliveIdle {
					continue
				}
				// Read BEFORE the write and armed only after it, for two
				// reasons — and NOT for a third that it looks like.
				//
				// It keeps the write's OWN duration inside the pong window: a
				// socket slow to accept thirteen bytes is part of what is being
				// measured, not an allowance on top of it.
				//
				// And the reply is recorded by a DIFFERENT goroutine, so on a
				// fast link the PONG can be read and stored while this one is
				// still descheduled after the write returns; a stamp taken
				// afterwards would sit later than the very frame that answers
				// it.
				//
				// What this ordering does NOT do is make the comparison safe on
				// its own. The `>=` above is what does that: Windows delivers
				// monotonic readings at roughly the same ~0.5 ms granularity as
				// its wall clock, so a PING and a PONG landing in one tick stay
				// indistinguishable however they are stamped.
				sentAt := time.Since(sessionStart)
				// The write is bounded by a timer WE own rather than by a
				// deadline on the context handed to the library, and that is
				// the whole of the ordering rule above applied to this arm.
				// coder/websocket installs a write deadline as
				// context.AfterFunc(ctx, func(){ clearWriteTimeout(); close() })
				// (conn.go:171-181), and that close tears down the underlying
				// net.Conn on ANOTHER goroutine — so a plain WithTimeout kills
				// the socket before Write returns, and the read loop is already
				// spinning on net.ErrClosed while we still have not published
				// the verdict. It would exhaust chatMaxConsecutiveErrs and
				// return "too many IRC errors" instead, which charges the
				// reconnect budget.
				//
				// So the timer declares the verdict FIRST and cancels SECOND;
				// the cancel is what closes the connection, and by then the
				// read loop's keepaliveFailed check — which sits before
				// consecutiveErrors++ — cannot lose the race.
				//
				// An unbounded write is not an option: on a half-open socket
				// with a full send buffer it parks forever and the session
				// silently falls back to the six-minute ircReadDeadline, which
				// is the failure this keepalive exists to catch.
				writeCtx, writeCancel := context.WithCancel(sessionCtx)
				writeTimer := time.AfterFunc(cd.delays.keepalivePongWait, func() {
					defer func() {
						if r := recover(); r != nil {
							cd.logger.Error("chat keepalive write-timeout panic", "panic", r)
						}
					}()
					declareDead("the keepalive PING could not be written in time")
					writeCancel()
				})
				err := cd.keepaliveWrite(writeCtx, conn, ircKeepalivePing)
				// Stop BEFORE our own cancel. A write that returned has already
				// had the library clear its deadline hook, so cancelling then
				// closes nothing; a write still in flight is only ever
				// cancelled by the timer, which published the verdict first.
				stopped := writeTimer.Stop()
				writeCancel()
				if !stopped {
					// The timer won. The verdict is published (or is being
					// published by that goroutine, which owns the same
					// sync.Once), and this session is over whatever the write
					// finally returned.
					return
				}
				if err != nil {
					// Unless WE are the reason: Stop, MarkStreamEnded and
					// Reauthenticate all cancel sessionCtx, and that reaches
					// this goroutine as a write error too. A shutdown is not
					// Twitch going quiet, and calling it one would hand the
					// read loop a verdict on a session nobody is judging.
					if sessionCtx.Err() != nil {
						return
					}
					declareDead("the keepalive PING could not be written")
					return
				}
				pingSentAt = sentAt
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

		// Read with a per-read deadline so a silent socket (e.g. NAT dropping
		// the connection mid-stream) cannot block until the parent context
		// cancels. This is the OUTER bound and nothing more: Twitch sends a
		// server PING about every 5 minutes, so six is one missed heartbeat
		// plus slack. What actually detects a half-open socket is the
		// keepalive above, within ircKeepaliveIdle + one ircKeepaliveCheck
		// tick + ircKeepalivePongWait. Derived from sessionCtx so
		// Stop/MarkStreamEnded unblock the read at once — and note that
		// coder/websocket CLOSES the connection when this context fires, which
		// is why the keepalive is a goroutine and not a shorter deadline here.
		readCtx, readCancel := context.WithTimeout(sessionCtx, ircReadDeadline)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Our own cancel. End the session now rather than spinning
			// chatMaxConsecutiveErrs failed reads against a context we
			// cancelled; Start's loop reads reauthPending and reconnects at
			// once. The error value itself is never compared against — see
			// errReauthRequested.
			if cd.reauthPending.Load() {
				return errReauthRequested
			}
			// The keepalive cancelled this session because Twitch stopped
			// answering. Returning the error (rather than counting a read
			// failure) is what makes Start's loop reconnect at once instead of
			// spinning chatMaxConsecutiveErrs reads against a cancelled
			// context — and wrapping errKeepaliveTimeout is what stops that
			// reconnect being charged to the budget. See the sentinel's doc.
			select {
			case <-keepaliveFailed:
				return fmt.Errorf("%w within %v", errKeepaliveTimeout, cd.delays.keepalivePongWait)
			default:
			}
			consecutiveErrors++
			if consecutiveErrors >= chatMaxConsecutiveErrs {
				// Return error to trigger reconnect
				return fmt.Errorf("too many IRC errors: %w", err)
			}
			continue
		}
		consecutiveErrors = 0
		// Every inbound FRAME, before any of it is interpreted: the keepalive's
		// question is whether Twitch is still talking to us at all. Elapsed
		// monotonic nanoseconds, in the same frame of reference the keepalive
		// reads them in — see sessionStart.
		lastInbound.Store(int64(time.Since(sessionStart)))

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
					// The one positive signal on this path. Twitch has accepted
					// the account login, so this session captures subscriber-only
					// messages and badges — the thing every downgrade in this
					// file is about NOT having.
					//
					// Info, and once per session rather than once per
					// downloader: a reconnect that re-authenticates after a
					// credential repair is exactly the event an operator is
					// looking for, and a per-downloader latch would hide it.
					// Names the channel and nothing else — the account name is
					// in the 001 line's parameters and must not be echoed.
					cd.logger.Info("twitch chat: authenticated login accepted", "channel", cd.channelLogin)
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

			// Twitch occasionally issues RECONNECT to ask clients to drop and
			// reconnect. The SENTINEL, not nil: nil is Start's clean-exit
			// value, so returning it ended chat capture for the rest of the
			// job. Logged at the loop rather than here, so one directive still
			// writes exactly one line — see errServerReconnect.
			if strings.HasPrefix(line, "RECONNECT") {
				// Force-closed with NO close handshake, before the sentinel is
				// returned: Twitch has already told us to leave, so waiting on
				// a peer that may not answer buys nothing, and the deferred
				// conn.Close() below would otherwise block for its full 5s
				// peer-ack timeout on every RECONNECT — a real chat gap this
				// avoids. See errServerReconnect.
				conn.CloseNow()
				return errServerReconnect
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

	// Unwrap /me BEFORE the emote tags are read. The offsets index the
	// unwrapped text — measured 2026-09-15 on real ACTION lines — so parsing
	// against the wrapped form lands every emote eight code points early and
	// renders the word "ACTION" as part of the message.
	messageText, isAction := stripActionWrapper(messageText)

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
		IsAction:     isAction,
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

// stripActionWrapper unwraps the CTCP form Twitch sends a /me message in:
// \x01ACTION <text>\x01. It returns the text and whether it was wrapped.
//
// PRIVMSG only, and only as a whole-value wrapper: the prefix must be at the
// very start, and the trailing \x01 is removed only when the prefix matched.
// A chat line that merely mentions the marker mid-text is an ordinary message,
// and a USERNOTICE body is never wrapped at all.
func stripActionWrapper(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, "\x01ACTION ")
	if !ok {
		return text, false
	}
	return strings.TrimSuffix(rest, "\x01"), true
}

// parseEmoteTags parses IRC emote tags like "id:start-end,start-end/id:start-end".
//
// TWO INDEX SPACES, and the whole point of this function is the conversion
// between them.
//
// The WIRE offsets count Unicode CODE POINTS of the PRIVMSG text — inclusive,
// zero-based. Not bytes, and NOT UTF-16 code units: this file asserted UTF-16
// from 2026-04-22 (commit 5031cd2b) until this arc, and the claim was wrong.
// It was re-measured 2026-09-15 over the raw IRC lines of 18 real archives: of
// 120 ranges preceded by a non-BMP character, 120 slice to a whole-word token
// by code point and 0 by UTF-16. Every reference client agrees —
// references/chatterino7/src/providers/twitch/TwitchIrc.cpp (codepointToUtf16Idx),
// gempir/go-twitch-irc ([]rune slicing), robotty/twitch-irc-rs
// (chars().skip().take()).
//
// The EMITTED Start/End count UTF-16 code units, because the only consumer is
// JavaScript: player.js renders the span with String.prototype.substring
// (web/public/modules/player.js, _appendTwitchMessage), and the VOD path emits
// UTF-16 already (utf16Len, api.go). Emitting the wire offsets unchanged would
// make the two producers disagree about what a chat file's offsets mean.
//
// So: read by code point, write by UTF-16, and Name comes from the code-point
// slice. For messages with no non-BMP character the two spaces coincide and
// nothing moves.
//
// A range that is inverted or runs past the end of the message is NOT a fatal
// input: the raw wire Start/End are passed through with an empty Name, exactly
// as before, so a malformed tag costs one unrendered emote rather than the
// session. The `start <= end` half of the guard is load-bearing — without it
// an inverted range slices backwards and panics inside the read loop.
func parseEmoteTags(emotesStr, message string) []TwitchEmoteRef {
	if emotesStr == "" {
		return nil
	}

	runes := []rune(message)
	// cpToUnit[i] is the UTF-16 index at which code point i begins. The extra
	// entry at len(runes) holds the message's total UTF-16 length, which is
	// what makes End computable as cpToUnit[end+1]-1 with no special case for
	// a range that ends on the last code point.
	cpToUnit := make([]int, len(runes)+1)
	units := 0
	for i, r := range runes {
		cpToUnit[i] = units
		if r >= 0x10000 {
			units += 2 // surrogate pair
		} else {
			units++
		}
	}
	cpToUnit[len(runes)] = units

	var refs []TwitchEmoteRef
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

			ref := TwitchEmoteRef{ID: emoteID, Start: start, End: end}
			if start >= 0 && start <= end && end < len(runes) {
				ref.Name = string(runes[start : end+1])
				ref.Start = cpToUnit[start]
				ref.End = cpToUnit[end+1] - 1
			}
			refs = append(refs, ref)
		}
	}

	return refs
}
