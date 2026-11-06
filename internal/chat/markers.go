package chat

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
//
// It returns, as messages, the replacements whose target was never stored
// (owner decision FE-14), for processBatch to commit. YouTube replaces an
// item Moombox never kept — a liveChatPlaceholderItemRenderer, shown while a
// message is held for review, is the usual one — with the message itself,
// and a marker on nothing dropped that message from chat.json, the raw
// sidecar its only copy. Moombox goes beyond yt-dlp here as everywhere
// markers are concerned: upstream writes the action verbatim and stores no
// messages (youtube_live_chat.py b77c9304e). "Never stored" is decided now
// when it can be: nothing is on disk, or the dedup still holds the ID of
// every message stored (dedup.Len() >= messageCount — it holds no other) and
// not this one. Otherwise — the target may be on disk past the dedup's
// window — the marker is held with the message, and applyPendingMarkers
// stores it at the end of the run if no message on disk is its target.
func (cd *ChatDownloader) applyMarkers(markers []ChatMarker) (orphans []ChatMessage) {
	if len(markers) == 0 {
		return nil
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	onDisk := cd.messageCount - len(cd.messages)
	for _, m := range markers {
		hit := applyMarker(cd.messages, m, len(cd.messages))
		if !hit && m.Kind == MarkerReplaced && m.Message != nil {
			if !cd.flushedToDisk || onDisk <= 0 || (!cd.dedup.Seen(m.TargetID) && cd.dedup.Len() >= cd.messageCount) {
				orphans = append(orphans, *m.Message)
				continue
			}
		}
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
	return orphans
}

// applyPendingMarkers rewrites the chat file once, at the end of a run, with
// the markers that arrived for messages already on disk. The file is
// append-only while the run polls (a mid-run rewrite of a marathon chat would
// cost its whole size per deletion), so this is the one place they land.
// Skipped — the raw sidecar still holds every marker — when the buffer was
// not flushed (the rewrite would drop it), or the file cannot be read.
//
// STREAMED, like readChatFileAdoptionSummary and for the same reason: an
// author sweep is held for every ban and timeout, so nearly every moderated
// live chat ends here, and a marathon's chat.json runs to hundreds of
// megabytes. A first pass reads only each message's id and author to see
// whether any held marker names one (a ban of an author with nothing on
// disk changes nothing and costs no write); only then does a second pass
// decode the messages one at a time, flag them, and append them
// markerRewriteBatch at a time to a temp file that is swapped in. Peak
// memory is one batch, never the file.
//
// A held replacement that carries its item (applyMarkers) makes the first
// pass read the whole file, not stop at the first match: a replacement no
// message on disk is the target of is stored as a message of its own (owner
// decision FE-14), committed and appended like any other — the player sorts
// a chat by offset, so its place at the end of the array is no matter.
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
	held := indexHeldMarkers(pending)
	var orphanable map[string]bool // a held replacement's target: found on disk
	for _, p := range pending {
		if p.marker.Kind == MarkerReplaced && p.marker.Message != nil {
			if orphanable == nil {
				orphanable = map[string]bool{}
			}
			orphanable[p.marker.TargetID] = false
		}
	}
	match := false
	_, err := streamChatFileMessages(outputFile, func(i int, m *heldMarkerKey) bool {
		if held.matches(i, m.ID, m.AuthorChannelID) {
			match = true
			if orphanable == nil {
				return true
			}
		}
		if _, ok := orphanable[m.ID]; ok && m.ID != "" {
			orphanable[m.ID] = true
		}
		return false
	})
	if err != nil {
		cd.logInfo("chat: could not rewrite the chat file to apply its deletion markers; they are in the raw sidecar",
			"videoID", cd.opts.VideoID, "markers", len(pending), "err", err)
		return
	}
	if match {
		n, err := cd.rewriteChatFileWithMarkers(outputFile, held)
		if err != nil {
			cd.logInfo("chat: could not rewrite the chat file to apply its deletion markers; they are in the raw sidecar",
				"videoID", cd.opts.VideoID, "markers", len(pending), "err", err)
			return
		}
		cd.mu.Lock()
		cd.messageCount = n
		cd.mu.Unlock()
	}
	var orphans []ChatMessage
	for _, p := range pending {
		if p.marker.Kind != MarkerReplaced || p.marker.Message == nil {
			continue
		}
		if found, ok := orphanable[p.marker.TargetID]; ok && !found {
			orphans = append(orphans, *p.marker.Message)
			delete(orphanable, p.marker.TargetID) // one message per target
		}
	}
	if len(orphans) > 0 && cd.commitMessages(orphans, false) > 0 {
		cd.writeChatFile()
	}
}

// markerRewriteBatch is how many messages applyPendingMarkers' rewrite holds
// before appending them to its temp file. A var so a test can watch the
// batches.
var markerRewriteBatch = 1000

// heldMarkers indexes the held markers by the message ID and the author they
// name; the indices into list keep the order they arrived in, which decides a
// DeletedState two of them set.
type heldMarkers struct {
	list     []pendingChatMarker
	byTarget map[string][]int
	byAuthor map[string][]int
}

func indexHeldMarkers(pending []pendingChatMarker) heldMarkers {
	h := heldMarkers{list: pending, byTarget: map[string][]int{}, byAuthor: map[string][]int{}}
	for i, p := range pending {
		if p.marker.Kind == MarkerAuthorDeleted {
			h.byAuthor[p.marker.AuthorChannelID] = append(h.byAuthor[p.marker.AuthorChannelID], i)
		} else {
			h.byTarget[p.marker.TargetID] = append(h.byTarget[p.marker.TargetID], i)
		}
	}
	return h
}

// forMessage calls fn with each held marker that applies to the file's
// message idx, in arrival order: one naming its ID, or a sweep of its author
// that arrived after it was on disk (onDisk > idx).
func (h heldMarkers) forMessage(idx int, id, author string, fn func(ChatMarker)) {
	t, a := h.byTarget[id], h.byAuthor[author]
	if id == "" {
		t = nil
	}
	if author == "" {
		a = nil
	}
	for len(t) > 0 || len(a) > 0 {
		var i int
		if len(a) == 0 || (len(t) > 0 && t[0] < a[0]) {
			i, t = t[0], t[1:]
		} else {
			i, a = a[0], a[1:]
			if idx >= h.list[i].onDisk {
				continue
			}
		}
		fn(h.list[i].marker)
	}
}

func (h heldMarkers) matches(idx int, id, author string) bool {
	hit := false
	h.forMessage(idx, id, author, func(ChatMarker) { hit = true })
	return hit
}

// heldMarkerKey is all the first pass decodes of a message.
type heldMarkerKey struct {
	ID              string `json:"id"`
	AuthorChannelID string `json:"authorChannelId"`
}

// streamChatFileMessages walks path's messages array one message at a time,
// decoding each into a T and handing it to fn with its index, and stops
// early, reporting true, once fn does. Any key may come before or after the
// array; a file that is not one of ours whole is an error.
func streamChatFileMessages[T any](path string, fn func(int, *T) bool) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, chatFileReadBuffer))
	if tok, terr := dec.Token(); terr != nil || tok != json.Delim('{') {
		return false, fmt.Errorf("parse chat file: top level is not a JSON object")
	}
	idx := 0
	for {
		keyTok, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("parse chat file: %w", err)
		}
		if keyTok == json.Delim('}') {
			break
		}
		if key, ok := keyTok.(string); !ok || key != "messages" {
			if !ok {
				return false, fmt.Errorf("parse chat file: unexpected token %v", keyTok)
			}
			if err := utils.SkipJSONValue(dec); err != nil {
				return false, fmt.Errorf("parse chat file: %w", err)
			}
			continue
		}
		opening, err := dec.Token()
		if err != nil {
			return false, fmt.Errorf("parse chat messages: %w", err)
		}
		if opening == nil {
			continue
		}
		if opening != json.Delim('[') {
			return false, fmt.Errorf("parse chat messages: not an array")
		}
		for dec.More() {
			var m T
			if err := dec.Decode(&m); err != nil {
				return false, fmt.Errorf("parse chat messages: %w", err)
			}
			if fn(idx, &m) {
				return true, nil
			}
			idx++
		}
		if _, err := dec.Token(); err != nil {
			return false, fmt.Errorf("parse chat messages: %w", err)
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("parse chat file: trailing data after the top-level object")
	}
	return false, nil
}

