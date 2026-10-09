package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// dumpLostChatBatch writes a batch that could not be written to the part file
// at path to a "<path>.lostbatch.json" sidecar, so the messages are
// recoverable rather than only logged. Best-effort.
//
// A spill already there is extended, never replaced: a part resumed after a
// restart, or one whose roll spilled before its stream-end did, can spill
// twice, and the second write used to overwrite the first's messages. A file
// at that name this cannot read as a spill is kept aside under its own
// timestamped name instead. The result is written beside it and renamed into
// place, so a failure part-way never costs the spill already on disk.
func dumpLostChatBatch[T any](path string, batch []T) error {
	spill := path + ".lostbatch.json"
	var all []T
	if data, err := os.ReadFile(spill); err == nil {
		if jsonErr := json.Unmarshal(data, &all); jsonErr != nil {
			if err := os.Rename(spill, fmt.Sprintf("%s.%d", spill, time.Now().UnixNano())); err != nil {
				return err
			}
			all = nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	all = append(all, batch...)
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := spill + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, spill); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// appendChatMessages is utils.AppendChatMessages for both Twitch writers; a
// test replaces it to make an append fail the way a full disk does.
var appendChatMessages = utils.AppendChatMessages[TwitchChatMessage]

// writeChatFile is utils.WriteChatFileAtomic for the IRC writer's full-file
// writes (writeFullChatFileTo); a test replaces it to land a message while a
// write is in flight, or to fail one the way a full disk does.
var writeChatFile = utils.WriteChatFileAtomic[*TwitchChatData]

// flush writes pending messages to the current part file, serialized with
// every other write to it by flushMu. A non-nil return means some are still
// pending because the write failed. A part still waiting for its video's
// first segment (baseAwaitPath) keeps its messages pending instead, up to
// ircPartBaseWait; see holdForPartBaseLocked. A part whose first segment was
// reported after that is rebased first; see rebaseLatePartLocked.
func (cd *ChatDownloader) flush() error {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()
	return cd.flushLocked(false)
}

// flushFinal is the flush Start's exit makes: nothing comes after it, so a
// part still waiting for its base is written against the provisional one.
func (cd *ChatDownloader) flushFinal() error {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()
	return cd.flushLocked(true)
}

// holdForPartBaseLocked reports whether the current part's messages must stay
// pending because its offset base is still provisional — the part is waiting
// for its video's first segment (AwaitPartBase) and has no file yet, so
// writing now would put a header and offsets on disk against a base that is
// about to move, and one file must keep one epoch. Past ircPartBaseWait, or on
// the final flush, the messages stop waiting — the chat must reach disk even
// when the video takes minutes to produce a first segment, or never does — and
// go out on the local-clock base, but the part stays open to its report
// (baseLatePath): one that arrives later rebases the whole file at the next
// flush or roll (rebaseLatePartLocked). Caller holds cd.mu.
func (cd *ChatDownloader) holdForPartBaseLocked(final bool) bool {
	if cd.baseAwaitPath == "" || cd.baseAwaitPath != cd.outputPath || cd.flushedToDisk || cd.partUnread {
		return false
	}
	if !final && time.Since(cd.baseAwaitSince) < cd.delays.partBaseWait {
		return true
	}
	cd.baseAwaitPath = ""
	cd.baseLatePath = cd.outputPath
	cd.logger.Info("twitch chat: no first-segment time from the part's video yet; writing on the local-clock base",
		"channel", cd.channelLogin, "path", cd.outputPath, "waited", time.Since(cd.baseAwaitSince).Round(time.Second),
		"finalFlush", final)
	return false
}

// rebaseLatePartLocked moves the current part's file onto the base its video
// reported AFTER the file went to disk (SettlePartBase's late arm, lateBaseMs):
// the part's wait ran past ircPartBaseWait — an ad break the engine skipped at
// the part's start, say — so its first messages were written on the
// provisional local-clock base. That base is not the part's: D-T8 keeps the
// local clock only for a playlist with no program date-times.
//
// The file is rewritten WHOLE, its history and the pending batch together,
// every offset recomputed from the message's own timestamp, and the header
// carrying the reported base — one atomic write, so the file holds one epoch
// before it and one after, never two. Only then do recordingStartMs and the
// messages that arrived during the write move onto the new base, in one cd.mu
// section with the swap, the way SettlePartBase rebases a held batch. A write
// that fails changes nothing — the file and the in-memory base still agree on
// the provisional base — and the next flush tries again. Caller holds flushMu.
func (cd *ChatDownloader) rebaseLatePartLocked() {
	cd.mu.Lock()
	base, path := cd.lateBaseMs, cd.outputPath
	if base == 0 || path == "" || cd.partUnread {
		cd.mu.Unlock()
		return
	}
	snapshotLen := len(cd.messages)
	msgs := make([]TwitchChatMessage, snapshotLen)
	copy(msgs, cd.messages)
	count := cd.fileCount
	provisional := cd.recordingStartMs.Load()
	cd.mu.Unlock()

	written, err := rewriteChatFileWithHistory(path, msgs, cd.logger, func(merged []TwitchChatMessage) error {
		for i := range merged {
			merged[i].OffsetMs = merged[i].TimestampMs - base
		}
		return cd.writeFullChatFileTo(path, merged, len(merged), base)
	})
	if err != nil {
		cd.logger.Warn("twitch chat: could not rebase the part onto its first segment's time yet; retrying next flush",
			"channel", cd.channelLogin, "path", path, "err", err)
		return
	}

	cd.mu.Lock()
	cd.lateBaseMs = 0
	cd.recordingStartMs.Store(base)
	cd.messages = cd.messages[snapshotLen:]
	for i := range cd.messages {
		cd.messages[i].OffsetMs = cd.messages[i].TimestampMs - base
	}
	// The file now holds the batch too; the counters follow the file, as
	// flushLocked's do after a salvage rewrite.
	if delta := written - count; delta != 0 {
		cd.fileCount += delta
		cd.totalCount = max(cd.totalCount+delta, 0)
	}
	cd.mu.Unlock()
	cd.logger.Info("twitch chat: part rebased onto its first segment's program date-time after the wait",
		"channel", cd.channelLogin, "path", path, "baseMs", base, "shiftMs", provisional-base, "messages", written)
}

// flushLocked writes pending messages to the current part file. Caller must
// hold flushMu. A non-nil return means the batch is still pending. final is
// flushFinal's: see holdForPartBaseLocked.
func (cd *ChatDownloader) flushLocked(final bool) error {
	cd.rebaseLatePartLocked()
	cd.mu.Lock()
	if cd.holdForPartBaseLocked(final) {
		cd.mu.Unlock()
		return nil
	}
	snapshotLen := len(cd.messages)
	msgs := make([]TwitchChatMessage, snapshotLen)
	copy(msgs, cd.messages)
	count := cd.fileCount
	flushed := cd.flushedToDisk
	unread := cd.partUnread
	path := cd.outputPath
	startMs := cd.recordingStartMs.Load()
	cd.mu.Unlock()

	if snapshotLen == 0 || path == "" {
		return nil
	}

	if !flushed && unread {
		// The part file was there at Start and could not be read (an AV lock,
		// a sharing violation), so it was neither adopted nor moved aside.
		// The first write would be a FULL one and replace its history with
		// this batch: adopt it now instead, or keep the batch until it can be.
		n, stillUnread := cd.adoptUnreadPart()
		if stillUnread {
			err := fmt.Errorf("part file %s still unreadable; holding %d messages", path, snapshotLen)
			cd.logger.Warn("twitch chat: not writing over a part file that cannot be read yet", "err", err)
			return err
		}
		cd.mu.Lock()
		flushed = cd.flushedToDisk
		startMs = cd.recordingStartMs.Load()
		// Snapshotted again: the adoption rebased them onto the file's clock.
		copy(msgs, cd.messages[:snapshotLen])
		cd.mu.Unlock()
		count += n
	}

	written, err := cd.writeBatch(path, msgs, count, flushed, startMs)
	if err != nil {
		// Can't write without destroying data — leave the file alone and
		// keep the batch in the in-memory buffer so the next flush retries.
		// The early return skips the truncation below.
		cd.logger.Error("write chat file failed; retrying next flush", "err", err)
		return err
	}

	// Remove only the messages we successfully wrote, preserving any
	// that arrived concurrently during the write. A salvage rewrite
	// (writeBatch) can leave the file holding a different number than the
	// counters expected; the counters follow the file.
	cd.mu.Lock()
	cd.messages = cd.messages[snapshotLen:]
	cd.flushedToDisk = true
	if delta := written - count; delta != 0 {
		cd.fileCount += delta
		cd.totalCount = max(cd.totalCount+delta, 0)
	}
	cd.mu.Unlock()

	// Prune dedup set to prevent unbounded memory growth
	cd.pruneDedup()

	// Save resume state after a flush, no more often than
	// delays.resumeSaveFloor (owner ruling; see ircResumeSaveFloor).
	cd.saveResumeStateThrottled()
	return nil
}

// writeBatch persists one batch of messages to path: full atomic write for a
// file that doesn't exist yet, append (with merge fallback) afterwards.
// Shared by the periodic flush and RollFile's final drain — the target is a
// parameter so the drain can keep writing to the CLOSED part's path after
// the in-memory state already points at the next part.
//
// A nil error means the batch is on disk, and written is the number of
// messages the file now holds: count, unless a damaged file had to be salvaged
// (rewriteChatFileWithHistory), when it is what the salvage kept plus the
// batch. A non-nil error means nothing was written and the caller decides
// whether the batch stays pending.
func (cd *ChatDownloader) writeBatch(path string, msgs []TwitchChatMessage, count int, alreadyFlushed bool, startMs int64) (written int, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}

	if !alreadyFlushed {
		// First write: complete file
		return count, cd.writeFullChatFileTo(path, msgs, count, startMs)
	}

	// Subsequent writes: append new messages to the existing file. The header
	// count/downloadedAt refresh is folded into AppendChatMessages' open handle
	// (warn-only on failure, as before).
	writeErr := appendChatMessages(path, msgs, count, cd.logger)
	if writeErr == nil {
		return count, nil
	}
	if errors.Is(writeErr, utils.ErrChatFilePartialWrite) {
		// The write failed and the append put the file's end back: the file
		// holds none of the batch, which stays pending for the next flush. It
		// used to be dropped here while the counters kept it, so the header
		// and the job total over-counted the file for good.
		return 0, writeErr
	}
	cd.logger.Warn("append failed, rewriting the part with its history", "err", writeErr)
	return rewriteChatFileWithHistory(path, msgs, cd.logger, func(merged []TwitchChatMessage) error {
		return cd.writeFullChatFileTo(path, merged, len(merged), startMs)
	})
}

// rewriteChatFileWithHistory is the append fallback both Twitch writers share:
// the file is written whole, its history ahead of the batch. The history is
// read by utils.SalvageChatMessages, so a damaged file — the zero tail a crash
// leaves, a cut mid-record — keeps every intact message, and its original
// bytes are kept beside it as <path>.corrupt first. The fallback used to read
// the file with a full Unmarshal, which fails on exactly those files, so the
// batch was retried every flush and never written. A batch message the file
// already holds is not written twice. A file that cannot be read at all is
// left alone (error). Returns the number of messages written.
func rewriteChatFileWithHistory(path string, msgs []TwitchChatMessage, logger interface {
	Error(msg string, args ...any)
}, write func([]TwitchChatMessage) error) (int, error) {
	existing, damaged, readErr := utils.SalvageChatMessages[TwitchChatMessage](path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return 0, readErr
	}
	if damaged {
		corruptPath := path + chatCorruptSuffix
		logger.Error("twitch chat: chat file damaged; keeping its intact messages and the original",
			"path", path, "kept", len(existing), "preservedAs", corruptPath)
		if err := utils.PreserveFileCopy(path, corruptPath); err != nil {
			logger.Error("twitch chat: could not preserve the damaged chat file", "path", path, "err", err)
		}
	}
	onDisk := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		if m.ID != "" {
			onDisk[m.ID] = struct{}{}
		}
	}
	merged := existing
	for _, m := range msgs {
		if _, dup := onDisk[m.ID]; dup && m.ID != "" {
			continue
		}
		merged = append(merged, m)
	}
	if merged == nil {
		merged = []TwitchChatMessage{}
	}
	if err := write(merged); err != nil {
		return 0, err
	}
	return len(merged), nil
}

