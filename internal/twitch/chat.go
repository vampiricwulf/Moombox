package twitch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// ircKeepaliveIdle is how long the session tolerates hearing NOTHING from
	// Twitch before it speaks first. Twitch's own server PING is every ~5 min,
	// so 45 s of silence is not by itself alarming — it is simply the point at
	// which asking is cheaper than waiting.
	ircKeepaliveIdle = 45 * time.Second
	// ircKeepalivePongWait is how long Twitch has to produce ANY inbound frame
	// after our PING before the socket is declared dead. A live connection
	// answers in milliseconds; this is generous enough to survive a stalled
	// second on a congested link.
	ircKeepalivePongWait = 10 * time.Second
	// ircKeepaliveCheck is how often the two windows above are evaluated. It
	// bounds the detection overshoot: a dead socket is noticed within
	// ircKeepaliveIdle + ircKeepaliveCheck + ircKeepalivePongWait ≈ 70 s,
	// against ircReadDeadline's 6 minutes. Twitch IRC has no replay, so every
	// second of that difference is chat that would have been lost outright.
	ircKeepaliveCheck = 15 * time.Second
	// ircResumeSaveFloor is the minimum gap between two writes of the IRC
	// chat resume sidecar. Owner ruling (sweep-2 "IRC sidecar"), and the VOD
	// path's shape (vodChatFlushInterval).
	//
	// Every flush used to be followed by a ~39 KB marshal, fsync and rename —
	// up to once a second while chat is pending, beside the chat.json append
	// fsync. The sidecar only ever carries the dedup window a reconnect replay
	// can overlap, so a save that is at most five seconds behind the file
	// costs a count that is short by the messages written in the unsaved
	// window: restoreResumeState seeds fileCount/totalCount straight from
	// that stale sidecar and sets flushedToDisk, which makes Start skip
	// adoptExistingPartFile — the only path that re-counts the file — so the
	// deficit persists in that part's header count and the job's chat total
	// until the next part roll. No message is lost; the file itself is
	// written every flush regardless. The DEFERRED final save on stop
	// (Start's exit path) is deliberately NOT throttled.
	ircResumeSaveFloor = 5 * time.Second
	// ircKeepalivePing is the exact line the keepalive sends. IRC PING/PONG
	// rather than a WebSocket ping frame: a WS pong proves the socket is open,
	// while this proves the IRC layer behind it is still serving us.
	// chatterino7 pings the same way
	// (references/chatterino7/src/providers/twitch/IrcConnection2.cpp).
	ircKeepalivePing = "PING :moombox"
	// chatHeaderScanLimit bounds the prefix chatFileRecordingBaseMs reads out
	// of an existing part file. TwitchChatData's header is a handful of short
	// scalars written before the messages array — that field order is already
	// load-bearing for AppendChatMessages — so 4 KB is far more than it can
	// occupy, and the point of the bound is that a marathon part's chat.json
	// is tens of megabytes that must not be pulled into memory for one stamp.
	chatHeaderScanLimit = 4096
	// chatPartReadBuffer sizes the reader readChatPartFileSummary streams a
	// part file through. json.Decoder refills 512 bytes at a time on its own,
	// which is ~100k syscalls for a 50 MB part; one buffered read per 64 KB
	// is the whole reason the bufio wrapper is there.
	chatPartReadBuffer = 64 * 1024
	// chatCorruptSuffix names the copy adoptExistingPartFile moves an
	// unreadable part file to before the session starts writing a new one.
	// One fixed name, not a timestamped series: the point is that the bytes
	// survive for a human to look at, not that every failed parse accumulates
	// its own artifact in staging. Same suffix the YouTube downloader uses
	// (internal/chat/downloader.go corruptChatSuffix) — one recovery
	// convention across both platforms.
	chatCorruptSuffix = ".corrupt"
	// chatEmoteOffsetsUTF16 is the only value TwitchChatData.EmoteOffsets ever
	// carries. One constant with two writers — the IRC full-file write and the
	// VOD one — because a file written with the marker misspelled is
	// indistinguishable at replay from a legacy file, and would be "corrected"
	// a second time.
	chatEmoteOffsetsUTF16 = "utf16"
	// chatResumeIDCap bounds the dedup IDs a resume sidecar carries. ONE
	// constant for both chat downloaders: the VOD path has always capped at
	// 1000 and the IRC path snapshotted its whole 5000-entry set, which is a
	// ~200 KB marshal + fsync + rename every second on a busy channel
	// (chatSaveInterval). The window a reconnect replay can overlap is
	// seconds, so the newest 1000 is the whole of what the cap has to cover.
	chatResumeIDCap = 1000
)

