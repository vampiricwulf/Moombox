package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// dumpLostChatBatch writes a boundary batch that writeBatch could not persist to
// a "<path>.lostbatch.json" sidecar so the messages are recoverable rather than
// only logged. Best-effort.
func dumpLostChatBatch(path string, batch any) error {
	data, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path+".lostbatch.json", data, 0o644)
}

// flush writes pending messages to the current part file and reports whether
// any are still pending because the write failed.
// appendChatMessages is utils.AppendChatMessages for both Twitch writers; a
// test replaces it to make an append fail the way a full disk does.
var appendChatMessages = utils.AppendChatMessages[TwitchChatMessage]

func (cd *ChatDownloader) flush() error {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()
	return cd.flushLocked()
}

// flushLocked writes pending messages to the current part file. Caller must
// hold flushMu. A non-nil return means the batch is still pending.
func (cd *ChatDownloader) flushLocked() error {
	cd.mu.Lock()
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
// (rewriteWithHistory), when it is what the salvage kept plus the batch. A
// non-nil error means nothing was written and the caller decides whether the
// batch stays pending.
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
		chatData.RecordingStartTime = time.UnixMilli(startMs).UTC().Format(time.RFC3339)
	}
	return utils.WriteChatFileAtomic(path, &chatData)
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
// Returns the closed file's path, or "" when the old part never produced a
// file — callers skip enrichment/copy in that case. Safe to call whether or
// not the downloader is running.
func (cd *ChatDownloader) RollFile(newOutputPath, newRecordingStart string) string {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()

	var newBaseMs int64
	if t, err := time.Parse(time.RFC3339, newRecordingStart); err == nil {
		newBaseMs = t.UnixMilli()
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
	cd.mu.Unlock()

	closedPath := ""
	if oldFlushed || len(batch) > 0 {
		closedPath = oldPath
	}
	if len(batch) > 0 {
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
