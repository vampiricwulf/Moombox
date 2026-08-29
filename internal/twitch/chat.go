package twitch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	chatMaxConsecutiveErrs = 20
	chatDedupMax           = 5000
	chatSaveInterval       = 1 * time.Second
	// ircReadDeadline bounds each conn.Read in runIRCSession. Twitch sends
	// PING every ~5 min; this gives us one missed heartbeat plus slack
	// before we treat the socket as dead and trigger the reconnect path.
	ircReadDeadline = 6 * time.Minute
)

// ChatDownloader connects to Twitch IRC and records live chat messages.
type ChatDownloader struct {
	mu sync.Mutex
	// flushMu serializes flush() — it's called from the session goroutine
	// (reconnect/exit paths) and from the periodic flusher goroutine; two
	// interleaved flushes would snapshot overlapping batches and
	// double-append them to the chat file.
	flushMu        sync.Mutex
	channelLogin   string
	channelDisplay string
	channelID      string
	streamID       string
	// credentials returns the CURRENT Twitch OAuth token AND the account name
	// it belongs to, re-read on every IRC reconnect.
	//
	// A getter and not captured strings because this downloader lives for the
	// whole stream: a credential that rotates, dies, or is re-imported mid-job
	// would otherwise never be picked up, and the failure is SILENT — the
	// handshake falls through to the anonymous justinfan login (see
	// runIRCSession), which keeps capturing chat minus subscriber-only
	// messages and badges.
	//
	// ONE getter returning BOTH halves, not two getters. The handshake is a
	// single authenticated-or-anonymous decision over the pair
	// (ircHandshakeLines), so the two values must describe one session: two
	// calls could straddle a concurrent jar Reload and hand the handshake one
	// account's token beside another's login. The atomicity is provided by
	// CookieJar.GetTwitchCredentials' single RLock; this field's job is to keep
	// it a single call all the way to the wire. nil-safe via
	// sessionCredentials, which is also where the anonymous fallback is
	// applied.
	credentials func() (token, login string)
	// authRefused latches the one-shot anonymous fallback. See
	// noteHandshakeOutcome.
	authRefused atomic.Bool
	// warnedNoLogin latches noteMissingLogin's single Warn for the life of this
	// downloader. A SEPARATE flag from authRefused on purpose: that one also
	// makes sessionCredentials return an empty pair, and this condition must
	// change nothing about what goes on the wire.
	warnedNoLogin atomic.Bool
	// recordingStartMs is the OffsetMs base for the CURRENT part file.
	// Atomic: the IRC session goroutine reads it per message while RollFile
	// rebases it at part boundaries from the orchestrator goroutine.
	recordingStartMs atomic.Int64
	streamStartTime  string
	streamStartMs    int64
	messages         []TwitchChatMessage // Unwritten messages in memory
	dedup            *utils.OrderedDedup[string]
	outputPath       string
	running          bool
	streamEnded      bool  // set by MarkStreamEnded — distinguishes drain from interruption
	totalCount       int   // cumulative across all part files (job-level metric)
	fileCount        int   // messages belonging to the CURRENT part file (header count)
	lastTimestampMs  int64 // Last message timestamp (epoch ms) for resume state
	flushedToDisk    bool
	emoteResolver    *EmoteResolver
	// emoteData caches the third-party emote resolve so multi-part jobs hit
	// the 7TV/BTTV/FFZ APIs once, not once per part. Guarded by emoteMu,
	// which is held across the resolve itself to single-flight concurrent
	// callers (background part mux + stream-end drain).
	emoteMu   sync.Mutex
	emoteData *TwitchEmoteData

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// sessionCancel aborts the in-flight IRC session's I/O (set by
	// runIRCSession for its lifetime, guarded by mu). Stop/MarkStreamEnded
	// fire it so a session parked in a quiet-channel read (up to
	// ircReadDeadline) reacts immediately instead of minutes later.
	sessionCancel context.CancelFunc

	// onProgress is read from addMessage under onProgressMu; callers must
	// use SetOnProgress rather than direct field assignment to avoid a
	// data race if the callback is reassigned after Start (audit
	// reports/worker.md F3).
	onProgressMu sync.RWMutex
	onProgress   func(count int)
}