// The fixed vocabulary of Twitch auth-downgrade reasons: one value per route
// from "this install HAD Twitch credentials" to "Twitch would not honour them".
// The first four are ChatDownloaderOptions.OnAuthDowngrade's reason — the
// routes from held credentials to a chat session captured anonymously. The
// fifth (Arc 10 R6) is the playback-token route and is never passed to
// OnAuthDowngrade; it shares this block because it shares the vocabulary.
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
	// AuthDowngradePlaybackTokenAnonymous: the jar held credentials and Twitch
	// nonetheless issued an ANONYMOUS playback access token, so this capture
	// is served stitched ads and would be refused subscriber-only content.
	//
	// Reported by Service.GetHLSMasterPlaylist, NOT by the chat downloader —
	// it lives in this block because it is a member of the same vocabulary and
	// splitting the vocabulary across two files is how two of them drift. It
	// is also the ONLY member a job with chat capture switched off can ever
	// produce: every other route runs on the IRC path.
	AuthDowngradePlaybackTokenAnonymous = "playback-token-anonymous"
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

// errKeepaliveTimeout ends an IRC session the KEEPALIVE gave up on: we spoke
// first and Twitch produced no frame at all within ircKeepalivePongWait.
//
// Unlike errReauthRequested it IS compared against, with errors.Is, and that
// comparison is the whole reason it exists. Start's loop charges
// reconnectAttempts for a session that failed, and resets the counter only for
// one that stayed up past reconnectResetUptime (5 min). A keepalive verdict
// lands at ircKeepaliveIdle + ircKeepaliveCheck + ircKeepalivePongWait (~70 s),
// which is BELOW that threshold — so without this sentinel a middlebox that
// swallows PONGs would charge every reconnect, exhaust maxReconnects in about
// fifteen minutes, and abandon chat for the rest of the job. Before the
// keepalive existed the same socket was detected at ircReadDeadline (6 min),
// always ABOVE the threshold, and chat retried forever; adding a faster
// detector must not turn a recoverable network into a surrendered one.
//
// So this is OUR reconnect, exactly like the reauth path: logged, charged
// nothing, and given no backoff of its own. At budget 0 — the ordinary case
// here, precisely because a keepalive verdict never charges — Start's
// `continue` re-dials at once, and the ~70 s the verdict itself took is the
// only wait there is. A budget carried from EARLIER, real failures is applied
// unchanged by the loop head: those failures are still a reason to slow down,
// and this path neither adds to them nor clears them. It is wrapped rather
// than returned bare so the operator-facing text can name the window that
// elapsed.
var errKeepaliveTimeout = errors.New("twitch IRC keepalive: no response")

// errServerReconnect ends an IRC session because TWITCH asked for it: the
// server sent a RECONNECT line, which it does routinely when it takes a chat
// edge out of service.
//
// Like errKeepaliveTimeout it IS compared against, with errors.Is, and that
// comparison is the whole reason it exists. Before it, the read loop answered
// the directive with nil — and nil is Start's CLEAN-EXIT value. The loop
// returned, the orchestrator's chat goroutine closed its done channel, and
// nothing relaunched chat for the rest of the job: the orchestrator relaunches
// only when a connectivity outage is declared over. A routine maintenance
// message therefore ended chat capture on a live stream with no error anywhere
// to say so.
//
// Deliberately the keepalive's shape and not a second mechanism: this is OUR
// reconnect, so it is logged once at the loop, charged nothing against
// reconnectAttempts, and given no backoff of its own. Charging it would be
// worse here than for a keepalive verdict — a server rotating its edges can
// issue several directives in one marathon stream, none of them after the five
// minutes of uptime that clears the counter, so ten would exhaust maxReconnects
// and abandon chat for messages Twitch asked us to come back for. It does NOT
// set the reauth path's `immediate`: a budget carried from EARLIER, real
// failures is still a reason to wait, and the loop head applies it unchanged.
//
// The socket is force-closed with CloseNow (no close handshake) right before
// this sentinel is returned: Twitch has already told us to leave, so waiting
// on a peer that may not answer buys nothing, and it would otherwise cost the
// ordinary close handshake's full peer-ack timeout — a real chat gap on every
// RECONNECT.
var errServerReconnect = errors.New("twitch IRC: server requested a reconnect")