// writeFullChatFileTo writes all messages as a complete JSON file atomically.
// path and startMs are parameters (not read from cd) so RollFile's drain can
// target the closed part with its own recording base.
func (cd *ChatDownloader) writeFullChatFileTo(path string, msgs []TwitchChatMessage, count int, startMs int64) error {
	chatData := TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       cd.channelLogin,
		ChannelDisplayName: cd.channelDisplay,
		StreamID:           cd.streamID,
		StreamStartTime:    cd.streamStartTime,
		DownloadedAt:       time.Now().UTC().Format(time.RFC3339),
		MessageCount:       count,
		EmoteOffsets:       chatEmoteOffsetsUTF16,
		Messages:           msgs,
	}
	if startMs > 0 {
		// RFC3339Nano, not RFC3339: a base from the first segment's program
		// date-time (SettlePartBase) carries milliseconds, and a header that
		// dropped them would hand a resumed part (adoptPartRecordingBase) a
		// base up to a second off the one its offsets were computed against.
		// A whole-second base prints exactly as RFC3339 did.
		chatData.RecordingStartTime = time.UnixMilli(startMs).UTC().Format(time.RFC3339Nano)
	}
	return writeChatFile(path, &chatData)
}

// pruneDedup trims the dedup to keep only the most recent chatDedupMax entries.
func (cd *ChatDownloader) pruneDedup() {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	cd.dedup.Keep(chatDedupMax)
}