// SetOnProgress installs the progress callback. Safe to call before or
// after Start — the IRC session goroutine reads via callOnProgress under
// the same lock.
func (cd *ChatDownloader) SetOnProgress(fn func(count int)) {
	cd.onProgressMu.Lock()
	cd.onProgress = fn
	cd.onProgressMu.Unlock()
}

// callOnProgress snapshots the current progress callback under the lock and
// invokes it outside the lock so a slow callback doesn't block a concurrent
// SetOnProgress.
func (cd *ChatDownloader) callOnProgress(count int) {
	cd.onProgressMu.RLock()
	fn := cd.onProgress
	cd.onProgressMu.RUnlock()
	if fn != nil {
		fn(count)
	}
}

// ChatDownloaderOptions configures the chat downloader.
type ChatDownloaderOptions struct {
	ChannelLogin   string
	ChannelDisplay string
	ChannelID      string
	StreamID       string
	// Credentials returns the CURRENT OAuth token AND the account name it
	// belongs to, re-read per reconnect. One getter for both halves so the
	// pair cannot be torn by a concurrent cookie reload; nil, or either half
	// empty, means anonymous. See ChatDownloader.credentials.
	Credentials     func() (token, login string)
	OutputPath      string
	StreamStartTime string
	EmoteResolver   *EmoteResolver
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
		credentials:     opts.Credentials,
		outputPath:      opts.OutputPath,
		streamStartTime: opts.StreamStartTime,
		streamStartMs:   streamStartMs,
		dedup:           utils.NewOrderedDedup[string](),
		emoteResolver:   opts.EmoteResolver,
		logger:          logger,
	}
}

// SetRecordingStartTime sets the recording start time for offset calculation.
// Should be called before Start() when the actual recording begins.
func (cd *ChatDownloader) SetRecordingStartTime(isoString string) {
	if t, err := time.Parse(time.RFC3339, isoString); err == nil {
		cd.recordingStartMs.Store(t.UnixMilli())
	}
}

// chatResumePath returns the resume-state sidecar path for a chat file. The
// suffix lives here exclusively — RollFile clears the CLOSED part's sidecar
// by the same rule that getResumeFilePath derives the current one.
func chatResumePath(chatPath string) string {
	return chatPath + ".resume.json"
}

// currentOutputPath snapshots the current part's chat path under cd.mu —
// required anywhere outside flushMu, because RollFile swaps outputPath from
// the orchestrator goroutine.
func (cd *ChatDownloader) currentOutputPath() string {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	return cd.outputPath
}

// sessionCredentials reads the credentials for ONE handshake: the live pair,
// unless the anonymous fallback has latched.
//
// Returns ("", "") when no getter was supplied — tests and cookieless installs
// construct the downloader without credentials, and an empty pair is the
// anonymous-login signal ircHandshakeLines already handles. It is also what a
// latched fallback returns, so the latch needs no separate branch at the
// handshake: "we have no usable credentials" and "Twitch refused the ones we
// have" produce the same wire bytes by construction.
func (cd *ChatDownloader) sessionCredentials() (token, login string) {
	if cd.credentials == nil || cd.authRefused.Load() {
		return "", ""
	}
	return cd.credentials()
}