// errChatPartMalformed marks a part file whose BYTES were read in full and are
// not chat JSON this package can use — a truncated write, a half-flushed
// array, something else entirely at the path.
//
// It exists because the caller's response to it is DESTRUCTIVE: a malformed
// part file is renamed to <path>.corrupt so the session can start a fresh one
// beside it. A file we merely failed to READ — an antivirus lock, a sharing
// violation, a directory in its place — has told us nothing about its content,
// and moving it aside on that basis would take a healthy archive out from
// under the job. See adoptExistingPartFile.
var errChatPartMalformed = errors.New("chat part file is not readable as chat JSON")

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
	// lastResumeSave is when saveResumeStateThrottled last WROTE. Guarded by
	// cd.mu; the zero value means "never", which always writes. A time.Time
	// in a struct field keeps its monotonic reading, so the comparison below
	// is immune to a wall-clock step (ruling R7b, sweep 1).
	lastResumeSave time.Time
	emoteResolver  *EmoteResolver
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

	// delays is every keepalive wait runIRCSession sleeps on;
	// defaultChatDelays() in production, a scaled copy in tests (see delays.go).
	// Assigned once at construction and never written again, so the session
	// goroutine reads it without the mutex.
	delays chatDelays

	// keepaliveWrite sends the keepalive PING, and ONLY that frame — the
	// handshake, the PONG and everything else still write through conn
	// directly. It is a seam rather than a call because the one behaviour that
	// has to be pinned here is a write that never completes, and the only way
	// to get one from a real socket is a peer with a full send buffer that a
	// test cannot conjure. writeIRCFrame in production; assigned once at
	// construction and read from the keepalive goroutine without the mutex,
	// exactly like delays.
	keepaliveWrite ircFrameWriter

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
		delays:          defaultChatDelays(),
		keepaliveWrite:  writeIRCFrame,
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
	// Newest-first window, not the whole set — see chatResumeIDCap.
	recentIDs := cd.dedup.Snapshot(chatResumeIDCap)
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

// saveResumeStateThrottled writes the resume sidecar unless one was written
// less than delays.resumeSaveFloor ago. Returns whether it wrote.
//
// This is the periodic path (flushLocked). The exit path in Start calls
// saveResumeState directly and is never throttled: that save is the one a
// restart actually reads.
func (cd *ChatDownloader) saveResumeStateThrottled() bool {
	cd.mu.Lock()
	floor := cd.delays.resumeSaveFloor
	last := cd.lastResumeSave
	if floor > 0 && !last.IsZero() && time.Since(last) < floor {
		cd.mu.Unlock()
		return false
	}
	cd.lastResumeSave = time.Now()
	cd.mu.Unlock()

	cd.saveResumeState()
	return true
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

// adoptPartRecordingBase seeds recordingStartMs from the part file already on
// disk, so a part RESUMED after a daemon restart keeps counting offsets from
// the base its earlier messages were written against instead of from the
// restart. No-op when there is no part file, or no base recorded in it.
//
// The file, not the run. The orchestrator hands every session time.Now() as
// the recording start — SetRecordingStartTime, and the RollFile that redirects
// a resumed job into the part it left off in — which is right for a part that
// begins now and wrong for one that began hours ago. The resumed part's VIDEO
// is appended to (the engine reopens video_stream O_APPEND at the resume
// sidecar's byte position, and the part is muxed with a derived start rather
// than the restart time), so the part's timeline still starts where it always
// did. Rebasing the chat to the restart drops every post-restart message back
// onto the head of the part — landing on top of the pre-restart chat, hours
// out of position, with two clocks in one file. One file, one epoch: the same
// rule the YouTube downloader states as "keeping the file's epoch over the
// run's start time" (internal/chat/downloader.go).
//
// What is left wrong is bounded and is the outage itself: offsets are placed
// at (message − base) while the video lost whatever segments expired from the
// playlist window while the daemon was down. Twitch live has no DVR, so a
// resume across a gap the window cannot cover makes the engine report a gap
// and the orchestrator split into a FRESH part — new dir, new file, new base —
// which is the case this function then correctly declines to touch. The
// residual is therefore at most a window's worth of drift, against hours of
// misplacement for the alternative.
//
// Only on a fresh Start (the caller's !alreadyInitialized gate). A downloader
// the orchestrator re-Starts after a connectivity outage already holds the
// base its part file was written with, so there is nothing to adopt and no
// reason to race a roll for it.
//
// The store is conditional on outputPath still being the path that was read.
// Start runs on its own goroutine while the video loop is already going, so a
// gap split can call RollFile in between — and the new part's base must not be
// overwritten with the closed part's.
func (cd *ChatDownloader) adoptPartRecordingBase() {
	path := cd.currentOutputPath()
	if path == "" {
		return
	}
	fileBaseMs, ok, readErr := chatFileRecordingBaseMs(path)
	if !ok {
		// "No file" is the ordinary case and stays silent. Anything else —
		// a Windows sharing violation, an AV lock, a directory in the file's
		// place — means the part MAY have had a base to adopt and we could not
		// see it, so the run's restart time stands and every offset in this
		// part is shifted by the outage. Without a line here that drift is
		// indistinguishable from a fresh part (review F6).
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			cd.logger.Debug("twitch chat: could not read the part file's recording base; using the run's start",
				"channel", cd.channelLogin, "path", path, "err", readErr)
		}
		return
	}

	cd.mu.Lock()
	runBaseMs := cd.recordingStartMs.Load()
	adopt := cd.outputPath == path && runBaseMs != fileBaseMs
	if adopt {
		cd.recordingStartMs.Store(fileBaseMs)
	}
	cd.mu.Unlock()

	if !adopt {
		return
	}
	cd.logger.Info("twitch chat: resuming part with its recorded base",
		"channel", cd.channelLogin, "path", path,
		"fileBaseMs", fileBaseMs, "runBaseMs", runBaseMs)
}