// RollFile closes the current part's chat file and redirects recording to
// newOutputPath, with offsets rebased to newRecordingStart (the new video
// part's capture start, RFC 3339). Called by the orchestrator at every part
// boundary — gap split, quality split, or resuming a restarted job into a
// later part's staging dir — so each chat file replays in sync against its
// own video part.
//
// The boundary is ATOMIC with respect to addMessage: the pending-message
// drain, counter capture, and output/base swap happen in one cd.mu critical
// section, and addMessage computes OffsetMs under the same lock — so every
// message's offset base matches the file it is written to. Messages parsed
// before the section land in the closing file with old-base offsets;
// messages after land in the new file with new-base offsets. No straddlers.
//
// The drained batch is then written to the CLOSED path (unlike the periodic
// flush, a failure here cannot leave the batch pending — it would later be
// flushed into the wrong file — so a persistent write failure drops the
// boundary batch with an error log). The closed part's resume state is
// removed (the part is final). The dedup set and the cumulative totalCount
// survive the roll: IRC reconnect replays must not duplicate across a
// boundary, and the job-level metric keeps counting.
//
// A part Start could not read (partUnread) is adopted first, as the flush
// would adopt it (adoptUnreadPart). The drain used to ignore the flag and take
// the first-write path flushLocked is forbidden for such a part, writing the
// batch over the history it could not see and then clearing the flag: a
// 40-message part came out holding the 5 of its boundary batch. While the file
// still cannot be read, the batch is spilled to <path>.lostbatch.json instead
// and the file is left as it is.
//
// Returns the closed file's path, or "" when the old part never produced a
// file — callers skip enrichment/copy in that case. A part file left unread is
// one it produced: it holds the part's history, and the part's mux must copy
// it (or fail and retry) rather than be recorded without it. Safe to call
// whether or not the downloader is running.
func (cd *ChatDownloader) RollFile(newOutputPath, newRecordingStart string) string {
	return cd.rollFile(newOutputPath, newRecordingStart, false)
}

