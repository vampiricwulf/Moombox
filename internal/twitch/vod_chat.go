package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	vodChatMaxConsecutiveErrors = 5
	vodChatFlushInterval        = 5 * time.Second
)

// vodChatOnlinePoll is how often a paging loop parked on a connectivity outage
// re-asks the probe. A variable so tests need not sleep it out.
var vodChatOnlinePoll = 5 * time.Second

// VodChatDownloader downloads chat messages from a Twitch VOD.
//
// **Concurrency contract** (audit twitch.md #1):
//
//   - Start() owns the long-running goroutine and is the only writer of
//     `messages`. flush() and writeFullFile() are called only from Start's
//     goroutine, so the in-memory message slice is single-goroutine by
//     construction.
//   - Cross-goroutine reads use the atomic counters: `totalCount` (via
//     MessageCount), `running` (via IsRunning).
//   - `dedup` is utils.OrderedDedup which carries its own mutex.
//   - `onProgress` is guarded by `onProgressMu` (RWMutex; reassign-safe).
//
// Stop() cancels the ctx and waits via `running.Load()`; it does NOT
// touch `messages` directly. Callers must NOT read `messages` from
// outside Start's goroutine — use MessageCount() instead.
type VodChatDownloader struct {
	api          *API
	vodID        string
	channelLogin string
	channelName  string
	channelID    string
	// authToken returns the CURRENT Twitch OAuth token, re-read on every
	// comment page. Same reason as ChatDownloader.credentials: the paging loop
	// runs for the length of a VOD, and a token captured at construction goes
	// stale underneath it. nil-safe via currentAuthToken.
	authToken     func() string
	outputPath    string
	vodDuration   int                 // seconds, used for progress % estimation
	vodStartMs    int64               // epoch ms when VOD started
	messages      []TwitchChatMessage // single-writer: Start goroutine only
	dedup         *utils.OrderedDedup[string]
	totalCount    atomic.Int64
	running       atomic.Bool
	emoteResolver *EmoteResolver

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// onProgress is read from reportProgress under onProgressMu; callers
	// must use SetOnProgress rather than direct field assignment to avoid a
	// data race if the callback is reassigned after Start (audit
	// reports/worker.md F3).
	onProgressMu sync.RWMutex
	onProgress   func(count int)

	// sessionCancel aborts the page fetch in flight. Stop() fires it so a
	// goroutine parked in a GQL round trip unwinds inside the orchestrator's
	// 2 s grace instead of outliving the job's staging removal (TWITCH-11).
	// The IRC path's interruptSession is the same shape.
	sessionCancelMu sync.Mutex
	sessionCancel   context.CancelFunc

	// isOnline is the device-connectivity probe SetIsOnline installs (nil =
	// none): a page fetch that fails while it reports offline waits for the
	// network instead of spending the consecutive-error budget.
	isOnline atomic.Pointer[func() bool]

	// wroteFile records that this downloader has successfully written its
	// chat file at least once — the precondition for reading a missing output
	// directory as "the job removed staging under us" rather than "the first
	// write has not happened yet". See outputDirGone.
	wroteFile atomic.Bool
}

// SetOnProgress installs the progress callback. Safe to call before or
// after Start.
func (vcd *VodChatDownloader) SetOnProgress(fn func(count int)) {
	vcd.onProgressMu.Lock()
	vcd.onProgress = fn
	vcd.onProgressMu.Unlock()
}

// callOnProgress snapshots the callback under the lock and invokes it
// outside.
func (vcd *VodChatDownloader) callOnProgress(count int) {
	vcd.onProgressMu.RLock()
	fn := vcd.onProgress
	vcd.onProgressMu.RUnlock()
	if fn != nil {
		fn(count)
	}
}

// VodChatOptions configures the VOD chat downloader.
type VodChatOptions struct {
	VodID         string
	ChannelLogin  string
	ChannelName   string
	ChannelID     string
	AuthToken     func() string // Returns the CURRENT OAuth token, re-read per page (nil = anonymous)
	OutputPath    string
	VodDuration   int   // seconds, used for progress % estimation
	VodStartMs    int64 // epoch ms when VOD started, for computing absolute timestamps
	EmoteResolver *EmoteResolver
}