// chatFileRecordingBaseMs reads recordingStartTime out of an existing part's
// chat file and returns it in Unix milliseconds.
//
// ok is false for everything it cannot read POSITIVELY — no file, a file this
// package did not write, a header written before the field existed, a header
// longer than chatHeaderScanLimit, an unparseable stamp. The caller's fallback
// is the run's own start time, and an invented base is worse than a stale one.
//
// The third return separates "the bytes never arrived" from "the bytes said
// nothing": it is non-nil ONLY for an os.Open / io.ReadFull failure, so the
// caller can tell a missing file (silence) from an unreadable one (a Debug
// line). Every content verdict returns a nil error, because none of them is a
// fault worth logging on an ordinary fresh part.
//
// Token-walked rather than unmarshalled: json.Decoder.Decode buffers the whole
// top-level value, which for a part file is the entire message history.
//
// A var, not a func, so a test can drive the race the caller's outputPath
// guard exists for — a RollFile landing between this read and the store. The
// same seam shape as readChatPartFileSummary below.
var chatFileRecordingBaseMs = func(path string) (int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	head := make([]byte, chatHeaderScanLimit)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, false, err
	}

	dec := json.NewDecoder(bytes.NewReader(head[:n]))
	opening, err := dec.Token()
	if err != nil {
		return 0, false, nil
	}
	if delim, isDelim := opening.(json.Delim); !isDelim || delim != '{' {
		return 0, false, nil
	}
	for {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, false, nil
		}
		key, isKey := keyTok.(string)
		if !isKey {
			return 0, false, nil // the object closed without the field
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, false, nil
		}
		if key != "recordingStartTime" {
			// Every header field is a scalar and all of them precede
			// "emotes"/"messages", so a composite value means the header is
			// over (or this is not one of our files) — and descending into it
			// is exactly the read the scan limit exists to avoid.
			if _, composite := valTok.(json.Delim); composite {
				return 0, false, nil
			}
			continue
		}
		stamp, isString := valTok.(string)
		if !isString {
			return 0, false, nil
		}
		t, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return 0, false, nil
		}
		return t.UnixMilli(), true, nil
	}
}