// RollFileAwaitingBase is RollFile for a part whose video starts a fresh file:
// newRecordingStart is only the PROVISIONAL base, and the new part waits for
// its video's first segment to supply the real one (SettlePartBase) — in the
// same critical section as the swap, so no flush can write the new part
// against the provisional base in between.
func (cd *ChatDownloader) RollFileAwaitingBase(newOutputPath, newRecordingStart string) string {
	return cd.rollFile(newOutputPath, newRecordingStart, true)
}

func (cd *ChatDownloader) rollFile(newOutputPath, newRecordingStart string, awaitBase bool) string {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()

	var newBaseMs int64
	if t, err := time.Parse(time.RFC3339, newRecordingStart); err == nil {
		newBaseMs = t.UnixMilli()
	}

	// A late report still pending for the part being closed is applied
	// first, so the drain below appends to a file already on the reported
	// base, with the base it then holds.
	cd.rebaseLatePartLocked()

	// Before the boundary section, while outputPath is still the old part:
	// the adoption reads and seeds it by that path, and rebases the pending
	// batch onto the old part's own clock.
	cd.mu.Lock()
	oldUnread := cd.partUnread && !cd.flushedToDisk
	cd.mu.Unlock()
	if oldUnread {
		_, oldUnread = cd.adoptUnreadPart()
	}

	cd.mu.Lock()
	batch := cd.messages
	cd.messages = nil
	oldPath := cd.outputPath
	oldCount := cd.fileCount
	oldFlushed := cd.flushedToDisk
	oldBase := cd.recordingStartMs.Load()
	cd.outputPath = newOutputPath
	cd.fileCount = 0
	cd.flushedToDisk = false
	cd.partUnread = false // the new part's file is this downloader's own
	// The new part has no sidecar yet, so the floor must not carry across
	// the boundary: its first flush has to write one.
	cd.lastResumeSave = time.Time{}
	if newBaseMs > 0 {
		cd.recordingStartMs.Store(newBaseMs)
	}
	// A closed part still waiting for its video's first segment is drained
	// below against its provisional base, which is now its base for good.
	oldAwaited := cd.baseAwaitPath == oldPath
	cd.baseAwaitPath = ""
	// A late base the rebase above could not write dies with the part: the
	// closed file keeps the provisional base its drain is written against.
	oldLateUnwritten := cd.lateBaseMs != 0
	cd.baseLatePath, cd.lateBaseMs = "", 0
	if awaitBase {
		cd.baseAwaitPath = newOutputPath
		cd.baseAwaitSince = time.Now()
	}
	cd.mu.Unlock()
	if oldAwaited && len(batch) > 0 {
		cd.logger.Info("twitch chat: part closed before its video reported a first segment; keeping its local-clock base",
			"channel", cd.channelLogin, "path", oldPath)
	}
	if oldLateUnwritten {
		cd.logger.Warn("twitch chat: part closed before its late first-segment time could be written; keeping its local-clock base",
			"channel", cd.channelLogin, "path", oldPath)
	}

	closedPath := ""
	if oldFlushed || len(batch) > 0 || oldUnread {
		closedPath = oldPath
	}
	if len(batch) > 0 && oldUnread {
		cd.logger.Error("twitch chat: the closed part still cannot be read; spilling its boundary batch instead of writing over it",
			"path", oldPath, "messages", len(batch))
		cd.noteRollUnwritten(len(batch))
		if dumpErr := dumpLostChatBatch(oldPath, batch); dumpErr != nil {
			cd.logger.Warn("could not spill lost chat batch to sidecar", "path", oldPath, "err", dumpErr)
		} else {
			cd.logger.Info("lost chat batch spilled to sidecar for recovery", "path", oldPath+".lostbatch.json")
		}
	} else if len(batch) > 0 {
		written, err := cd.writeBatch(oldPath, batch, oldCount, oldFlushed, oldBase)
		if err == nil && written != oldCount {
			// A salvage rewrite of the closed part: the job total follows the
			// file, as flushLocked's does.
			cd.mu.Lock()
			cd.totalCount = max(cd.totalCount+written-oldCount, 0)
			cd.mu.Unlock()
		}
		if err != nil {
			cd.logger.Error("final drain of rolled chat part failed; boundary batch lost",
				"path", oldPath, "messages", len(batch), "err", err)
			cd.noteRollUnwritten(len(batch))
			// Spill the un-writable batch to a sidecar so the messages are
			// recoverable rather than only logged. Best-effort: a failure here
			// just leaves the log line as the only record.
			if dumpErr := dumpLostChatBatch(oldPath, batch); dumpErr != nil {
				cd.logger.Warn("could not spill lost chat batch to sidecar", "path", oldPath, "err", dumpErr)
			} else {
				cd.logger.Info("lost chat batch spilled to sidecar for recovery", "path", oldPath+".lostbatch.json")
			}
			if !oldFlushed {
				closedPath = "" // nothing ever reached disk for this part
			}
		}
	} else if oldFlushed && !oldUnread {
		// Nothing to drain, so writeBatch's salvage never reads the closed
		// part — and it can be torn: a roll that lands between Start's resume
		// and its repair (repairDamagedPart) found the restored sidecar, the
		// path already swapped, or no salvage yet, and handed the torn file to
		// the part's mux and enrichment. Salvage it here, under the flushMu
		// the roll holds.
		if kept, salvaged := cd.salvageDamagedPartLocked(oldPath, oldBase, "the closed part file is damaged; salvaging it"); salvaged {
			cd.mu.Lock()
			cd.totalCount = max(cd.totalCount+kept-oldCount, 0)
			cd.mu.Unlock()
		}
	}

	if closedPath != "" {
		// The closed part is final — its resume state must not be picked up
		// by any future session.
		store := utils.ResumeStore[ChatResumeState]{Path: chatResumePath(closedPath)}
		if err := store.Clear(); err != nil {
			cd.logger.Warn("remove rolled chat resume state", "err", err)
		}
		cd.logger.Info("[TwitchChat] Rolled chat file for new part",
			"closed", filepath.Base(closedPath), "next", filepath.Base(newOutputPath))
	}
	return closedPath
}