// noteMissingLogin reports the one anonymous handshake nothing else can see:
// an auth-token with no login cookie beside it.
//
// Every other way chat ends up anonymous is either visible or benign. No
// credentials at all is the ordinary cookieless install. A login Twitch
// REFUSES produces noteHandshakeOutcome's Warn. But a token with no login
// never attempts the authenticated handshake at all — ircHandshakeLines
// renders the full anonymous pair, because a token beside the justinfan
// nickname is the hybrid Twitch rejects — so there is no refusal to observe,
// nothing logs, and the operator-facing predicates disagree with reality:
// HasTwitchAuthCookies reads true (the token IS there) and both UIs show
// green, while the capture quietly drops every subscriber-only message and
// badge for the whole job. A minimal hand-written cookies.txt carrying only
// auth-token lands exactly here on day one, and mergeCookieFiles can
// manufacture it later by pruning an expired login row.
//
// It is reported HERE rather than by the jar's auth predicates, and that split
// is the point: this site knows the handshake actually went anonymous, whereas
// twitchAuthCookieNames could only know a name is absent — and making that
// list say so fires the auth-loss alarm on installs whose login cookie may
// never have meant anything. See twitchAuthCookieNames for that trace.
//
// ONCE PER DOWNLOADER, not per session. The condition is a property of the
// cookie file, so a job that reconnects hourly for three days would otherwise
// repeat this line hourly for three days and bury the rest of its log. The
// cost if the cookies are repaired mid-job: nothing is said when the next
// reconnect starts authenticating properly, which is a silence about GOOD news.
//
// Neither the token nor the login is named — this line names neither, and
// there is nothing here to name them with.
func (cd *ChatDownloader) noteMissingLogin(token, login string) {
	// The token check is load-bearing, not defensive: without it every
	// cookieless install — most installs — would get this warning about
	// credentials it never had.
	if token == "" || login != "" || cd.warnedNoLogin.Swap(true) {
		return
	}
	cd.logger.Warn("twitch chat: auth-token present but no login cookie — chat will be captured "+
		"anonymously (subscriber-only messages and badges will be missing); re-export cookies "+
		"from a signed-in browser",
		"channel", cd.channelLogin)
}

// noteHandshakeOutcome runs at the end of every session that presented
// credentials and decides whether to fall back to anonymous chat for the rest
// of the job.
//
// The floor this restores: before the account nickname was sent at all, EVERY
// install used the anonymous handshake, which Twitch always accepts. An
// authenticated handshake is new, and if Twitch refuses it — a stale login
// cookie, a web-session token tmi will not take — nothing here parses the
// refusal, the socket closes, and Start's reconnect loop burns all ten attempts
// on a login that cannot succeed and then abandons chat for the whole job. A
// working degraded capture would become no capture at all. So: authenticated if
// Twitch accepts it, anonymous if it does not, never nothing.
//
// The trigger is the ABSENCE of RPL_WELCOME (numeric 001) on a session that
// sent real credentials AND heard something back. Not "no chat arrived" — a
// quiet channel produces none for minutes. Not the NOTICE text alone — 001
// covers every way a login can be refused, including a torn credential pair,
// while the NOTICE only names the two cases Twitch happens to spell out.
//
// heardFromServer is the second half of that trigger and it separates a REFUSAL
// from a DROP. Twitch answers a refused login with a NOTICE before closing —
// the documented shape, and what references/chatterino7 relies on — so a
// refusal always arrives with at least one inbound line. A session that read
// NOTHING at all learned nothing about the login: the socket died, and the
// credentials are as unproven as before it opened.
//
// That distinction is load-bearing rather than fastidious.
// orchestrator_twitch.go relaunches startChat() on this same downloader the
// moment a connectivity outage is declared over — precisely when the link is
// least trustworthy — so without this bit a single unlucky reconnect on a
// marathon stream would latch subscriber-only chat off for the remaining days,
// on evidence that never mentioned the login.
//
// Cost if wrong: a Twitch that refuses SILENTLY — closing with no NOTICE, which
// is undocumented and not the observed behaviour — reads as a drop, and the job
// keeps spending its reconnect budget on a credentialed login that cannot
// succeed. That is the pre-fallback outcome, for that one hypothetical case
// only, traded against a real and reachable path.
//
// ONE-SHOT per job: once anonymous, the job stays anonymous. Flapping between
// the two would re-pay the rejected handshake on every reconnect, which is the
// cost this exists to bound. Cost if wrong: a cookie repaired mid-job does not
// re-authenticate chat until the next job — exactly the behaviour that shipped
// before the nickname existed, so a floor rather than a regression.
//
// Neither the token nor the login is named in the log line.
func (cd *ChatDownloader) noteHandshakeOutcome(welcomed, heardFromServer, sawLoginFailure bool) {
	// Order matters: a drop must leave the latch untouched, so the Swap is
	// reached only once both exits above have been ruled out.
	if welcomed || !heardFromServer || cd.authRefused.Swap(true) {
		return
	}
	if sawLoginFailure {
		cd.logger.Warn("twitch rejected the authenticated IRC login (Twitch replied that the login "+
			"failed); continuing anonymously — subscriber-only messages and badges will not be "+
			"captured for this job",
			"channel", cd.channelLogin)
		return
	}
	cd.logger.Warn("twitch never acknowledged the authenticated IRC login; continuing anonymously "+
		"— subscriber-only messages and badges will not be captured for this job",
		"channel", cd.channelLogin)
}

