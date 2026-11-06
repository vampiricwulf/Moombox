package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// Chat completeness (G36, owner decision D16: MARKERS + RAW SIDECAR).
//
// yt-dlp's live chat downloader writes every action of every continuation
// verbatim, one JSON per line (downloader/youtube_live_chat.py b77c9304e,
// 47-52 for a replay, 83-94 live), so a deletion, a retraction, a ban, a
// pinned banner, a poll or a ticker all survive in its live_chat.json. This
// kept only the messages, and a message deleted after it was sent stayed in
// the archive with nothing to say so.
//
// Now the three actions that rewrite stored messages are applied to them as
// MARKERS — the message is kept exactly as sent and flagged (Deleted,
// DeletedState; Replaced, Replacement) — and every action that did not become
// a message is appended verbatim to the raw sidecar, <chat file less
// ".json">.raw.jsonl ("chat.raw.jsonl" in staging), one per line as yt-dlp
// writes them. Moombox goes beyond upstream in keeping the messages parsed
// and the markers applied, so the dashboard's viewer and every export read
// them without the sidecar; a file without markers reads exactly as before.

// maxPendingChatMarkers bounds the markers held for messages already on disk
// until the end-of-run rewrite applies them (applyPendingMarkers). A stream
// that moderates more than this loses the excess as flags only: every marker
// is in the raw sidecar regardless.
const maxPendingChatMarkers = 20000

// maxPendingRawActions bounds the raw sidecar lines buffered while the chat
// file has no path yet (early chat, before staging exists). Past it the
// oldest are dropped.
const maxPendingRawActions = 20000

// pendingChatMarker is a marker whose message may already be on disk, held
// for the end-of-run rewrite. onDisk is how many messages the file held when
// it arrived: an author's ban sweeps the messages BEFORE it, not the ones the
// author sends after a timeout ends.
type pendingChatMarker struct {
	marker ChatMarker
	onDisk int
}

// applyMarker applies m to msgs[:limit] and reports whether it changed one.
// A deletion or replacement names one message, searched from the newest (it
// is almost always recent); an author's sweep flags every message of theirs.
func applyMarker(msgs []ChatMessage, m ChatMarker, limit int) bool {
	limit = min(limit, len(msgs))
	switch m.Kind {
	case MarkerAuthorDeleted:
		hit := false
		for i := range msgs[:limit] {
			if msgs[i].AuthorChannelID == m.AuthorChannelID {
				msgs[i].Deleted = true
				if m.State != "" {
					msgs[i].DeletedState = m.State
				}
				hit = true
			}
		}
		return hit
	case MarkerDeleted, MarkerReplaced:
		for i := limit - 1; i >= 0; i-- {
			if msgs[i].ID != m.TargetID {
				continue
			}
			if m.Kind == MarkerDeleted {
				msgs[i].Deleted = true
				if m.State != "" {
					msgs[i].DeletedState = m.State
				}
			} else {
				msgs[i].Replaced = true
				msgs[i].Replacement = m.Replacement
			}
			return true
		}
	}
	return false
}

// applyMarkers applies a batch's markers to the messages still buffered, and
// holds for the end-of-run rewrite every marker that may name a message the
// file already holds: an author sweep always (their earlier messages may be
// anywhere), a single-message one when the buffer does not have it.
func (cd *ChatDownloader) applyMarkers(markers []ChatMarker) {
	if len(markers) == 0 {
		return
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	onDisk := cd.messageCount - len(cd.messages)
	for _, m := range markers {
		hit := applyMarker(cd.messages, m, len(cd.messages))
		if !cd.flushedToDisk || onDisk <= 0 || (hit && m.Kind != MarkerAuthorDeleted) {
			continue
		}
		if len(cd.pendingMarkers) >= maxPendingChatMarkers {
			cd.logDebug("chat: too many markers held for the end-of-run rewrite; this one is in the raw sidecar only",
				"videoID", cd.opts.VideoID, "kind", m.Kind)
			continue
		}
		cd.pendingMarkers = append(cd.pendingMarkers, pendingChatMarker{marker: m, onDisk: onDisk})
	}
}

// applyPendingMarkers rewrites the chat file once, at the end of a run, with
// the markers that arrived for messages already on disk. The file is
// append-only while the run polls (a mid-run rewrite of a marathon chat would
// cost its whole size per deletion), so this is the one place they land.
// Skipped — the raw sidecar still holds every marker — when the buffer was
// not flushed (the rewrite would drop it), or the file cannot be read whole.
func (cd *ChatDownloader) applyPendingMarkers() {
	pending := cd.pendingMarkers
	cd.pendingMarkers = nil
	if len(pending) == 0 || !cd.flushedToDisk || len(cd.messages) > 0 {
		return
	}
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return
	}
	existing, damaged, err := utils.SalvageChatMessages[ChatMessage](outputFile)
	if err != nil || damaged {
		cd.logInfo("chat: could not read the chat file to apply its deletion markers; they are in the raw sidecar",
			"videoID", cd.opts.VideoID, "markers", len(pending), "err", err, "damaged", damaged)
		return
	}
	changed := false
	for _, p := range pending {
		limit := len(existing)
		if p.marker.Kind == MarkerAuthorDeleted {
			limit = p.onDisk
		}
		if applyMarker(existing, p.marker, limit) {
			changed = true
		}
	}
	if !changed {
		return
	}
	cd.mu.Lock()
	prevCount := cd.messageCount
	cd.messages, cd.messageCount = existing, len(existing)
	cd.mu.Unlock()
	ok := cd.writeFullChatFile()
	cd.mu.Lock()
	cd.messages = nil
	if !ok {
		cd.messageCount = prevCount
	}
	cd.mu.Unlock()
}

// rawSidecarPath is the raw sidecar beside a chat file: "chat.json" →
// "chat.raw.jsonl". A replay re-run's temporary file shares the archive's
// sidecar — the sidecar is a log of what the endpoint served, and the
// re-run's file is swapped in or thrown away whole while the log is not.
func rawSidecarPath(chatFile string) string {
	base := strings.TrimSuffix(chatFile, replayRerunSuffix)
	return strings.TrimSuffix(base, ".json") + ".raw.jsonl"
}

// queueRawActions buffers a batch's raw actions for the sidecar.
func (cd *ChatDownloader) queueRawActions(raw []json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	cd.rawPending = append(cd.rawPending, raw...)
	if over := len(cd.rawPending) - maxPendingRawActions; over > 0 {
		cd.rawPending = cd.rawPending[over:]
	}
}

// flushRawActions appends the buffered raw actions to the sidecar, one JSON
// per line. Best effort: the sidecar is the lossless extra, and a write it
// cannot make costs the chat file nothing — the lines stay buffered for the
// next flush.
func (cd *ChatDownloader) flushRawActions() {
	if len(cd.rawPending) == 0 {
		return
	}
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return // no staging yet (early chat): keep them buffered
	}
	path := rawSidecarPath(outputFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		cd.logDebug("chat: raw sidecar open failed", "file", path, "err", err)
		return
	}
	var b strings.Builder
	for _, line := range cd.rawPending {
		b.Write(line)
		b.WriteByte('\n')
	}
	_, werr := f.WriteString(b.String())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		cd.logDebug("chat: raw sidecar write failed", "file", path, "err", fmt.Sprint(werr))
		return
	}
	cd.rawPending = nil
}