// NewVodChatDownloader creates a new VOD chat downloader.
func NewVodChatDownloader(api *API, opts VodChatOptions, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *VodChatDownloader {
	return &VodChatDownloader{
		api:           api,
		vodID:         opts.VodID,
		channelLogin:  opts.ChannelLogin,
		channelName:   opts.ChannelName,
		channelID:     opts.ChannelID,
		authToken:     opts.AuthToken,
		outputPath:    opts.OutputPath,
		vodDuration:   opts.VodDuration,
		vodStartMs:    opts.VodStartMs,
		emoteResolver: opts.EmoteResolver,
		dedup:         utils.NewOrderedDedup[string](),
		logger:        logger,
	}
}

// SetIsOnline installs the device-connectivity probe. Without it a page fetch
// failing through an outage spent the whole consecutive-error budget in about
// twenty seconds and gave the archive up, while the video beside it waited
// the outage out. Safe to call before or after Start; nil removes the probe.
func (vcd *VodChatDownloader) SetIsOnline(fn func() bool) {
	if fn == nil {
		vcd.isOnline.Store(nil)
		return
	}
	vcd.isOnline.Store(&fn)
}

// offline reports whether the installed probe says the device has no network.
func (vcd *VodChatDownloader) offline() bool {
	fn := vcd.isOnline.Load()
	return fn != nil && !(*fn)()
}

// waitOnline blocks until the probe reports the network back, polling every
// vodChatOnlinePoll, and reports false when ctx ends first.
func (vcd *VodChatDownloader) waitOnline(ctx context.Context) bool {
	t := time.NewTicker(vodChatOnlinePoll)
	defer t.Stop()
	for vcd.offline() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

// currentAuthToken reads the live OAuth token. Returns "" when no getter was
// supplied — an anonymous comment fetch, exactly as an empty token behaved.
func (vcd *VodChatDownloader) currentAuthToken() string {
	if vcd.authToken == nil {
		return ""
	}
	return vcd.authToken()
}

// Start downloads all VOD chat comments.
func (vcd *VodChatDownloader) Start(ctx context.Context) (retErr error) {
	vcd.running.Store(true)
	defer vcd.running.Store(false)
	defer func() {
		if r := recover(); r != nil {
			vcd.logger.Error("VOD chat downloader panic", "panic", r, "vodID", vcd.vodID)
			// A panic is an outcome, not a clean exit: the worker records
			// chat_status "incomplete" for it instead of "finished".
			retErr = fmt.Errorf("VOD chat downloader panic: %v", r)
		}
	}()

	// Derive a cancellable context so Stop() can abort a page fetch in flight.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	vcd.sessionCancelMu.Lock()
	vcd.sessionCancel = cancel
	vcd.sessionCancelMu.Unlock()

	vcd.logger.Info("starting VOD chat download", "vodID", vcd.vodID)

	var contentOffset float64
	// cursor is empty for the FIRST request of a run (a fresh start or a
	// resume, both of which enter by offset) and holds the previous page's
	// last edge cursor thereafter. It is deliberately not persisted: a Twitch
	// cursor is opaque and undocumented as durable, and a stale one answers
	// with an empty page that this loop reads as the end of the VOD.
	var cursor string
	consecutiveErrors := 0

	// A previous run on this instance — the orchestrator re-Starts the same
	// downloader after a connectivity outage — can have left a batch its last
	// flush could not write, with the sidecar still at the last good flush.
	// The resume below restored that sidecar's count, replaced the dedup and
	// paged again from its offset, so the same comments were buffered a second
	// time behind the first: 13 records, 7 of them distinct. The run starts
	// from what the disk holds, as a fresh downloader does, and the batch is
	// fetched again from the resume offset. The IDs and the count go with it:
	// kept where no sidecar replaces them, the dropped batch's IDs filtered out
	// every comment fetched again in its place.
	vcd.messages = nil
	vcd.dedup.Restore(nil)
	vcd.totalCount.Store(0)

	// Try loading resume state. sidecarCounted records that it says the file
	// holds messages: one that counted none had no history to lose with the
	// file (the YouTube downloader draws the same line).
	sidecarCounted := false
	if state, err := vcd.loadResumeState(); err == nil && state != nil {
		if state.StreamID == vcd.vodID {
			sidecarCounted = state.MessageCount > 0
			contentOffset = state.LastOffsetSeconds
			vcd.totalCount.Store(int64(state.MessageCount))
			vcd.dedup.Restore(state.RecentIDs)
			vcd.logger.Info("resumed VOD chat download",
				"vodID", vcd.vodID,
				"offset", contentOffset,
				"previousMessages", vcd.totalCount.Load(),
			)
		}
	}
	kept, salvaged := vcd.repairDamagedFile()
	fileGone := sidecarCounted && !salvaged && vcd.outputFileMissing()
	if salvaged || fileGone {
		// The sidecar's offset, IDs and count describe the file as it was
		// when they were saved, and the damage — or the file's loss — took
		// part of that away. Kept, the run carried on from the sidecar's
		// offset with its dedup, so the pages the disk lost were never asked
		// for again: a file cut inside its fourth comment ended with six of
		// twelve while the job read "finished". The file is what the archive
		// holds, so the run continues from it — its count, its own IDs
		// (adoptExistingFile below seeds them) and its newest offset, even
		// when that is below the sidecar's, or 0 with no file at all. The
		// YouTube downloader clears its dedup and replay mark the same way
		// (internal/chat/downloader.go).
		vcd.logger.Warn("[TwitchVodChat] the chat file no longer holds what the resume state covers; continuing from the file",
			"vodID", vcd.vodID, "sidecarOffset", contentOffset, "fileMessages", kept, "fileGone", fileGone)
		contentOffset = 0
		vcd.dedup.Restore(nil)
		vcd.totalCount.Store(int64(kept))
	}
	if fileOffset, ok := vcd.adoptExistingFile(); ok && fileOffset > contentOffset {
		// The file reaches further than the sidecar says (or there is no
		// sidecar at all): continue from the file's newest message. A run that
		// found no sidecar started from offset 0 and re-appended everything the
		// dedup's last chatDedupMax IDs did not cover — a VOD whose chat had
		// already finished, re-run on a restart while its video was still
		// downloading, came out with every message beyond 5000 twice.
		vcd.logger.Info("continuing VOD chat from the existing file's newest message",
			"vodID", vcd.vodID, "offset", fileOffset, "sidecarOffset", contentOffset)
		contentOffset = fileOffset
	}
	if salvaged || fileGone {
		// And the sidecar follows the file now, not at the first checkpoint:
		// a process killed before then left the old one beside a file that
		// reads intact, and the next run trusted it and lost the same pages.
		vcd.saveResumeState(contentOffset)
	}

	lastFlush := time.Now()

	for vcd.running.Load() {
		select {
		case <-ctx.Done():
			vcd.finishInterrupted(contentOffset)
			return nil
		default:
		}

		edges, hasNext, err := vcd.api.GetVodComments(ctx, vcd.vodID, contentOffset, cursor, vcd.currentAuthToken())
		if err != nil {
			// A cancelled session is not a fetch error. Stop() fires
			// sessionCancel while a page is in flight, so this arm saw
			// context.Canceled on EVERY mid-page stop and reported a
			// deliberate shutdown as a failure — counted against
			// vodChatMaxConsecutiveErrors and written at WARN, the default
			// log level. Finish exactly as the ctx.Done arm above does
			// (flush, sidecar, one Info line) so the outcome is still
			// `incomplete` with a consistent resume offset.
			if ctx.Err() != nil {
				vcd.finishInterrupted(contentOffset)
				return ctx.Err()
			}
			// No network is not a fault of the page: wait for it, then ask
			// for the same page again with the budget untouched.
			if vcd.offline() {
				vcd.logger.Warn("vod chat fetch failed while offline; waiting for connectivity", "err", err)
				if !vcd.waitOnline(ctx) {
					vcd.finishInterrupted(contentOffset)
					return ctx.Err()
				}
				vcd.logger.Info("connectivity restored; resuming VOD chat download", "vodID", vcd.vodID)
				consecutiveErrors = 0
				continue
			}
			consecutiveErrors++
			if consecutiveErrors >= vodChatMaxConsecutiveErrors {
				vcd.checkpoint(contentOffset)
				return fmt.Errorf("too many VOD chat errors: %w", err)
			}
			vcd.logger.Warn("vod chat fetch error", "err", err, "consecutive", consecutiveErrors)
			select {
			case <-ctx.Done():
				vcd.finishInterrupted(contentOffset)
				return ctx.Err()
			case <-time.After(2 * time.Duration(consecutiveErrors) * time.Second):
			}
			continue
		}
		consecutiveErrors = 0

		newCount := 0
		for _, edge := range edges {
			if !vcd.dedup.Add(edge.ID) {
				continue
			}

			authorName := edge.CommenterDisplayName
			if authorName == "" {
				authorName = "Deleted User"
			}

			offsetMs := int64(edge.ContentOffsetSeconds * 1000)
			timestampMs := offsetMs
			if vcd.vodStartMs > 0 {
				timestampMs = vcd.vodStartMs + offsetMs
			}

			msg := TwitchChatMessage{
				ID:           edge.ID,
				TimestampMs:  timestampMs,
				OffsetMs:     offsetMs,
				AuthorName:   authorName,
				AuthorID:     edge.CommenterID,
				AuthorBadges: edge.UserBadges,
				AuthorColor:  edge.UserColor,
				Message:      edge.MessageText,
				Emotes:       edge.Emotes,
				MessageType:  "chat",
			}

			vcd.messages = append(vcd.messages, msg)
			vcd.totalCount.Add(1)
			newCount++
		}

		// Once per page, not per message — reportProgress logs an Info line,
		// and per-message it floods the log with thousands of identical
		// entries on comment-heavy VODs.
		if newCount > 0 {
			vcd.reportProgress(contentOffset)
		}

		// Zero edges.
		if len(edges) == 0 {
			if !hasNext {
				// Genuine end of VOD comments.
				vcd.logger.Info("[TwitchVodChat] Reached end of VOD comments")
				break
			}
			// Twitch claims more pages exist (hasNextPage=true) yet sent none
			// — a stall, not completion (fix round R4). Returning an error
			// instead of breaking keeps the resume sidecar so a relaunch or a
			// restart continues from here, rather than the archive being
			// silently marked complete and truncated.
			return vcd.pagingStalled(contentOffset, cursor, "page carried zero edges with hasNextPage=true")
		}

		// If this page was entirely duplicates AND the server has no more
		// pages, we're done. Previously we broke on newCount==0 alone,
		// which killed the download on resume whenever the first page
		// back matched already-seen IDs — the rest of the VOD past that
		// offset was never archived. With hasNext==true we keep paging.
		if newCount == 0 && !hasNext {
			break
		}

		if !hasNext {
			break
		}

		// Advance by the LAST edge's cursor, not by its offset.
		// contentOffsetSeconds is an integer second, and a second of a busy
		// VOD holds more than one page of comments — so an offset-based next
		// request asks for the page just read, sees only duplicates, and used
		// to break here with the rest of the VOD's chat unarchived (T1-3).
		//
		// contentOffset keeps tracking the last edge's offset because it is
		// what the resume sidecar and the progress line are written from; it
		// is no longer what the next request is built from.
		//
		// Reaching here means hasNext is true (the two breaks above already
		// handled the false case), so every branch below is a paging
		// decision — never a completion signal (fix round R4).
		last := edges[len(edges)-1]
		switch {
		case last.Cursor == "":
			// A schema surprise, not completion: Twitch still says more
			// pages exist. Fall back to the pre-T1-3 offset paging rather
			// than stopping here; if the offset itself fails to advance
			// either, that IS a genuine stall (below).
			newOffset := last.ContentOffsetSeconds
			if newOffset <= contentOffset {
				return vcd.pagingStalled(contentOffset, cursor,
					"page carried no cursor and the offset fallback did not advance")
			}
			vcd.logger.Warn("[TwitchVodChat] page carried no cursor; falling back to offset paging",
				"offset", newOffset)
			cursor = ""
			contentOffset = newOffset
		case last.Cursor == cursor:
			// A server that answers a cursor with the page that cursor came
			// from would otherwise spin forever — a genuine stall.
			return vcd.pagingStalled(contentOffset, cursor, "cursor did not advance")
		default:
			cursor = last.Cursor
			contentOffset = last.ContentOffsetSeconds
		}

		// Periodic flush every 5 seconds
		if time.Since(lastFlush) >= vodChatFlushInterval {
			vcd.checkpoint(contentOffset)
			lastFlush = time.Now()
		}

		// Prune dedup — keep most recent chatDedupMax entries by insertion order
		if vcd.dedup.Len() > chatDedupMax*2 {
			vcd.dedup.Keep(chatDedupMax)
		}
	}

	// Distinguish Stop() (orchestrator's post-video chat timeout — pagination
	// forcibly cut short) from natural completion (loop exits via break when
	// the server has no more pages). On Stop, preserve the resume state so a
	// relaunch or a restart continues from this offset (NOT /retry, which
	// reinitializes the job and deletes staging), and skip enrichment — the
	// chat is incomplete and an enriched-then-resumed file must not be
	// re-appended. The nil below is not "finished" either: when that Stop()
	// came from the orchestrator's own chat wait expiring, resolveChatOutcome
	// (internal/worker/orchestrator_chat.go) reports the timeout rather than
	// this nil, so the row records incomplete.
	if !vcd.running.Load() {
		vcd.finishInterrupted(contentOffset)
		return nil
	}

	if err := vcd.flush(); err != nil {
		// The last page is in, but the file would not take the last batch.
		// This used to be ignored: the sidecar was removed, "download
		// complete" logged and nil returned, so the job read "finished" over
		// a chat missing its tail. Keep the sidecar at the offset the file
		// does reach (checkpoint saves only after a good flush, so the one on
		// disk is it) and report the capture incomplete.
		return fmt.Errorf("vod chat: final flush failed, %d messages not written: %w", len(vcd.messages), err)
	}

	// Resolve and inject third-party emotes (7TV, BTTV, FFZ).
	// Use a fresh context — the original ctx may already be cancelled.
	if vcd.totalCount.Load() > 0 && vcd.emoteResolver != nil && vcd.channelID != "" {
		vcd.logger.Info("resolving emotes for VOD chat", "channelID", vcd.channelID)
		emoteCtx, emoteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		emoteData := vcd.emoteResolver.Resolve(emoteCtx, vcd.channelID, vcd.channelLogin)
		emoteCancel()
		if emoteData != nil {
			if err := EnrichWithEmotes(vcd.outputPath, emoteData); err != nil {
				vcd.logger.Warn("emote injection failed", "err", err)
			}
		}
	}

	vcd.removeResumeState()
	vcd.logger.Info("VOD chat download complete", "vodID", vcd.vodID, "messages", vcd.totalCount.Load())
	return nil
}

// pagingStalled flushes buffered messages and preserves the resume sidecar,
// then returns an error naming why VOD chat paging could not continue.
//
// Callers MUST return this value directly rather than break the loop: a
// paging stall — a repeated cursor, a page with no edges despite
// hasNextPage=true, or the offset fallback failing to advance past a
// cursorless edge — is never natural completion. Reaching the post-loop
// removeResumeState()/"download complete" path on a stall would delete the
// sidecar and mark an irrecoverably truncated archive as finished (fix
// round R4); the orchestrator (internal/worker) now consumes this error too
// — chatStatusForOutcome (internal/worker/orchestrator_chat.go) turns it into
// chat_status = "incomplete" on the job row — but preserving the sidecar
// here is still the only thing that makes the stall RECOVERABLE: a relaunch
// inside the same job (the one internal/worker/orchestrator_twitch.go fires
// once a connectivity outage is over) or a restart that re-processes a job
// still in Downloading reconstructs a downloader against the same outputPath,
// which loadResumeState() reads to continue from contentOffset. NOT the
// operator's /resume: that route is restricted to YouTube jobs
// (internal/web/routes/jobs.go), and the TUI's batch chord mirrors it.
//
// That recovery holds only WHILE THE STAGING DIRECTORY DOES. outputPath is
// <staging>/chat.json (internal/worker/stream_processor_twitch.go), so the
// sidecar lives in staging too, and a job that finalizes deletes staging
// wholesale (os.RemoveAll in processJob, internal/worker/worker.go). Past
// that point there is nothing left to continue from, and the stall survives
// as the Warn below, a chat count short of the VOD, and the incomplete
// chat_status row that tells an operator the archive stopped short — which
// is why the Warn is not optional.
func (vcd *VodChatDownloader) pagingStalled(contentOffset float64, cursor, reason string) error {
	vcd.checkpoint(contentOffset)
	// The orchestrator now records this error as chat_status = "incomplete"
	// (recordChatOutcome, internal/worker/orchestrator_chat.go) — its own Warn
	// there logs this same offset/cursor/reason again, via this error's
	// string — but that record only lands if the job reaches finalization.
	// This Warn is still the only trace EMITTED AT THE MOMENT the stall
	// happens: a killed process, or a job that never finalizes, leaves only
	// this line.
	vcd.logger.Warn("[TwitchVodChat] paging stalled; resume state kept",
		"reason", reason,
		"offset", contentOffset,
		"cursor", cursor,
		"messages", vcd.MessageCount(),
	)
	return fmt.Errorf("vod chat paging stalled at offset %v cursor %q: %s", contentOffset, cursor, reason)
}

// adoptExistingFile reconciles the run with a chat file already on disk:
// the dedup learns the IDs of its last chatDedupMax messages and the total
// rises to its count. The sidecar is saved right AFTER each flush, so a
// process killed between the two left a file holding pages the sidecar's
// offset and IDs did not cover; the resumed run re-fetched those pages and
// appended them a second time, and its header count fell short. The YouTube
// and Twitch IRC downloaders already re-read the file this way on resume.
// Nothing on disk is a fresh start; a file that cannot be read is left alone
// (a damaged one was salvaged by repairDamagedFile just before this).
//
// It also returns how far into the VOD (seconds) the file's newest message
// is, so Start can continue from there when the sidecar says less — or is
// missing — rather than re-page what the file already holds.
func (vcd *VodChatDownloader) adoptExistingFile() (offsetSeconds float64, ok bool) {
	if vcd.outputPath == "" {
		return 0, false
	}
	summary, err := readChatPartFileSummary(vcd.outputPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			vcd.logger.Warn("cannot read the existing VOD chat file; resuming without its message IDs",
				"vodID", vcd.vodID, "path", vcd.outputPath, "err", err)
		}
		return 0, false
	}
	for _, id := range summary.recentIDs {
		vcd.dedup.Add(id)
	}
	if n := int64(summary.messages); n > vcd.totalCount.Load() {
		vcd.totalCount.Store(n)
	}
	if summary.messages == 0 || !summary.hasOffset {
		return 0, false
	}
	return float64(summary.maxOffsetMs) / 1000, true
}

// repairDamagedFile salvages a VOD chat file that no longer ends the way an
// append needs (a crash-torn tail, a cut mid-record). Left as it was, every
// flush failed against it and the completion path ignored that: the chat was
// lost while the job read "finished". The intact messages are kept, the
// original bytes are kept beside it as .corrupt, and the count follows.
//
// salvaged reports that the file was rewritten, and kept is how many messages
// it holds now: what the damage took may be below the sidecar's offset, and
// Start must not resume past it.
func (vcd *VodChatDownloader) repairDamagedFile() (kept int, salvaged bool) {
	if vcd.outputPath == "" {
		return 0, false
	}
	if intact, err := utils.ChatFileEndIntact(vcd.outputPath); err != nil || intact {
		return 0, false
	}
	vcd.logger.Warn("[TwitchVodChat] the existing chat file is damaged; salvaging it",
		"vodID", vcd.vodID, "path", vcd.outputPath)
	kept, err := rewriteChatFileWithHistory(vcd.outputPath, nil, vcd.logger, func(merged []TwitchChatMessage) error {
		return vcd.writeFullFileCount(merged, len(merged))
	})
	if err != nil {
		vcd.logger.Error("[TwitchVodChat] could not salvage the damaged chat file", "path", vcd.outputPath, "err", err)
		return 0, false
	}
	vcd.totalCount.Store(int64(kept))
	vcd.wroteFile.Store(true)
	return kept, true
}

// outputFileMissing reports whether the chat file is positively absent — not
// merely unreadable, which says nothing about what it holds.
func (vcd *VodChatDownloader) outputFileMissing() bool {
	if vcd.outputPath == "" {
		return false
	}
	_, err := os.Stat(vcd.outputPath)
	return errors.Is(err, fs.ErrNotExist)
}

// checkpoint flushes and, only when that wrote everything, saves the resume
// sidecar at contentOffset. Saving after a failed flush recorded an offset,
// IDs and a count that covered messages only memory held, so the next run
// never asked for those pages again and dropped their boundary as duplicates.
// Left as it was, the previous sidecar points at what the file really holds.
func (vcd *VodChatDownloader) checkpoint(contentOffset float64) {
	if err := vcd.flush(); err != nil {
		vcd.logger.Warn("[TwitchVodChat] flush failed; resume state left at the last good flush", "err", err)
		return
	}
	vcd.saveResumeState(contentOffset)
}

// finishInterrupted is the exit every path that ends BEFORE the VOD's last
// page shares: flush what is buffered and keep the resume sidecar, so a
// relaunch inside the same job (or a restart of a still-Downloading job)
// continues from this offset. It never deletes the sidecar and never enriches:
// an enriched file must not receive further appends.
//
// Reaching it through a context cancellation is new. Before Stop() cancelled
// the session, the fetch-error branch's cancel arm returned ctx.Err() with no
// flush and no save at all — which, once Stop() starts cancelling, would have
// thrown away exactly the batch the pre-cancel code preserved.
func (vcd *VodChatDownloader) finishInterrupted(contentOffset float64) {
	vcd.checkpoint(contentOffset)
	vcd.logger.Info("VOD chat download stopped before completion; resume state preserved",
		"vodID", vcd.vodID, "offset", contentOffset, "messages", vcd.totalCount.Load())
}

// outputDirGone reports whether the directory that held the chat file has been
// removed under this downloader — the finalize race of TWITCH-11: the job
// removed its staging tree while this goroutine was mid-GQL. Only meaningful
// once a file has been written; before that the directory is the worker's
// freshly-created staging dir and creating it is the ordinary first-write path.
func (vcd *VodChatDownloader) outputDirGone() bool {
	if vcd.outputPath == "" || !vcd.wroteFile.Load() {
		return false
	}
	_, err := os.Stat(filepath.Dir(vcd.outputPath))
	return err != nil && os.IsNotExist(err)
}

// flush writes the buffered messages to the chat file. A non-nil return means
// they are still buffered (or, with the output directory gone, dropped).
func (vcd *VodChatDownloader) flush() error {
	if len(vcd.messages) == 0 || vcd.outputPath == "" {
		return nil
	}
	if vcd.outputDirGone() {
		// The job finalized and removed staging while this goroutine was still
		// paging. MkdirAll here would REBUILD <staging>/<jobID>/ around a
		// chat.json and a resume sidecar nothing ever cleans (TWITCH-11).
		vcd.logger.Warn("[TwitchVodChat] output directory is gone; dropping the exit flush",
			"path", vcd.outputPath, "pending", len(vcd.messages))
		return fmt.Errorf("vod chat output directory %s is gone", filepath.Dir(vcd.outputPath))
	}

	dir := filepath.Dir(vcd.outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		vcd.logger.Error("create vod chat output dir", "err", err)
		return err
	}

	// Check if this is the first flush (file doesn't exist yet)
	_, statErr := os.Stat(vcd.outputPath)
	isFirstFlush := statErr != nil

	if isFirstFlush {
		// First flush: write complete file
		if err := vcd.writeFullFile(vcd.messages); err != nil {
			vcd.logger.Error("write vod chat file", "err", err)
			return err
		}
	} else {
		// Subsequent flushes: append new messages to existing file. The header
		// count/downloadedAt refresh is folded into AppendChatMessages' open
		// handle (warn-only on failure, as before).
		count := int(vcd.totalCount.Load())
		appendErr := appendChatMessages(vcd.outputPath, vcd.messages, count, vcd.logger)
		switch {
		case appendErr == nil:
			// header refreshed inside the append
		case errors.Is(appendErr, utils.ErrChatFilePartialWrite):
			// The write failed and the append put the file's end back: the
			// file holds none of the batch, which stays buffered for the next
			// flush. It used to be dropped while totalCount kept it.
			vcd.logger.Error("partial vod chat append; keeping the batch for the next flush", "err", appendErr)
			return appendErr
		default:
			// Rewrite with history (shared with the IRC path): rewriting
			// with only the current batch would replace hours of flushed
			// comments with the last few seconds' worth, and a damaged file
			// keeps every intact message.
			vcd.logger.Warn("append failed, rewriting the chat file with its history", "err", appendErr)
			written, err := rewriteChatFileWithHistory(vcd.outputPath, vcd.messages, vcd.logger, func(merged []TwitchChatMessage) error {
				return vcd.writeFullFileCount(merged, len(merged))
			})
			if err != nil {
				// Can't recover without destroying data — keep the batch in
				// memory and retry on the next flush.
				vcd.logger.Error("append failed and the chat file could not be rewritten; retrying next flush", "err", err)
				return err
			}
			vcd.totalCount.Store(int64(written))
		}
	}

	// Clear messages from memory after successful write to prevent unbounded growth
	vcd.messages = vcd.messages[:0]
	vcd.wroteFile.Store(true)
	return nil
}

// writeFullFile writes the complete file atomically using the shared helper.
func (vcd *VodChatDownloader) writeFullFile(msgs []TwitchChatMessage) error {
	return vcd.writeFullFileCount(msgs, int(vcd.totalCount.Load()))
}

// writeFullFileCount is writeFullFile with the header's messageCount given.
func (vcd *VodChatDownloader) writeFullFileCount(msgs []TwitchChatMessage, count int) error {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       vcd.channelLogin,
		ChannelDisplayName: vcd.channelName,
		StreamID:           vcd.vodID,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       count,
		EmoteOffsets:       chatEmoteOffsetsUTF16,
		Messages:           msgs,
	}
	return utils.WriteChatFileAtomic(vcd.outputPath, &chatData)
}