// rewriteChatFileWithMarkers is applyPendingMarkers' second pass: the file
// rewritten through a temp beside it — the header as writeFullChatFile writes
// it, then the messages, flagged, appended a batch at a time the way every
// flush appends them — and swapped in. Returns how many messages it holds.
// On any failure the temp is removed and the file is as it was.
func (cd *ChatDownloader) rewriteChatFileWithMarkers(path string, held heldMarkers) (int, error) {
	tf, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".markers-*")
	if err != nil {
		return 0, err
	}
	tmp := tf.Name()
	tf.Close()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmp)
		}
	}()
	header := cd.chatFileData([]ChatMessage{})
	header.MessageCount = 0
	if err := utils.WriteChatFileAtomic(tmp, &header); err != nil {
		return 0, err
	}
	batch := make([]ChatMessage, 0, markerRewriteBatch)
	n := 0
	var appendErr error
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		appendErr = appendChatMessages(tmp, batch, n, chatWarnAdapter{cd})
		batch = batch[:0]
		return appendErr == nil
	}
	if _, err := streamChatFileMessages(path, func(i int, m *ChatMessage) bool {
		held.forMessage(i, m.ID, m.AuthorChannelID, func(mk ChatMarker) {
			one := []ChatMessage{*m}
			applyMarker(one, mk, 1)
			*m = one[0]
		})
		batch = append(batch, *m)
		n++
		return len(batch) >= markerRewriteBatch && !flush()
	}); err != nil {
		return 0, err
	}
	if appendErr != nil || !flush() {
		return 0, appendErr
	}
	if err := utils.ReplaceFile(tmp, path); err != nil {
		return 0, err
	}
	committed = true
	return n, nil
}