// adoptExistingPartFile is THE ADOPTION RULE, the Twitch twin of the YouTube
// downloader's adoptExistingChatFile (internal/chat/downloader.go): when no
// usable sidecar restored this part's state but its chat file is on disk, the
// FILE is the part's history and it must be appended to, not replaced.
// Returns the number of messages adopted (0 = start fresh).
//
// Without it, flushedToDisk stays false and the first flush takes
// writeFullChatFileTo, which writes the file from scratch out of the current
// batch alone — every message archived before the restart is gone. A sidecar
// goes missing on any of several ordinary routes: a crash in the window
// between the file write and the sidecar write, a stream-end drain that
// cleared it before the job was resumed anyway, or an operator tidying
// staging. None of them should cost the part its chat.
//
// CALLED ONLY WHEN NO SIDECAR RESTORED THE PART — see the gate at its call
// site in Start, which mirrors the twin's (internal/chat/downloader.go's
// `if !resuming`). Not a nicety: this function's failure branch RENAMES the
// part file aside, and a sidecar restore has already set flushedToDisk from a
// file that merely stats. Renaming under that flag would leave every later
// flush appending to a path with no file — failing, failing again through the
// merge fallback, and buffering the whole broadcast in memory with nothing on
// disk. The gate also keeps the O(file) read off every ordinary resume, where
// its result would be discarded anyway.
//
// Four cases:
//   - No file: 0, and the part starts fresh exactly as before.
//   - It reads and holds messages: fileCount becomes the length of the
//     messages ARRAY, not the header's messageCount — the array is the data,
//     and a header that over-counts would propagate forever, whereas the array
//     length self-heals the header on the very first flush (AppendChatMessages
//     writes the new count into it). flushedToDisk is set so that flush takes
//     the append path, and the tail of the file's IDs seeds the dedup so an
//     IRC reconnect replaying messages already on disk cannot duplicate them.
//   - Its BYTES read fine and are not our JSON (errChatPartMalformed): they
//     are moved aside to <path>.corrupt and the part starts fresh. Overwriting
//     them destroys the only copy of something a human could still salvage;
//     refusing to write at all is worse again, because the session would then
//     buffer the whole broadcast's chat in memory and persist none of it. If
//     the RENAME itself fails, the failure is logged and the run proceeds
//     anyway — an unreadable file must not stop the job archiving for good.
//   - The bytes could not be READ at all — an antivirus lock, a Windows
//     sharing violation, a directory in the file's place: logged and left
//     exactly where it is. A failed read is not a verdict on the content, and
//     the response to the verdict is a rename; moving a healthy archive out
//     from under a running job on the strength of a transient lock is a
//     worse outcome than not adopting it.
//
// A file that reads but holds no messages is not adopted: there is no history
// for a full write to lose, and its header base has already been taken by
// adoptPartRecordingBase.
//
// The adoption is conditional on outputPath still being the path that was
// read, for the same reason adoptPartRecordingBase's store is: Start runs on
// its own goroutine while the video loop is already going, so a gap split can
// call RollFile in between — and the new part's counters must not be seeded
// from the closed part's file.
func (cd *ChatDownloader) adoptExistingPartFile() int {
	path := cd.currentOutputPath()
	if path == "" {
		return 0
	}
	summary, err := readChatPartFileSummary(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0 // nothing on disk — a fresh part
		}
		if !errors.Is(err, errChatPartMalformed) {
			// Read failure, not a content verdict. Leave the file where it is
			// and leave flushedToDisk false: this part behaves exactly as it
			// did before the adoption existed.
			cd.logger.Warn("twitch chat: cannot read the existing part file; leaving it in place",
				"channel", cd.channelLogin, "path", path, "err", err)
			return 0
		}
		corruptPath := path + chatCorruptSuffix
		cd.logger.Error("twitch chat: existing part file unreadable; preserving it instead of overwriting",
			"channel", cd.channelLogin, "path", path, "preservedAs", corruptPath, "err", err)
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			cd.logger.Error("twitch chat: could not preserve the unreadable part file; the next flush will overwrite it",
				"channel", cd.channelLogin, "path", path, "err", renameErr)
		}
		return 0
	}
	if summary.messages == 0 {
		return 0
	}

	cd.mu.Lock()
	adopt := cd.outputPath == path && !cd.flushedToDisk
	if adopt {
		cd.fileCount = summary.messages
		// The job-level metric never drops: a sidecar that restored a larger
		// cumulative total (parts closed earlier in this job) keeps it.
		cd.totalCount = max(cd.totalCount, summary.messages)
		cd.flushedToDisk = true
		// Add, not Restore: additive seeding cannot discard whatever a
		// sidecar restore already put there.
		for _, id := range summary.recentIDs {
			cd.dedup.Add(id)
		}
	}
	// cd.messages is deliberately NOT cleared. On a fresh Start it is empty,
	// and anything it did hold would be unwritten messages — dropping them is
	// the very loss this function exists to prevent.
	cd.mu.Unlock()

	if !adopt {
		return 0
	}
	cd.logger.Info("twitch chat: adopting the existing part file",
		"channel", cd.channelLogin, "path", path, "messages", summary.messages)
	return summary.messages
}

// chatPartFileSummary is everything adoptExistingPartFile needs out of a part
// file already on disk: how many messages it holds and the tail of their IDs.
type chatPartFileSummary struct {
	messages  int
	recentIDs []string
}

