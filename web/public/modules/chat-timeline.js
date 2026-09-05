/**
 * Chat ↔ video timeline math. Pure — no DOM, no fetch — so it is covered by
 * web/tests/chat-timeline.test.mjs.
 *
 * Offset semantics (verified against the Go producers, review 2026-09-03):
 * - YouTube chat.json: offsetMs counts from chat.streamStartTime, which is
 *   the FILE'S FIRST run's epoch — pinned across resumes/adoption (Go
 *   ChatResumeState.streamStartMs; the header value is rendered by
 *   epochRFC3339()), not whatever start time the run that is CURRENTLY
 *   appending to it was created with. For a live/early-chat file that epoch
 *   is typically the SCHEDULED start; the video begins at the ACTUAL start
 *   (job.streamStartTime; DASH backfills from sequence 0), so bias = actual
 *   − scheduled, and negative offsets are waiting-room chat. A replay/VOD
 *   chat file's offsets are already VOD-relative (YouTube's own
 *   videoOffsetTimeMsec, with pre-stream messages recovered from
 *   timestampText), and the worker refreshes job.streamStartTime to that
 *   same actual start once the job is classified VOD/post-live
 *   (vodStatusUpdates, internal/worker/stream_processor.go) — so bias is 0
 *   there too.
 * - Twitch chat.json (platform:"twitch"): live IRC offsets count from the
 *   part's recording start and VOD offsets from the VOD start — both already
 *   video-relative. Bias is 0. Multi-part files are shifted per part.
 */

export function normalizeOffsetMs(raw) {
  const n = typeof raw === "number" ? raw : Number(raw);
  return Number.isFinite(n) ? n : 0;
}

/**
 * T-F12 remainder: messages written before `hasOffset` existed (or with
 * `hasOffset` false) carry `offsetMs` 0 and pile at the start of the timeline.
 * When the file header has an epoch and the message a `timestampUsec`, the
 * offset is derivable with the arithmetic the Go producer itself uses
 * (`offsetMs = timestampUsec/1000 − epochMs`, internal/chat/downloader.go), so
 * the player recovers it at load. Messages that already have an offset are
 * untouched — theirs is authoritative — and without a parseable epoch nothing
 * changes (R10 keeps the pile from flooding the overlay). Derived offsets are
 * signed: waiting-room chat predates the epoch and stays negative (N-F2).
 *
 * `streamStartTime` must be the FILE's own header epoch — one file, one epoch
 * — so on a multi-part job this runs per part, before mergePartChats shifts
 * the parts onto the global timeline. Twitch parts carry no `timestampUsec`
 * and fall through untouched; their offsets are already video-relative.
 *
 * Returns the same array, mutated in place.
 * @param {Array<{offsetMs?:number, hasOffset?:boolean, timestampUsec?:string|number}>} messages
 * @param {string|undefined} streamStartTime the chat file header's epoch
 */
export function deriveMissingOffsets(messages, streamStartTime) {
  const epochMs = Date.parse(streamStartTime || "");
  if (!Number.isFinite(epochMs)) return messages;
  for (const m of messages || []) {
    if (m.hasOffset) continue;
    const usec = typeof m.timestampUsec === "number" ? m.timestampUsec : Number(m.timestampUsec);
    if (!Number.isFinite(usec) || usec <= 0) continue;
    m.offsetMs = Math.round(usec / 1000) - epochMs;
    m.hasOffset = true;
  }
  return messages;
}

export function computeChatBiasMs({ platform, chatStreamStartTime, jobStreamStartTime }) {
  if (platform === "twitch") return 0;
  const chatStart = Date.parse(chatStreamStartTime || "");
  const jobStart = Date.parse(jobStreamStartTime || "");
  if (!Number.isFinite(chatStart) || !Number.isFinite(jobStart)) return 0;
  return jobStart - chatStart;
}

/** First index whose offsetMs is strictly greater than `offsetMs` (messages sorted ascending). */
export function indexAfter(messages, offsetMs) {
  let lo = 0;
  let hi = messages.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (messages[mid].offsetMs <= offsetMs) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/**
 * Split a sorted list into pre-show (offset < 0), in-video and post-end
 * (offset > totalDurationMs) regions. totalDurationMs <= 0 means unknown.
 */
export function partitionChatByVideo(messages, totalDurationMs) {
  // Direct scan rather than indexAfter(messages, -1): exact against
  // fractional negative offsets, and O(pre) where pre is usually small.
  let preCount = 0;
  while (preCount < messages.length && messages[preCount].offsetMs < 0) preCount++;
  const firstLiveIndex = preCount < messages.length ? preCount : -1;
  let firstPostIndex = -1;
  if (totalDurationMs > 0) {
    const idx = indexAfter(messages, totalDurationMs);
    if (idx < messages.length) firstPostIndex = idx;
  }
  const postCount = firstPostIndex === -1 ? 0 : messages.length - firstPostIndex;
  return { preCount, firstLiveIndex, postCount, firstPostIndex };
}

/**
 * The chat sidebar's header text. Counts describe the messages actually
 * loaded — never a file header's own messageCount, which can disagree with
 * what was parsed — and an empty region contributes no clause at all.
 * @param {number} total
 * @param {number} preCount
 * @param {number} postCount
 * @returns {string}
 */
export function formatChatHeader(total, preCount, postCount) {
  let text = `${total} messages`;
  if (preCount > 0) text += ` · ${preCount} pre-show`;
  if (postCount > 0) text += ` · ${postCount} after end`;
  return text;
}

/**
 * Divider text for the row at `index`, or null when no region starts there.
 * Both regions can begin on the SAME row (a chat whose entire live section
 * falls past the recording); the waiting-room label wins there — it explains
 * that row's own position.
 * @param {ReturnType<typeof partitionChatByVideo>|null} parts
 * @param {number} index
 * @returns {string|null}
 */
export function dividerLabelFor(parts, index) {
  if (!parts) return null;
  if (parts.preCount > 0 && index === parts.firstLiveIndex) {
    return `Waiting room — ${parts.preCount} messages before the stream`;
  }
  if (parts.firstPostIndex >= 0 && index === parts.firstPostIndex) {
    return `Recording ended — ${parts.postCount} messages after it`;
  }
  return null;
}

/**
 * Merge per-part chat files onto the global timeline: each part's offsets are
 * part-relative, so add the part's start offset. Header fields come from the
 * first part that has them.
 * @param {Array<{startOffsetSec:number, data:object}>} parts in playback order
 */
export function mergePartChats(parts) {
  const merged = { platform: undefined, streamStartTime: undefined, emotes: undefined, messages: [] };
  for (const { startOffsetSec, data } of parts) {
    if (!data) continue;
    merged.platform ??= data.platform;
    merged.streamStartTime ??= data.streamStartTime;
    merged.emotes ??= data.emotes;
    const shiftMs = Math.round((startOffsetSec || 0) * 1000);
    for (const m of data.messages || []) {
      merged.messages.push({ ...m, offsetMs: normalizeOffsetMs(m.offsetMs) + shiftMs });
    }
  }
  return merged;
}