// rawSidecarPath is the raw sidecar beside a chat file: "chat.json" →
// "chat.raw.jsonl". A replay re-run's temporary file has one of its own,
// "chat.raw.jsonl.rerun", settled with it (finishReplayRerun): the re-run
// fetches the replay from the top, so sharing the archive's sidecar appended
// every action a second time, and again on each later re-run, even one that
// was thrown away.
func rawSidecarPath(chatFile string) string {
	if base, ok := strings.CutSuffix(chatFile, replayRerunSuffix); ok {
		return rawSidecarPath(base) + replayRerunSuffix
	}
	return strings.TrimSuffix(chatFile, ".json") + ".raw.jsonl"
}

// startRawSidecarFresh removes the sidecar a non-resuming replay run would
// otherwise append to. Such a run fetches the replay from the top and writes
// its chat file whole (or a re-run's beside the archive), so the sidecar
// starts over with it, as yt-dlp's live_chat.json does on every run; an
// earlier run's lines kept would be served again and appended twice. A
// resuming run, and every live run, appends: its file keeps the history.
func (cd *ChatDownloader) startRawSidecarFresh() {
	outputFile, _ := cd.getOutputPaths()
	if outputFile == "" {
		return
	}
	path := rawSidecarPath(outputFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		cd.logDebug("chat: could not remove the raw sidecar a fresh replay run starts over", "file", path, "err", err)
	}
}

// settleRerunRawSidecar is the sidecar half of finishReplayRerun: the
// re-run's replaces the archive's when the re-run became the archive (none
// written means the replay served none, so the archive's goes too), and is
// removed when it was discarded.
func (cd *ChatDownloader) settleRerunRawSidecar(rerunChatFile, archiveChatFile string, swapped bool) {
	tmp, dst := rawSidecarPath(rerunChatFile), rawSidecarPath(archiveChatFile)
	if swapped {
		err := utils.ReplaceFile(tmp, dst)
		if os.IsNotExist(err) {
			err = os.Remove(dst)
		}
		if err == nil || os.IsNotExist(err) {
			return
		}
		cd.logDebug("chat: could not settle the re-run's raw sidecar", "file", dst, "err", err)
	}
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		cd.logDebug("chat: could not remove the discarded re-run's raw sidecar", "file", tmp, "err", err)
	}
}

// queueRawActions buffers a batch's raw actions for the sidecar. A live
// action is wrapped as yt-dlp's live path writes every action
// (youtube_live_chat.py parse_actions_live, b77c9304e): {"replayChatItemAction":
// {"actions": [action]}, "videoOffsetTimeMsec": "<offset>", "isLive": true},
// the offset that of the newest message before it, in this batch or an
// earlier one. Without it a live deletion, banner or poll — none of which
// carries a time of its own — could not be placed on the archive's timeline.
// Moombox's offset counts from the chat file's epoch, as its messages' do,
// where upstream's counts from when its download began; before any message
// with a timestamp, it is the wall clock's distance from the epoch, and with
// no epoch at all the line carries no offset.
func (cd *ChatDownloader) queueRawActions(resp *ChatApiResponse) {
	raw := resp.RawActions
	if len(raw) == 0 {
		cd.noteLiveRawOffset(resp.Messages)
		return
	}
	scanned := 0
	for i, action := range raw {
		after := -1
		if i < len(resp.RawLiveAfter) {
			after = min(resp.RawLiveAfter[i], len(resp.Messages))
		}
		if after < 0 {
			cd.rawPending = append(cd.rawPending, action)
			continue
		}
		if after > scanned {
			cd.noteLiveRawOffset(resp.Messages[scanned:after])
			scanned = after
		}
		wrapper := map[string]any{
			"replayChatItemAction": map[string]any{"actions": []json.RawMessage{action}},
			"isLive":               true,
		}
		if off, ok := cd.liveRawOffset(); ok {
			wrapper["videoOffsetTimeMsec"] = strconv.FormatInt(off, 10)
		}
		if b, err := json.Marshal(wrapper); err == nil {
			cd.rawPending = append(cd.rawPending, b)
		}
	}
	if scanned < len(resp.Messages) {
		cd.noteLiveRawOffset(resp.Messages[scanned:])
	}
	if over := len(cd.rawPending) - maxPendingRawActions; over > 0 {
		cd.rawPending = cd.rawPending[over:]
	}
}

// noteLiveRawOffset advances the live raw lines' offset to the newest of msgs
// with a timestamp, against the chat file's epoch.
func (cd *ChatDownloader) noteLiveRawOffset(msgs []ChatMessage) {
	if cd.streamStartMs <= 0 {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if usec, err := strconv.ParseInt(msgs[i].TimestampUsec, 10, 64); err == nil && usec > 0 {
			cd.liveRawOffsetMs, cd.hasLiveRawOffset = usec/1000-cd.streamStartMs, true
			return
		}
	}
}

// liveRawOffset is the offset a live raw line is stamped with.
func (cd *ChatDownloader) liveRawOffset() (int64, bool) {
	if cd.hasLiveRawOffset {
		return cd.liveRawOffsetMs, true
	}
	if cd.streamStartMs > 0 {
		return time.Now().UnixMilli() - cd.streamStartMs, true
	}
	return 0, false
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