// readChatPartFileSummary reads an existing part's chat file and reports its
// message count and the last chatDedupMax message IDs in it.
//
// STREAMED, not unmarshalled. A marathon part's chat.json runs to tens of
// megabytes and this runs at Start for every resumed job, so the decoder walks
// the header's tokens to the "messages" key and then decodes one message at a
// time into an id-only shape — peak memory is one message plus the decoder's
// buffer, instead of the whole file twice (raw bytes plus the decoded slice),
// which is what readChatFileMessages' os.ReadFile + json.Unmarshal costs the
// append-failure fallback. Counting still means reading every byte of the
// file; that is inherent to the format (the header's messageCount is exactly
// the number this function refuses to trust) and it is one sequential pass at
// startup, not per flush.
//
// The price is a second reader that has to know the file's shape. That shape
// is already load-bearing elsewhere — AppendChatMessages splices at the
// messages array's closing bracket, and TwitchChatData documents why
// "messages" must serialize last — so this reader adds no new constraint: it
// tolerates any field order, and steps over an enriched file's "emotes"
// object on the way.
//
// An error means the file could not be read as one of ours, INCLUDING a
// well-formed messages array followed by a broken tail: such a file cannot be
// appended to (AppendChatMessages would splice at whatever ']' the tail's last
// 256 bytes happen to hold), so the caller must preserve it rather than adopt
// it. A missing file returns an os.IsNotExist error, which the caller reads as
// "fresh part" rather than as damage.
// A var, not a func, for the same reason chatFileRecordingBaseMs is one: a
// test needs a RollFile to land between this read and the counter store the
// caller guards with `cd.outputPath == path`.
var readChatPartFileSummary = func(path string) (chatPartFileSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return chatPartFileSummary{}, err
	}
	defer f.Close()

	// The reader is wrapped so a mid-file I/O failure can be told apart from
	// bad bytes AFTER the decoder has folded both into one error. Both arrive
	// at the caller through json.Decoder, but only one of them is a verdict on
	// the file's content — and the caller acts destructively on that verdict.
	src := &ioErrReader{r: f}
	summary, err := decodeChatPartFile(bufio.NewReaderSize(src, chatPartReadBuffer))
	if err == nil {
		return summary, nil
	}
	if src.err != nil {
		// The bytes never arrived. Nothing is known about the content, so the
		// error is reported as-is: not malformed.
		return chatPartFileSummary{}, src.err
	}
	return chatPartFileSummary{}, fmt.Errorf("%w: %w", errChatPartMalformed, err)
}

// ioErrReader records the first non-EOF error the underlying reader returns.
// io.EOF and io.ErrUnexpectedEOF are NOT recorded: a truncated file is a real
// corruption signal and reaches the decoder as an unexpected end of input,
// which is exactly the verdict the caller should act on.
//
// The ErrUnexpectedEOF arm is unreachable through today's ONE caller — the
// reader underneath is an *os.File, and Read never manufactures that error;
// io.ReadFull and io.ReadAtLeast do, and this path uses neither. It stays
// because the rule it states belongs to this type rather than to that caller:
// the field is an io.Reader, and a future one wrapping a ReadFull-based source
// would otherwise have a truncation silently reclassified from "the content is
// corrupt" to "the bytes never arrived", which is the opposite verdict.
type ioErrReader struct {
	r   io.Reader
	err error
}

func (r *ioErrReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && r.err == nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		r.err = err
	}
	return n, err
}