// getResumeFilePath returns the path to the resume state file.
func (cd *ChatDownloader) getResumeFilePath() string {
	return chatResumePath(cd.currentOutputPath())
}

// loadResumeState loads the resume state from disk.
// Returns nil if no valid resume state exists for the current stream.
//
// Both the IRC and VOD paths share the exported ChatResumeState type from
// types.go — the previously separate `twitchChatResumeState` mirror was
// dropped (audit-finding twitch.md #43). LastOffsetSeconds is unused on the
// IRC side and serializes as 0; LastTimestampMs is unused on the VOD side.
func (cd *ChatDownloader) loadResumeState() *ChatResumeState {
	store := utils.ResumeStore[ChatResumeState]{Path: cd.getResumeFilePath()}
	state, err := store.Load()
	if err != nil {
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
	// Path captured in the SAME critical section as the counters — a
	// concurrent RollFile must not pair one part's counts with the other
	// part's sidecar.
	cd.mu.Lock()
	recentIDs := cd.dedup.Snapshot(0)
	state := ChatResumeState{
		MessageCount:    cd.fileCount,
		TotalCount:      cd.totalCount,
		LastTimestampMs: cd.lastTimestampMs,
		Timestamp:       time.Now().UnixMilli(),
		StreamID:        cd.streamID,
		RecentIDs:       recentIDs,
	}
	resumePath := chatResumePath(cd.outputPath)
	cd.mu.Unlock()

	store := utils.ResumeStore[ChatResumeState]{Path: resumePath}
	if err := store.Save(state); err != nil {
		cd.logger.Warn("save chat resume state", "err", err)
	}
}

// restoreResumeState applies a loaded resume snapshot to the downloader's
// counters and dedup set. fileCount continues the current part file's count;
// totalCount falls back to MessageCount for states written before
// part-splitting existed (one file meant the file count WAS the total).
//
// flushedToDisk is restored only when the chat file actually exists. A
// resume state without its file happens when a part was rolled and the
// daemon stopped before any message arrived (the exit path saves state for
// the new, never-written part) — blindly marking it flushed would route the
// first flush onto the append path against a missing file, which fails,
// merge-fails, and retries forever: the part's chat would never reach disk.
func (cd *ChatDownloader) restoreResumeState(state *ChatResumeState) {
	fileExists := false
	if path := cd.currentOutputPath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			fileExists = true
		}
	}

	cd.mu.Lock()
	if fileExists {
		cd.fileCount = state.MessageCount
		cd.flushedToDisk = true
	} else {
		cd.fileCount = 0
		cd.flushedToDisk = false
	}
	cd.totalCount = max(state.TotalCount, state.MessageCount)
	cd.lastTimestampMs = state.LastTimestampMs
	cd.dedup.Restore(state.RecentIDs)
	cd.mu.Unlock()
}