// AwaitPartBase marks the current part's offset base PROVISIONAL: the local
// clock at the part's start, standing in until the part's video reports the
// program date-time of the first segment it writes (SettlePartBase). Until
// then the periodic flush holds the part's messages, up to ircPartBaseWait, so
// the part file's header is written with the base its offsets were computed
// against (owner decision D-T8); a report that comes after the wait rewrites
// the whole file onto it (rebaseLatePartLocked). For the job's first part —
// RollFileAwaitingBase is the same mark for every later one — and called
// before Start.
//
// A part that already has a file keeps that file's base whatever arrives
// later, the "one file, one epoch" rule adoptPartRecordingBase states, so the
// mark is ignored for one, and a part Start then finds on disk drops it.
func (cd *ChatDownloader) AwaitPartBase() {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if cd.outputPath == "" || cd.flushedToDisk || cd.partUnread {
		return
	}
	cd.baseAwaitPath = cd.outputPath
	cd.baseAwaitSince = time.Now()
}

// SettlePartBase gives the part at path the base its video reported: the
// #EXT-X-PROGRAM-DATE-TIME of the first segment written into the part's video
// file (engine.DownloaderOptions.OnFirstSegment). That segment is the part's
// first frame, so pinning offsets to it lines chat up with the video, where the
// local clock at the roll put every message late by however far behind the
// live edge the downloader joined the playlist window.
//
// The messages captured while the part waited were offset against the
// provisional base; they are rebased onto the reported one under cd.mu, the
// lock addMessage computes offsets under, exactly as adoptUnreadPart rebases a
// held batch onto an adopted file's base — so the ones held and the ones still
// to come share one clock, and the first write puts that clock in the header.
//
// A report that comes after the wait ran out (ircPartBaseWait) still counts:
// the part's first messages are on disk by then, on the provisional base, so
// the base is only recorded here (lateBaseMs) and the next flush or roll
// rewrites the file onto it whole (rebaseLatePartLocked) — not here, on the
// download goroutine, which must not block on a write.
//
// A zero time — the playlist carries no program date-times — ends the wait on
// the provisional base, today's local-clock behaviour. A report for any part
// other than the one waiting is ignored: a late report from a downloader that
// has since been replaced, a part already rolled away, or one whose file was
// on disk before it began to wait — a resumed part, adopted at Start (one
// file, one epoch).
func (cd *ChatDownloader) SettlePartBase(path string, programDateTime time.Time) {
	cd.mu.Lock()
	waiting := cd.baseAwaitPath == path && !cd.flushedToDisk
	late := cd.baseLatePath == path
	if path == "" || cd.outputPath != path || cd.partUnread || (!waiting && !late) {
		cd.mu.Unlock()
		return
	}
	cd.baseAwaitPath, cd.baseLatePath = "", ""
	if programDateTime.IsZero() {
		cd.mu.Unlock()
		cd.logger.Debug("twitch chat: the part's playlist carries no program date-time; keeping the local-clock base",
			"channel", cd.channelLogin, "path", path)
		return
	}
	provisional := cd.recordingStartMs.Load()
	base := programDateTime.UnixMilli()
	if cd.flushedToDisk {
		cd.lateBaseMs = base
		cd.mu.Unlock()
		cd.logger.Info("twitch chat: the part's first-segment time arrived after its file was written; rebasing the file",
			"channel", cd.channelLogin, "path", path, "baseMs", base, "shiftMs", provisional-base)
		return
	}
	cd.recordingStartMs.Store(base)
	for i := range cd.messages {
		cd.messages[i].OffsetMs = cd.messages[i].TimestampMs - base
	}
	held := len(cd.messages)
	cd.mu.Unlock()
	cd.logger.Info("twitch chat: part base set from its first segment's program date-time",
		"channel", cd.channelLogin, "path", path, "baseMs", base,
		"shiftMs", provisional-base, "heldMessages", held)
}