// reportProgress calls OnProgress and logs percentage if vodDuration is known.
func (vcd *VodChatDownloader) reportProgress(contentOffset float64) {
	count := int(vcd.totalCount.Load())
	vcd.callOnProgress(count)
	if vcd.vodDuration > 0 {
		pct := min(contentOffset/float64(vcd.vodDuration)*100, 100)
		vcd.logger.Info("VOD chat progress",
			"messages", count,
			"percent", fmt.Sprintf("%.1f%%", pct),
			"offsetSeconds", fmt.Sprintf("%.0f", contentOffset),
			"durationSeconds", vcd.vodDuration,
		)
	}
}

// resumeStatePath returns the sidecar resume state file path.
func (vcd *VodChatDownloader) resumeStatePath() string {
	return vcd.outputPath + ".resume.json"
}

// loadResumeState attempts to load a resume state from the sidecar JSON file.
func (vcd *VodChatDownloader) loadResumeState() (*ChatResumeState, error) {
	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// saveResumeState writes the current resume state to the sidecar JSON file.
func (vcd *VodChatDownloader) saveResumeState(contentOffset float64) {
	if vcd.outputPath == "" || vcd.outputDirGone() {
		return
	}
	// Deterministic insertion-order snapshot capped to bound the resume file.
	recentIDs := vcd.dedup.Snapshot(chatResumeIDCap)

	state := ChatResumeState{
		MessageCount:      int(vcd.totalCount.Load()),
		LastOffsetSeconds: contentOffset,
		Timestamp:         time.Now().UnixMilli(),
		StreamID:          vcd.vodID,
		RecentIDs:         recentIDs,
	}

	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	if err := store.Save(state); err != nil {
		vcd.logger.Error("save vod chat resume state", "err", err)
	}
}

// removeResumeState deletes the sidecar resume state file after successful completion.
func (vcd *VodChatDownloader) removeResumeState() {
	if vcd.outputPath == "" {
		return
	}
	store := utils.ResumeStore[ChatResumeState]{Path: vcd.resumeStatePath()}
	if err := store.Clear(); err != nil {
		vcd.logger.Warn("remove vod chat resume state", "err", err)
	}
}

// MessageCount returns the total number of messages collected.
func (vcd *VodChatDownloader) MessageCount() int {
	return int(vcd.totalCount.Load())
}

// IsRunning returns whether the downloader is currently running.
func (vcd *VodChatDownloader) IsRunning() bool {
	return vcd.running.Load()
}

// Stop cancels the VOD chat download: the loop stops paging AND the page fetch
// in flight is aborted, so the goroutine unwinds inside the orchestrator's
// grace window rather than minutes later, after staging has been removed
// (TWITCH-11). running is cleared FIRST so the exit path takes the
// "stopped before completion" branch, which preserves the resume sidecar.
func (vcd *VodChatDownloader) Stop() {
	vcd.running.Store(false)
	vcd.sessionCancelMu.Lock()
	cancel := vcd.sessionCancel
	vcd.sessionCancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// MarkStreamEnded signals that the stream has ended (no-op for VODs).
// VOD chat downloads run to completion when all pages are fetched, so
// this is intentionally a no-op.
func (vcd *VodChatDownloader) MarkStreamEnded() {
	// No-op for VOD — download completes when all pages are fetched.
}

// EnrichWithEmotes adds resolved emote data to the chat output. The rewrite
// goes through WriteChatFileAtomic (tmp + fsync + rename) — this rewrites the
// ENTIRE chat archive, and a rename journaled before the data pages hit disk
// (power loss) would otherwise replace hours of archived chat with a
// truncated file. Same threat model the periodic flush writers defend
// against.
func EnrichWithEmotes(chatPath string, emoteData *TwitchEmoteData) error {
	data, err := os.ReadFile(chatPath)
	if err != nil {
		return err
	}

	var chatData TwitchChatData
	if err := json.Unmarshal(data, &chatData); err != nil {
		return err
	}

	chatData.Emotes = emoteData
	return utils.WriteChatFileAtomic(chatPath, &chatData)
}
