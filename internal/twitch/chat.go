package twitch

import (
	"context"
	"errors"
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

// The fixed vocabulary of ChatDownloaderOptions.OnAuthDowngrade's reason: one
// value per route from "this job HAD Twitch credentials" to "this job's chat is
// being captured anonymously".
//
// Opaque tokens for the consumer to switch on, deliberately not sentences — the
// consumer renders the operator-facing wording, and the same fact has to reach
// a Discord embed, a log line, and whatever comes next without four of them
// drifting apart.
//
// None of them carries anything read from the cookie file or off the wire. That
// is a property of the vocabulary itself and not of the caller's discipline:
// there is no format verb here to interpolate a token, a login, or a chat
// message into, so the consumer can put the reason anywhere — including a
// notification body — without a leak being possible.
const (
	// AuthDowngradeLoginRefused: Twitch answered the authenticated login with
	// one of the two refusal NOTICEs. See noteHandshakeOutcome.
	AuthDowngradeLoginRefused = "login-refused"
	// AuthDowngradeLoginUnacknowledged: Twitch spoke on the session but never
	// sent RPL_WELCOME and never named a reason. See noteHandshakeOutcome.
	AuthDowngradeLoginUnacknowledged = "login-never-acknowledged"
	// AuthDowngradeNoLoginCookie: an auth-token with no "login" cookie beside
	// it, so the authenticated handshake is never attempted at all. See
	// noteMissingLogin.
	AuthDowngradeNoLoginCookie = "no-login-cookie"
	// AuthDowngradeUnusableLoginCookie: an auth-token beside a "login" that
	// cannot be sent as an IRC nickname (see hasRowBreakingChar) — a
	// hand-edited cookies.txt carrying a display name with a space in it lands
	// here. Same silence as the case above, from a different input.
	AuthDowngradeUnusableLoginCookie = "unusable-login-cookie"
)

// errReauthRequested ends an IRC session that Reauthenticate cancelled.
//
// It exists to END THE READ LOOP on the first failed read rather than spin
// through chatMaxConsecutiveErrs of them against a context we cancelled. It is
// never compared against and never reaches a log line: Start's loop decides on
// reauthPending and `continue`s before the Warn that would print it, and the
// Info line beside that `continue` is what says what happened. Its text is
// therefore for a reader of the code, not for an operator.
var errReauthRequested = errors.New("IRC session cancelled to present refreshed credentials")

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
	// downloader's CURRENT CREDENTIAL PAIR — Reauthenticate resets it, because
	// it also gates this site's reportAuthDowngrade and a repaired file that is
	// still missing its login row has to be reported again.
	//
	// A SEPARATE flag from authRefused on purpose: that one also makes
	// sessionCredentials return an empty pair, and this condition must change
	// nothing about what goes on the wire.
	warnedNoLogin atomic.Bool
	// onAuthDowngrade reports to the OWNER of this downloader that a job which
	// HAD Twitch credentials is now capturing chat anonymously. nil-safe, and
	// fired at most once per credential pair (Reauthenticate resets the latch)
	// — see reportAuthDowngrade.
	onAuthDowngrade func(reason string)
	// downgradeReported latches onAuthDowngrade across ALL of its trigger
	// sites. A third flag, not a reuse of either above: warnedNoLogin is
	// per-site (the fallback's own Warns never touch it) and authRefused is a
	// behaviour switch rather than a report. See reportAuthDowngrade.
	downgradeReported atomic.Bool
	// reauthPending marks the window between Reauthenticate() cancelling a
	// session and Start's loop consuming that fact. Three sites read it, one
	// consumes it: runIRCSession's read-error branch ends the session at once
	// instead of burning chatMaxConsecutiveErrs failed reads against a context
	// we cancelled; its handshake-outcome defer refuses to read our own cancel
	// as Twitch refusing the login; and Start's loop Swaps it to false and
	// reconnects immediately, charging nothing to the reconnect budget.
	//
	// It is armed ONLY when there is a live session to interrupt. A flag left
	// standing on an idle downloader would suppress the handshake verdict of
	// some LATER session, turning a genuine refusal into an unbounded retry
	// loop on credentials Twitch will not take.
	reauthPending atomic.Bool
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
	// runIRCSession for its lifetime, guarded by mu). Stop, MarkStreamEnded and
	// Reauthenticate fire it so a session parked in a quiet-channel read (up to
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
	Credentials func() (token, login string)
	// OnAuthDowngrade is called AT MOST ONCE per CREDENTIAL PAIR — once per
	// downloader until Arc 10, and still once per downloader for any job whose
	// cookies never change. Reauthenticate resets the latch, so a repaired
	// credential that fails again reports again, by design.
	//
	// It fires when a job that had Twitch credentials is capturing chat
	// anonymously anyway. reason is one of the AuthDowngrade* constants and
	// never contains a credential, so it is safe to put in front of an operator
	// verbatim. Optional — nil is the ordinary case (tests, and any caller with
	// nowhere to route it).
	//
	// Called on the IRC session goroutine, so it must not block: the read loop
	// is waiting behind it.
	OnAuthDowngrade func(reason string)
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
		onAuthDowngrade: opts.OnAuthDowngrade,
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

// reportAuthDowngrade tells this downloader's owner, at most once, that a job
// which HAD Twitch credentials is capturing chat anonymously.
//
// ONE report per CREDENTIAL PAIR across every trigger site (Reauthenticate
// resets the latch — see there for why all three latches move together), not
// one per site. All of them mean the same thing to an operator —
// subscriber-only messages and badges are being lost for this job — and the
// consumer turns the report into a notification, so a job that reported "no
// login cookie", had its cookies re-imported mid-stream, and was then REFUSED
// would notify twice about one broken capture. The log keeps every line; the
// report is one.
//
// The latch is a THIRD flag rather than a reuse of the two that already exist,
// and both refusals are load-bearing. authRefused is not a report flag:
// sessionCredentials returns an empty pair once it is set, so latching through
// it would demote a job to anonymous chat as a side effect of describing it
// (see noteMissingLogin's own note on that trap). warnedNoLogin is per-SITE —
// the fallback's Warns never touch it — so latching through that one would let
// noteHandshakeOutcome report a second time for the same job.
//
// Fired synchronously on the IRC session goroutine, AFTER the Warn at each
// site. The consumer's delivery is asynchronous, so this does not hold the read
// loop; ordering it after the Warn keeps the log line ahead of the notification
// it explains.
//
// reason is one of the AuthDowngrade* constants and nothing else — never the
// token, never the login, never anything read off the wire.
func (cd *ChatDownloader) reportAuthDowngrade(reason string) {
	if cd.onAuthDowngrade == nil || cd.downgradeReported.Swap(true) {
		return
	}
	cd.onAuthDowngrade(reason)
}

// noteMissingLogin reports the anonymous handshakes nothing else can see: an
// auth-token with no USABLE login cookie beside it.
//
// The states Twitch chat can be anonymous in are enumerated in
// cookies.twitchAuthCookieNames; this function owns two of them, and they are
// the two that reach the wire without anything else noticing. No credentials at
// all is the ordinary cookieless install and is not a degradation. A login
// Twitch REFUSES produces noteHandshakeOutcome's Warn. But a token whose login
// is absent — or present and unsendable as an IRC nickname — never attempts the
// authenticated handshake at all: ircHandshakeLines renders the full anonymous
// pair, because a token beside the justinfan nickname is the hybrid Twitch
// rejects. So there is no refusal to observe, nothing logs, and the
// operator-facing predicates disagree with reality — HasTwitchAuthCookies reads
// true (the token IS there) and both UIs show green, while the capture quietly
// drops every subscriber-only message and badge for the whole job.
//
// Both inputs are reachable on day one. A minimal hand-written cookies.txt
// carrying only auth-token is the first; mergeCookieFiles can manufacture it
// later by pruning an expired login row. The second is the same file with
// `login` filled in by hand as a display name — "archiver account", with the
// space — which HasTwitchAuthCookies also reads as green.
//
// UNUSABLE rather than ABSENT is the condition, and the narrower one was wrong
// rather than merely incomplete: `login != ""` passes a value that
// ircHandshakeLines then throws away, which is precisely a silent anonymous
// session with credentials in the file. One predicate decides both — see
// hasRowBreakingChar.
//
// It is reported HERE rather than by the jar's auth predicates, and that split
// is the point: this site knows the handshake actually went anonymous, whereas
// twitchAuthCookieNames could only know a name is absent — and making that
// list say so fires the auth-loss alarm on installs whose login cookie may
// never have meant anything. See twitchAuthCookieNames for that trace.
//
// ONCE PER CREDENTIAL PAIR, not per session (Reauthenticate resets the latch).
// The condition is a property of the cookie file, so a job that reconnects
// hourly for three days would otherwise repeat this line hourly for three days
// and bury the rest of its log. Repaired cookies are not a silence any more:
// the credential change resets this latch, so a file that is STILL missing its
// login row says so again, and one that is not says so positively through the
// accepted-login line at the 001 (see runIRCSession).
//
// ONE line for both inputs, because the remedy is one thing — re-export the
// cookies — and an operator who hand-wrote either of them re-exports out of
// both. The two are still told apart where the difference can be acted on
// programmatically: reportAuthDowngrade's reason.
//
// Neither the token nor the login is named — this line names neither, and
// there is nothing here to name them with.
func (cd *ChatDownloader) noteMissingLogin(token, login string) {
	// The token check is load-bearing, not defensive: without it every
	// cookieless install — most installs — would get this warning about
	// credentials it never had.
	if token == "" {
		return
	}
	reason := AuthDowngradeNoLoginCookie
	if login != "" {
		if !hasRowBreakingChar(login) {
			return // a complete, sendable pair — nothing is degraded
		}
		reason = AuthDowngradeUnusableLoginCookie
	}
	if cd.warnedNoLogin.Swap(true) {
		return
	}
	cd.logger.Warn("twitch chat: auth-token present but no usable login cookie — chat will be "+
		"captured anonymously (subscriber-only messages and badges will be missing); re-export "+
		"cookies from a signed-in browser",
		"channel", cd.channelLogin)
	cd.reportAuthDowngrade(reason)
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
// ONE-SHOT per CREDENTIAL PAIR: once anonymous, the job stays anonymous for as
// long as the cookie file holds the pair Twitch refused. Flapping would re-pay
// the rejected handshake on every reconnect, which is the cost this exists to
// bound — so the latch is cleared by exactly one thing, Reauthenticate, which
// fires when the credential pair on disk actually changes. A cookie repaired
// mid-job therefore DOES re-authenticate chat, in place, without waiting for
// the next job; a second refusal on the new pair latches again here.
//
// Neither the token nor the login is named in the log line.
//
// Both Warns are followed by reportAuthDowngrade, which carries the same fact
// out of the log to whoever owns this downloader. That report is latched ONCE
// PER CREDENTIAL PAIR across this site and noteMissingLogin together, so it is
// not the one-shot above by another name — see reportAuthDowngrade.
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
		cd.reportAuthDowngrade(AuthDowngradeLoginRefused)
		return
	}
	cd.logger.Warn("twitch never acknowledged the authenticated IRC login; continuing anonymously "+
		"— subscriber-only messages and badges will not be captured for this job",
		"channel", cd.channelLogin)
	cd.reportAuthDowngrade(AuthDowngradeLoginUnacknowledged)
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
	// immediate suppresses the backoff for a reconnect WE asked for. Separate
	// from reconnectAttempts because the two answer different questions: how
	// many times the network has failed us, and whether to wait before the
	// next attempt. A credential the operator has just repaired must reach the
	// wire now, not after thirty seconds.
	immediate := false

	for reconnectAttempts <= maxReconnects {
		if ctx.Err() != nil || !cd.IsRunning() {
			return nil
		}

		if reconnectAttempts > 0 && !immediate {
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
		immediate = false

		sessionStart := time.Now()
		err := cd.runIRCSession(ctx)
		sessionUptime := time.Since(sessionStart)

		// A Reauthenticate() cancelled this session on purpose. Swap rather
		// than Load, and on EVERY exit path rather than only the expected one:
		// the flag's job is done once the session it interrupted has unwound,
		// and a cancel that raced a real socket failure must not leave it
		// standing into a later session where it would suppress a genuine
		// refusal.
		//
		// Straight back in: no backoff, and nothing charged to the reconnect
		// budget. That budget bounds retries against a network that will not
		// stay up; this drop was ours, and eleven credential repairs during one
		// marathon stream must not be able to exhaust it and abandon chat for
		// the rest of the job.
		if cd.reauthPending.Swap(false) && ctx.Err() == nil && cd.IsRunning() {
			// Flush first, exactly as the backoff path above does. The owner
			// priced this reconnect as "one flush plus one reconnect per
			// session per credential change", and that is the cost the docs
			// quote; skipping it would leave the tail of this session's chat in
			// memory until the next session's flusher tick, which is a second,
			// undocumented behaviour for no saving.
			cd.flush()

			// Again, and here rather than only in Reauthenticate: the defer
			// that judged the session we just cancelled reads reauthPending
			// OUTSIDE any lock, so a verdict whose guard slipped in a moment
			// before the arm can have set these three AFTER Reauthenticate
			// cleared them — and the reconnect we are about to make would then
			// present the anonymous pair, which is the whole failure this
			// mechanism exists to end. Deferred functions all complete before
			// runIRCSession returns, so this point dominates every exit path of
			// the session it interrupted; it is the only one that does.
			//
			// After the flush rather than before it, so the re-clear is the
			// last thing between the outgoing session and the next handshake.
			// The report that already fired in that window is not recoverable
			// here and is not meant to be: at the verdict the code cannot know
			// a reset is coming, and the sticky platform mark is where a stale
			// one is reconciled.
			cd.authRefused.Store(false)
			cd.downgradeReported.Store(false)
			cd.warnedNoLogin.Store(false)

			// A session WE ended after it had been healthy for a long time is
			// still a healthy session, so it must clear the counter exactly as
			// any other exit would. Without this, a job carrying failures from
			// earlier network trouble keeps them across a credential repair and
			// sits that much closer to abandoning chat for the rest of the job
			// — a repair making things worse. Same threshold, same line, same
			// fact as the ordinary path below.
			if sessionUptime >= reconnectResetUptime && reconnectAttempts > 0 {
				cd.logger.Info("IRC session was stable before disconnect; resetting reconnect counter",
					"channel", cd.channelLogin, "uptime", sessionUptime)
				reconnectAttempts = 0
			}

			cd.logger.Info("twitch chat: reconnecting with the refreshed credentials", "channel", cd.channelLogin)
			immediate = true
			continue
		}

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

// Reauthenticate tells a running downloader to drop its IRC session and open a
// new one with whatever credentials the cookie jar holds NOW.
//
// The problem it solves. Credentials are re-read per session, but nothing ends
// a healthy session, and after a refusal authRefused latches for the life of
// the downloader — so an operator who repairs cookies.txt four hours into a
// twelve-hour capture keeps capturing anonymously until the job ends, losing
// every subscriber-only message and badge in between. This is the only thing
// that can undo that.
//
// IT RESETS THREE LATCHES, not two. authRefused is the behaviour switch that
// makes sessionCredentials return an empty pair. downgradeReported is the
// one-report-per-downloader latch. And warnedNoLogin is not merely a log
// latch: noteMissingLogin returns on its Swap BEFORE it reaches
// reportAuthDowngrade, so leaving it set means a repaired cookie file that is
// still missing its login row reports NOTHING the second time — precisely the
// silence this whole mechanism exists to end.
//
// All three are reset TWICE: once here, under cd.mu, and once in Start's reauth
// branch just before the next session opens. That is not belt-and-braces, it is
// two different orderings — see the critical section below for the first and
// the branch itself for the second. Between them the next handshake is judged
// on its own merits whatever the dying session did on its way out.
//
// The drop goes through the existing sessionCancel, so a session parked in a
// six-minute read reacts at once rather than minutes later.
//
// reauthPending is armed inside the same critical section that reads
// sessionCancel, and only when a session exists. Arming it for an idle
// downloader would leave it standing until some later session ended, and that
// session's handshake-outcome defer would then read a genuine refusal as our
// own cancel.
//
// Safe on a downloader that is not running: the latches are still cleared, so
// the next Start — the orchestrator relaunches chat after a connectivity gap —
// presents credentials. Safe from any goroutine, and it does not block.
func (cd *ChatDownloader) Reauthenticate() {
	// ONE critical section, and the three resets belong inside it. It holds
	// interruptSession's inlined body — the read of sessionCancel and the arm
	// must not be separated, because between them a session could start or end
	// — and the resets are in it for a second, sharper reason: runIRCSession
	// clears sessionCancel under this same lock AFTER its handshake defer has
	// judged the outgoing session. Reset outside the lock and that verdict can
	// land on the NEW pair, re-latching authRefused so sessionCredentials
	// returns an empty pair and the reconnect we are about to ask for goes out
	// ANONYMOUS. Inside the lock, either we arm before the defer reads
	// reauthPending (the verdict is skipped) or we are ordered after the clear
	// and our resets dominate the verdict. There is no third order.
	//
	// Nothing here does I/O, so the hold is O(1): the cancel is called after
	// the unlock, which is also the one thing in this function that could
	// deadlock if it were not.
	cd.mu.Lock()
	cd.authRefused.Store(false)
	cd.downgradeReported.Store(false)
	cd.warnedNoLogin.Store(false)
	cancel := cd.sessionCancel
	if cancel != nil {
		cd.reauthPending.Store(true)
	}
	cd.mu.Unlock()

	// Names no credential, and there is nothing here to name one with.
	cd.logger.Info("twitch chat: re-authenticating with the current credentials",
		"channel", cd.channelLogin, "hadLiveSession", cancel != nil)
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