// noteRollUnwritten records n boundary-batch messages RollFile could not write
// to the part it closed. They are in no part file, so they leave the job total
// — which follows what the files hold, as flushLocked's salvage arm does — and
// they make the capture incomplete on Start's way out (rollUnwrittenErr).
func (cd *ChatDownloader) noteRollUnwritten(n int) {
	cd.mu.Lock()
	cd.rollUnwritten += n
	cd.totalCount = max(cd.totalCount-n, 0)
	cd.mu.Unlock()
}

// rollUnwrittenErr is the verdict noteRollUnwritten's count puts on Start's
// exit: nil while every roll wrote its boundary batch, otherwise the error
// naming how many messages are in no part file. Start returns it from the
// stream-end drain and from an interrupted exit alike, because both can be
// the exit a job finalizes on.
func (cd *ChatDownloader) rollUnwrittenErr() error {
	cd.mu.Lock()
	unwritten := cd.rollUnwritten
	cd.mu.Unlock()
	if unwritten <= 0 {
		return nil
	}
	return fmt.Errorf("twitch chat: %d messages could not be written to their part at a part boundary; see the part's chat.json.lostbatch.json", unwritten)
}

// resolveEmotesCached resolves third-party emotes (7TV/BTTV/FFZ) once per
// downloader and caches the result, so multi-part jobs don't re-hit the
// emote APIs for every part. emoteMu is held across the resolve to
// single-flight concurrent callers.
//
// A resolve in which NO provider answered returns nil (EmoteResolver.Resolve),
// which this leaves uncached so a later part retries. That sentence was here
// before the resolver could produce it: until 2026-09-15 a total failure came
// back as a non-nil empty set, which latched here for the life of the job AND
// in the resolver for the life of the process (T1-11). Both layers now turn on
// the same fact.
func (cd *ChatDownloader) resolveEmotesCached(ctx context.Context) *TwitchEmoteData {
	if cd.emoteResolver == nil || cd.channelID == "" {
		return nil
	}
	cd.emoteMu.Lock()
	defer cd.emoteMu.Unlock()
	if cd.emoteData != nil {
		return cd.emoteData
	}
	cd.emoteData = cd.emoteResolver.Resolve(ctx, cd.channelID, cd.channelLogin)
	return cd.emoteData
}

// EnrichFile injects third-party emotes into a closed part's chat file.
// Called by the orchestrator from the background part-mux goroutine after
// RollFile — a rolled file receives no further appends, which is the
// precondition for enrichment (appends after enrichment would splice into
// the emotes block). No-op when the file path is empty or resolve fails.
func (cd *ChatDownloader) EnrichFile(ctx context.Context, path string) {
	if path == "" {
		return
	}
	emoteData := cd.resolveEmotesCached(ctx)
	if emoteData == nil {
		return
	}
	if err := EnrichWithEmotes(path, emoteData); err != nil {
		cd.logger.Warn("emote injection failed for part chat", "path", path, "err", err)
	}
}