// clearResumeState deletes the resume state file on successful completion.
func (cd *ChatDownloader) clearResumeState() {
	store := utils.ResumeStore[ChatResumeState]{Path: cd.getResumeFilePath()}
	if err := store.Clear(); err != nil {
		cd.logger.Warn("remove chat resume state", "err", err)
	}
}

// Start connects to Twitch IRC and begins recording chat messages.
// C1: Includes reconnect logic with exponential backoff on error limit.
//
// Start is safe to call once per ChatDownloader instance. Calling Start
// concurrently (or after a previous Start returns) returns an error rather
// than racing on the dedup/resume state — the struct retains seenIDs and
// seenOrder across calls, and re-initialising them while a previous session
// is still draining would drop messages.
func (cd *ChatDownloader) Start(ctx context.Context) error {
	cd.mu.Lock()
	if cd.running {
		cd.mu.Unlock()
		return fmt.Errorf("twitch chat downloader already running for %s", cd.channelLogin)
	}
	cd.running = true
	alreadyInitialized := cd.totalCount > 0 || cd.dedup.Len() > 0
	cd.mu.Unlock()

	// Try to resume from saved state (matches TS start() resume logic).
	// Only load resume state on a fresh Start — if the downloader already
	// has in-memory state from a prior session, preserve it rather than
	// replacing with the on-disk snapshot.
	if !alreadyInitialized {
		if resumeState := cd.loadResumeState(); resumeState != nil && len(resumeState.RecentIDs) > 0 {
			cd.restoreResumeState(resumeState)
			cd.logger.Info("[TwitchChat] Resuming from saved state",
				"fileMessages", resumeState.MessageCount, "totalMessages", cd.MessageCount())
		}
	}

	defer func() {
		panicked := false
		if r := recover(); r != nil {
			cd.logger.Error("chat downloader panic", "panic", r)
			panicked = true
		}

		cd.mu.Lock()
		cd.running = false
		streamEnded := cd.streamEnded
		cd.mu.Unlock()
		cd.flush()

		if panicked {
			// Don't clear resume state on panic — allow resume on restart
			return
		}

		if !streamEnded {
			// Interrupted exit (Stop() on shutdown/user-cancel, ctx
			// cancellation, reconnect exhaustion) — NOT the end of the
			// stream. Preserve resume state so the resumed session appends
			// to chat.json instead of rewriting it from scratch (clearing
			// here used to destroy all previously archived chat), and skip
			// emote enrichment: enriched files must not receive appends.
			cd.saveResumeState()
			return
		}

		// Stream-over drain: clear resume state
		cd.clearResumeState()

		// Inject third-party emotes (7TV, BTTV, FFZ) after final flush.
		// Use a fresh context -- the original ctx may already be cancelled.
		// Cached resolve: parts rolled earlier in the job already resolved
		// the emote set, so the final part reuses it. flushedToDisk gates
		// the case where every message landed in earlier parts and the
		// final part's file was never created.
		cd.mu.Lock()
		finalFileExists := cd.flushedToDisk
		total := cd.totalCount
		cd.mu.Unlock()
		if total > 0 && finalFileExists && cd.emoteResolver != nil && cd.channelID != "" {
			cd.logger.Info("resolving emotes for Twitch chat", "channelID", cd.channelID)
			emoteCtx, emoteCancel := context.WithTimeout(context.Background(), 30*time.Second)
			emoteData := cd.resolveEmotesCached(emoteCtx)
			emoteCancel()
			if emoteData != nil {
				if err := EnrichWithEmotes(cd.currentOutputPath(), emoteData); err != nil {
					cd.logger.Warn("emote injection failed", "err", err)
				}
			}
		}
	}()

	// C1: Reconnect loop -- on error limit, save state and reconnect.
	// reconnectAttempts is reset after any session that stayed connected for
	// longer than reconnectResetUptime. Long-running (8+ hour) streams
	// previously exhausted the counter on sparse network hiccups and then
	// gave up chat for the remainder of the stream.
	const (
		maxReconnects        = 10
		reconnectResetUptime = 5 * time.Minute
	)
	reconnectAttempts := 0

	for reconnectAttempts <= maxReconnects {
		if ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		if reconnectAttempts > 0 {
			// Exponential backoff: 1000 * 2^attempts, capped at 30s (matches TypeScript)
			shift := min(reconnectAttempts, 15) // cap shift to prevent overflow
			delayMs := min(1000*(1<<shift), 30000)
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

		sessionStart := time.Now()
		err := cd.runIRCSession(ctx)
		sessionUptime := time.Since(sessionStart)

		if err == nil || ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		// Session stayed connected long enough to be considered healthy —
		// treat the disconnect as an isolated hiccup and reset the counter.
		if sessionUptime >= reconnectResetUptime && reconnectAttempts > 0 {
			cd.logger.Info("IRC session was stable before disconnect; resetting reconnect counter",
				"channel", cd.channelLogin, "uptime", sessionUptime)
			reconnectAttempts = 0
		}

		reconnectAttempts++
		cd.logger.Warn("IRC session error, will reconnect", "err", err, "channel", cd.channelLogin)
	}

	return fmt.Errorf("exceeded max IRC reconnects for %s", cd.channelLogin)
}

func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()
	defer cd.mu.Unlock()

	if !cd.dedup.Add(msg.ID) {
		return
	}
	// Prune at 2× threshold to amortize the Keep cost across inserts.
	if cd.dedup.Len() > chatDedupMax*2 {
		cd.dedup.Keep(chatDedupMax)
	}

	// OffsetMs is computed HERE, under the same lock RollFile holds to swap
	// the output file and rebase recordingStartMs — guaranteeing a message's
	// offset base always matches the part file it gets flushed into.
	// Computing it at parse time (outside the lock) let a message parsed
	// just before a roll land in the NEW part with an OLD-base offset,
	// replaying hours out of position.
	baseMs := cd.recordingStartMs.Load()
	if baseMs == 0 {
		baseMs = cd.streamStartMs
	}
	if baseMs > 0 {
		msg.OffsetMs = max(msg.TimestampMs-baseMs, 0)
	}

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	cd.fileCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}

	cd.callOnProgress(cd.totalCount)
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

