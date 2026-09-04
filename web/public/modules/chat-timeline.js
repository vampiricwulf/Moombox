/**
 * Chat ↔ video timeline math. Pure — no DOM, no fetch — so it is covered by
 * web/tests/chat-timeline.test.mjs.
 *
 * Offset semantics (verified against the Go producers, review 2026-09-03):
 * - YouTube chat.json: offsetMs counts from chat.streamStartTime (the start
 *   the downloader was created with — the SCHEDULED time for early chat).
 *   The video begins at the ACTUAL start (job.streamStartTime; DASH backfills
 *   from sequence 0), so bias = actual − scheduled. Negative offsets are
 *   waiting-room chat.
 * - Twitch chat.json (platform:"twitch"): live IRC offsets count from the
 *   part's recording start and VOD offsets from the VOD start — both already
 *   video-relative. Bias is 0. Multi-part files are shifted per part.
 */

export function normalizeOffsetMs(raw) {
  const n = typeof raw === "number" ? raw : Number(raw);
  return Number.isFinite(n) ? n : 0;
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
