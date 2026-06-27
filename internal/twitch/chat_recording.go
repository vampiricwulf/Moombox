package twitch

import (
	"context"
	"encoding/json"
	"errors"
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

func (cd *ChatDownloader) flush() {
	cd.flushMu.Lock()
	defer cd.flushMu.Unlock()
	cd.flushLocked()
}

// flushLocked writes pending messages to the current part file. Caller must
// hold flushMu.
func (cd *ChatDownloader) flushLocked() {
	cd.mu.Lock()
	snapshotLen := len(cd.messages)
	msgs := make([]TwitchChatMessage, snapshotLen)
	copy(msgs, cd.messages)
	count := cd.fileCount
	flushed := cd.flushedToDisk
	path := cd.outputPath
	startMs := cd.recordingStartMs.Load()
	cd.mu.Unlock()

	if snapshotLen == 0 || path == "" {
		return
	}

	if err := cd.writeBatch(path, msgs, count, flushed, startMs); err != nil {
		// Can't write without destroying data — leave the file alone and
		// keep the batch in the in-memory buffer so the next flush retries.
		// The early return skips the truncation below.
		cd.logger.Error("write chat file failed; retrying next flush", "err", err)
		return
	}

	// Remove only the messages we successfully wrote, preserving any
	// that arrived concurrently during the write.
	cd.mu.Lock()
	cd.messages = cd.messages[snapshotLen:]
	cd.flushedToDisk = true
	cd.mu.Unlock()

	// Prune dedup set to prevent unbounded memory growth
	cd.pruneDedup()

	// Save resume state after each flush (matches TS periodicSave -> saveResumeState)
	cd.saveResumeState()
}

// writeBatch persists one batch of messages to path: full atomic write for a
// file that doesn't exist yet, append (with merge fallback) afterwards.
// Shared by the periodic flush and RollFile's final drain — the target is a
// parameter so the drain can keep writing to the CLOSED part's path after
// the in-memory state already points at the next part.
//
// A nil return means the batch is on disk (or deliberately abandoned: the
// partial-write sentinel marks the on-disk tail broken, where retry/merge
// would corrupt history). A non-nil return means nothing was written and the
// caller decides whether the batch stays pending.
func (cd *ChatDownloader) writeBatch(path string, msgs []TwitchChatMessage, count int, alreadyFlushed bool, startMs int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if !alreadyFlushed {
		// First write: complete file
		return cd.writeFullChatFileTo(path, msgs, count, startMs)
	}

	// Subsequent writes: append new messages to the existing file
	writeErr := utils.AppendChatMessages(path, msgs, cd.logger)
	if writeErr == nil {
		if err := utils.UpdateChatFileHeaderFields(path, count); err != nil {
			cd.logger.Warn("update chat header", "err", err)
		}
		return nil
	}
	if errors.Is(writeErr, utils.ErrChatFilePartialWrite) {
		// Truncated-then-failed write: the on-disk tail is broken, and the
		// merge below would parse-fail (dropping history) or splice into
		// garbage on retry. Per the sentinel's contract, advance past the
		// batch.
		cd.logger.Error("partial chat append; advancing past batch", "err", writeErr)
		return nil
	}
	// Fallback: read the existing file, merge with the current batch, and
	// rewrite. The earlier implementation passed only `msgs` to the full
	// write here, which replaced the aggregate with just the latest batch —
	// silently dropping every message from prior flushes whenever append
	// hit a transient I/O glitch.
	cd.logger.Warn("append failed, merging existing file with current batch", "err", writeErr)
	existing, readErr := readChatFileMessages(path)
	if readErr != nil {
		return readErr
	}
	merged := append(existing, msgs...)
	return cd.writeFullChatFileTo(path, merged, count, startMs)
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
	if newBaseMs > 0 {
		cd.recordingStartMs.Store(newBaseMs)
	}
	cd.mu.Unlock()

	closedPath := ""
	if oldFlushed || len(batch) > 0 {
		closedPath = oldPath
	}
	if len(batch) > 0 {
		if err := cd.writeBatch(oldPath, batch, oldCount, oldFlushed, oldBase); err != nil {
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
// single-flight concurrent callers; a failed resolve (nil) is not cached,
// letting a later part retry.
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