// interruptSession cancels the in-flight IRC session's I/O. Without it,
// Stop/MarkStreamEnded only flip flags that the session goroutine checks
// BETWEEN reads — on a chat-quiet channel the goroutine sits inside
// conn.Read for up to ircReadDeadline (6 min; Twitch PINGs every ~5), far
// past the orchestrator's chatWaitTimeout, so the stream-end drain (final
// flush + emote enrichment) used to lose the race against the final part's
// chat-file copy.
func (cd *ChatDownloader) interruptSession() {
	cd.mu.Lock()
	cancel := cd.sessionCancel
	cd.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Stop cancels the chat download.
func (cd *ChatDownloader) Stop() {
	cd.mu.Lock()
	cd.running = false
	cd.mu.Unlock()
	cd.interruptSession()
}

// MarkStreamEnded signals that the upstream live stream has ended and the
// chat downloader should drain. Unlike Stop (an interruption — shutdown or
// user cancel — after which the session must be resumable), MarkStreamEnded
// is a CLEAN end: Start's exit path clears the resume state and runs emote
// enrichment only on this flavour of shutdown. Audit-finding twitch.md #45 /
// full-project review 2026-06-09 (resume state destroyed on restart).
func (cd *ChatDownloader) MarkStreamEnded() {
	cd.mu.Lock()
	cd.streamEnded = true
	cd.running = false
	cd.mu.Unlock()
	cd.interruptSession()
}