// decodeChatPartFile is readChatPartFileSummary's walk, over an already-open
// reader so the caller can classify what went wrong.
func decodeChatPartFile(r io.Reader) (chatPartFileSummary, error) {
	dec := json.NewDecoder(r)
	opening, err := dec.Token()
	if err != nil {
		return chatPartFileSummary{}, fmt.Errorf("parse chat file: %w", err)
	}
	if delim, isDelim := opening.(json.Delim); !isDelim || delim != '{' {
		return chatPartFileSummary{}, fmt.Errorf("parse chat file: top level is not a JSON object")
	}

	var summary chatPartFileSummary
	found := false
	for !found {
		keyTok, err := dec.Token()
		if err != nil {
			return chatPartFileSummary{}, fmt.Errorf("parse chat file: %w", err)
		}
		if delim, isDelim := keyTok.(json.Delim); isDelim && delim == '}' {
			// A well-formed file with no messages array at all: nothing to
			// adopt, but nothing damaged either.
			return chatPartFileSummary{}, nil
		}
		key, isKey := keyTok.(string)
		if !isKey {
			return chatPartFileSummary{}, fmt.Errorf("parse chat file: unexpected token %v", keyTok)
		}
		if key != "messages" {
			if err := utils.SkipJSONValue(dec); err != nil {
				return chatPartFileSummary{}, fmt.Errorf("parse chat file: %w", err)
			}
			continue
		}
		summary, err = decodeChatMessageIDs(dec)
		if err != nil {
			return chatPartFileSummary{}, err
		}
		found = true
	}

	// The rest of the document has to be well-formed too — see the doc above
	// on why a broken tail must not be adopted.
	if err := utils.FinishJSONValue(dec, 1); err != nil {
		return chatPartFileSummary{}, fmt.Errorf("parse chat file: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return chatPartFileSummary{}, fmt.Errorf("parse chat file: trailing data after the top-level object")
	}
	return summary, nil
}

// decodeChatMessageIDs consumes the messages array, counting its entries and
// keeping the last chatDedupMax IDs — the window an IRC reconnect replay can
// overlap. Holding all of them would scale the dedup with the file instead of
// with the window. "messages": null is an empty part, not damage.
func decodeChatMessageIDs(dec *json.Decoder) (chatPartFileSummary, error) {
	opening, err := dec.Token()
	if err != nil {
		return chatPartFileSummary{}, fmt.Errorf("parse chat messages: %w", err)
	}
	if opening == nil {
		return chatPartFileSummary{}, nil
	}
	if delim, isDelim := opening.(json.Delim); !isDelim || delim != '[' {
		return chatPartFileSummary{}, fmt.Errorf("parse chat messages: not an array")
	}

	var summary chatPartFileSummary
	for dec.More() {
		// id only: the decoder skips every other field without materialising
		// it, so a 400-byte message costs nothing but the scan.
		var msg struct {
			ID string `json:"id"`
		}
		if err := dec.Decode(&msg); err != nil {
			return chatPartFileSummary{}, fmt.Errorf("parse chat messages: %w", err)
		}
		summary.messages++
		if msg.ID == "" {
			continue
		}
		summary.recentIDs = append(summary.recentIDs, msg.ID)
		// Compact at 2x so the trim is amortised rather than per message.
		if len(summary.recentIDs) >= chatDedupMax*2 {
			summary.recentIDs = append([]string(nil), summary.recentIDs[len(summary.recentIDs)-chatDedupMax:]...)
		}
	}
	if _, err := dec.Token(); err != nil { // the array's ']'
		return chatPartFileSummary{}, fmt.Errorf("parse chat messages: %w", err)
	}
	if len(summary.recentIDs) > chatDedupMax {
		summary.recentIDs = summary.recentIDs[len(summary.recentIDs)-chatDedupMax:]
	}
	return summary, nil
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
		// Independently of the sidecar: the part FILE decides this part's
		// offset base, and it outlives any resume state (RollFile clears a
		// closed part's, and a crash can lose one). See adoptPartRecordingBase
		// for why the restart is the wrong base for a part that began hours ago.
		cd.adoptPartRecordingBase()

		// And the part file is this part's HISTORY. With the file on disk but
		// no usable sidecar, flushedToDisk would otherwise stay false and the
		// first flush would write the file from scratch out of the new batch
		// alone. Base first, deliberately: a malformed file is moved aside
		// there, and the base read out of its header before that is the epoch
		// the preserved copy's offsets were computed against — so the fresh
		// file and the .corrupt beside it keep one clock between them.
		//
		// ONLY when no sidecar restored the part. flushedToDisk is the record
		// of that (restoreResumeState is the one thing that sets it before
		// this point), and reading it here rather than inside the adoption
		// keeps the destructive branch unreachable in the restored case
		// instead of merely harmless — see adoptExistingPartFile. It also
		// keeps a full pass over a marathon part file off every ordinary
		// resume, where the result would be discarded.
		cd.mu.Lock()
		sidecarRestored := cd.flushedToDisk
		cd.mu.Unlock()
		if !sidecarRestored {
			// The adopted count is deliberately NOT pushed through
			// callOnProgress here. MessageCount() is cd.totalCount, which this
			// path seeds from the CURRENT PART's file alone, and
			// ProgressTracker.SetChatCount is a plain assignment rather than a
			// max (internal/worker/progress.go). On a multi-part job whose row
			// already holds the cumulative total, pushing at Start would drop
			// the row to this part's count immediately. That undercount is a
			// pre-existing intake item — totalCount is not seeded from the
			// sidecar-less earlier parts' files — and a push here would only
			// make its symptom instant and deterministic instead of waiting
			// for the next message. Fix the seeding, then push.
			cd.adoptExistingPartFile()
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
			// Don't clear resume state on panic — allow resume on restart.
			// Unthrottled, exactly like the interrupted-exit save just below:
			// the flush above went through saveResumeStateThrottled, which can
			// leave the sidecar up to resumeSaveFloor stale, and a panic's
			// resume point must be as fresh as any other interrupted exit.
			cd.saveResumeState()
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

		// The keepalive gave up on a socket that stopped answering. That is a
		// reconnect WE asked for, not the network refusing us one, so it costs
		// nothing from the budget — see errKeepaliveTimeout for why a cheaper
		// detector would otherwise abandon chat on a network the slow one
		// tolerated forever.
		//
		// NOT the reauth path's `immediate`, though: that one exists because a
		// repaired credential must reach the wire now. Here the far side is
		// unresponsive, so whatever the budget already carries is exactly
		// right — and at budget 0, which is where a run of keepalive verdicts
		// leaves it, that is no backoff at all: the `continue` below re-dials
		// immediately and the ~70 s the verdict took is the only wait. A
		// budget carried from EARLIER, real failures is applied unchanged by
		// the loop head, so a session that has genuinely been failing does
		// still wait between attempts.
		if errors.Is(err, errKeepaliveTimeout) {
			// Flush first, exactly as the backoff path above and the reauth
			// path do. At budget 0 — where a keepalive verdict leaves it —
			// the loop head's backoff block does not run at all, and that
			// block is where a reconnect normally saves state; without the
			// flush here the tail of this session's chat would sit in memory
			// until the next session's flusher tick, and the session we just
			// lost is precisely the one whose last messages are least likely
			// to be recoverable. A budget carried from earlier real failures
			// does reach that block and flush again — one redundant flush of
			// an empty buffer, not a second behaviour.
			cd.flush()
			cd.logger.Warn("twitch IRC keepalive gave up on the connection; reconnecting without charging the reconnect budget",
				"err", err, "channel", cd.channelLogin, "uptime", sessionUptime)
			continue
		}

		// Twitch asked for this one. Same accounting as the keepalive verdict
		// above, and for a stronger reason: a server rotating its chat edges
		// can issue several RECONNECTs in one marathon stream, none of them
		// after the five minutes of uptime that clears the counter, so charging
		// them would walk the budget to zero and abandon chat for messages
		// Twitch asked us to come back for. Flush first, exactly as every other
		// path that re-dials does — at budget 0 the loop head's backoff block
		// does not run, and that block is where a reconnect normally saves
		// state.
		if errors.Is(err, errServerReconnect) {
			cd.flush()
			cd.logger.Info("twitch IRC: server requested a reconnect; reconnecting without charging the reconnect budget",
				"channel", cd.channelLogin, "uptime", sessionUptime)
			continue
		}

		reconnectAttempts++
		cd.logger.Warn("IRC session error, will reconnect", "err", err, "channel", cd.channelLogin)
	}

	return fmt.Errorf("exceeded max IRC reconnects for %s", cd.channelLogin)
}

func (cd *ChatDownloader) addMessage(msg *TwitchChatMessage) {
	cd.mu.Lock()

	if !cd.dedup.Add(msg.ID) {
		cd.mu.Unlock()
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
		// Signed on purpose: a message that arrived before this part's
		// recording base was sent BEFORE the video starts. The player renders
		// negative offsets as pre-show chat ("-1:30"); clamping to 0 used to
		// pile them onto 0:00 (review 2026-09-03, N-F2).
		msg.OffsetMs = msg.TimestampMs - baseMs
	}

	cd.messages = append(cd.messages, *msg)
	cd.totalCount++
	cd.fileCount++
	if msg.TimestampMs > cd.lastTimestampMs {
		cd.lastTimestampMs = msg.TimestampMs
	}
	total := cd.totalCount
	cd.mu.Unlock()

	// OUTSIDE the lock. This callback is ProgressTracker.SetChatCount, which
	// reaches db.UpdateJobFields under the database's FULL sync up to ~60×/s
	// on a busy channel; reporting under cd.mu queued the flusher tick,
	// RollFile and every MessageCount() behind an fsync (TWITCH-5). The
	// YouTube twin has always released first — internal/chat/downloader.go.
	cd.callOnProgress(total)
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
