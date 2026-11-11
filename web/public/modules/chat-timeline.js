/**
 * Chat-file load-time normalization: timeline math, plus the legacy Twitch
 * emote-offset repair. Pure — no DOM, no fetch — so it is covered by
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
 * T-F12 remainder: a message with no offset of its own — `offsetMs` 0 and no
 * `hasOffset` (the producer's pre-2026-04-22 sentinel; `hasOffset` arrived in
 * 068465ed and internal/chat/types.go still records that "offsetMs=0 was the
 * unset sentinel") or an explicit `hasOffset: false` — piles at the start of
 * the timeline. When the file header has an epoch and the message a
 * `timestampUsec`, the offset is derivable with the arithmetic the Go
 * producer itself uses (`offsetMs = timestampUsec/1000 − epochMs`, integer
 * division, internal/chat/downloader.go), so the player recovers it at load.
 *
 * A message that carries a NON-ZERO `offsetMs` is authoritative even without
 * `hasOffset` and is left alone: a pre-2026-04-22 file has real offsets on
 * every message and no `hasOffset` anywhere, and on a replay/VOD file those
 * are YouTube's own video-relative `videoOffsetTimeMsec` — re-deriving them
 * from wall-clock would shift the whole archive by the ingest latency. That
 * is why the skip tests the sentinel, not just the flag (F1).
 *
 * Without a parseable epoch nothing changes (R10 keeps the pile from flooding
 * the overlay). Derived offsets are signed: waiting-room chat predates the
 * epoch and stays negative (N-F2).
 *
 * `streamStartTime` must be the FILE's own header epoch — one file, one epoch
 * — so on a multi-part job this runs per part, before mergePartChats shifts
 * the parts onto the global timeline. Twitch files are skipped at the call
 * sites (their offsets are already video-relative, and their epoch is the
 * recording start, not a chat clock); Twitch messages also carry no
 * `timestampUsec`, so they would fall through untouched anyway.
 *
 * Returns the same array, mutated in place.
 * @param {Array<{offsetMs?:number, hasOffset?:boolean, timestampUsec?:string|number}>} messages
 * @param {string|undefined} streamStartTime the chat file header's epoch
 */
export function deriveMissingOffsets(messages, streamStartTime) {
  const epochMs = Date.parse(streamStartTime || "");
  if (!Number.isFinite(epochMs)) return messages;
  for (const m of messages || []) {
    if (m.hasOffset || normalizeOffsetMs(m.offsetMs) !== 0) continue;
    const usec = typeof m.timestampUsec === "number" ? m.timestampUsec : Number(m.timestampUsec);
    if (!Number.isFinite(usec) || usec <= 0) continue;
    // Math.trunc, not Math.round: the Go producer divides int64 by 1000.
    m.offsetMs = Math.trunc(usec / 1000) - epochMs;
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
/** "1 message", "2 messages". */
const messageCount = (n) => `${n} message${n === 1 ? "" : "s"}`;

export function formatChatHeader(total, preCount, postCount) {
  let text = messageCount(total);
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
    return `Waiting room — ${messageCount(parts.preCount)} before the stream`;
  }
  if (parts.firstPostIndex >= 0 && index === parts.firstPostIndex) {
    return `Recording ended — ${messageCount(parts.postCount)} after it`;
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

/** The `messageType` of a stored Twitch CLEARCHAT or CLEARMSG (Go: twitch.MessageTypeModeration). */
export const TWITCH_MODERATION_TYPE = "moderation";

/**
 * What a marked message's "deleted" tooltip says when its row carries no sentence.
 *
 * The keys are the row's `moderation` words, Go's twitch.ModerationKind
 * (internal/twitch/types.go). Those words and the row's `targetMsgId`,
 * `targetUserId` and `targetLogin` keys, read below, are held to the
 * recorder's by web/tests/fixtures/twitch-moderation-parity.json.
 */
const TWITCH_MODERATION_FALLBACK = {
  deleted: "Deleted by a moderator.",
  author_deleted: "The user was timed out or banned.",
};

/**
 * The one line the sidebar shows where a moderator cleared the whole chat
 * (owner ruling "show a marker only"). The recorder writes the same sentence
 * in the row's `systemMsg` (internal/twitch/chat_irc.go parseClearchat); the
 * player prints this one whatever the row carries.
 */
export const TWITCH_CHAT_CLEARED_TEXT = "Chat was cleared by a moderator";

/**
 * Apply a Twitch chat file's moderation rows (owner ruling R-G1): mark the
 * messages a timeout, a ban or a deletion removed and take those rows out of
 * the list; turn a full clear into a marker row and mark nothing for it.
 *
 * The live IRC recorder stores each CLEARCHAT and CLEARMSG as a row of its
 * own, `messageType: "moderation"`, in arrival order, and never rewrites the
 * messages the event removed (internal/twitch/types.go). This is the one pass
 * that turns the rows into the marks the player already draws for YouTube
 * (`deleted`, `deletedState`: _markChatChange, the overlay's skip, the
 * dropped-count rule):
 *
 * - `moderation: "deleted"` marks the one message whose `id` is `targetMsgId`;
 * - `"author_deleted"` (a timeout or a ban) marks every EARLIER message whose
 *   `authorId` is `targetUserId` — the user may post again once a timeout ends.
 *   A row with no `targetUserId` (a CLEARCHAT without the target-user-id tag,
 *   which Twitch sends today) is matched by `targetLogin` against the
 *   author's display name, case-insensitively: a display name differs from the
 *   login in case only, unless it is a localized one, which such a row cannot
 *   reach;
 * - `"chat_cleared"` (a moderator's /clear) marks NOTHING (owner ruling
 *   "show a marker only"): every earlier message stays as it was, still
 *   flying on the overlay. The row stays in the list, at its own place and
 *   offset, as a copy retyped `messageType: "system"` with
 *   TWITCH_CHAT_CLEARED_TEXT as its `systemMsg`, an empty `message` and
 *   `sidebarOnly: true`. The sidebar draws it as it draws any `system`
 *   notice (isDimTwitchNotice, player.js) and counts it in its header as it
 *   counts those; `sidebarOnly` keeps it off the video (_buildNicoEl) and
 *   out of the "+N not shown" count (NicoScheduler.countDrop).
 *
 * "Earlier" is the array's order, which is the order the rows were written
 * in, so call this BEFORE the sort by offset; for a multi-part job, on the
 * merged list, so a ban in part 2 reaches the user's messages in part 1. A
 * marked message's `deletedState` is the sentence of the first event that
 * removed it. A row of a kind this build does not know marks nothing and is
 * hidden.
 *
 * A list with no moderation row — every YouTube file, every VOD chat, every
 * file written before the rows existed — is returned as it is, the same
 * array, untouched. Otherwise the kept messages are marked in place and a new
 * array is returned, without the rows except a full clear's marker.
 * @param {Array<object>} messages in file order
 * @returns {Array<object>}
 */
export function applyTwitchModeration(messages) {
  if (!Array.isArray(messages)) return messages;
  if (!messages.some((m) => m && m.messageType === TWITCH_MODERATION_TYPE)) return messages;
  // Walked BACKWARDS, so each lookup already holds the earliest event after
  // the message in hand: a later event of the same kind is overwritten by an
  // earlier one as the walk reaches it.
  const byMsgId = new Map();
  const byUserId = new Map();
  const byLogin = new Map();
  const kept = [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (!m) continue;
    if (m.messageType === TWITCH_MODERATION_TYPE) {
      const event = { index: i, row: m };
      if (m.moderation === "deleted" && m.targetMsgId) byMsgId.set(m.targetMsgId, event);
      else if (m.moderation === "author_deleted" && m.targetUserId) byUserId.set(m.targetUserId, event);
      else if (m.moderation === "author_deleted" && typeof m.targetLogin === "string" && m.targetLogin) {
        byLogin.set(m.targetLogin.toLowerCase(), event);
      }
      else if (m.moderation === "chat_cleared") {
        kept.push({ ...m, messageType: "system", systemMsg: TWITCH_CHAT_CLEARED_TEXT, message: "", sidebarOnly: true });
      }
      continue;
    }
    kept.push(m);
    let first;
    const candidates = [m.id ? byMsgId.get(m.id) : undefined, m.authorId ? byUserId.get(m.authorId) : undefined];
    if (byLogin.size > 0 && typeof m.authorName === "string") candidates.push(byLogin.get(m.authorName.toLowerCase()));
    for (const e of candidates) {
      if (e && (!first || e.index < first.index)) first = e;
    }
    if (!first || m.deleted === true) continue;
    m.deleted = true;
    const sentence = typeof first.row.systemMsg === "string" ? first.row.systemMsg.trim() : "";
    m.deletedState = sentence || TWITCH_MODERATION_FALLBACK[first.row.moderation];
  }
  return kept.reverse();
}

/** The CTCP wrapper Twitch sends a /me message in: \x01ACTION <text>\x01. */
const TWITCH_ACTION_PREFIX = "\u0001ACTION ";
const TWITCH_SOH = "\u0001";

/**
 * Repair a Twitch chat file written before 2026-09-15, in place.
 *
 * Until then the live IRC producer read Twitch's emote offsets — which count
 * Unicode CODE POINTS — as if they were UTF-16 code units, and left /me
 * messages wrapped in \x01ACTION …\x01. Files written since carry the header
 * scalar `emoteOffsets: "utf16"` (Go: TwitchChatData.EmoteOffsets), so its
 * ABSENCE is the era marker and this function is the era's reader.
 *
 * Three gates, each load-bearing:
 * - `platform === "twitch"`: a YouTube chat file has no such offsets.
 * - `emoteOffsets !== "utf16"`: a marked file is already right, and correcting
 *   it a second time would shift every span the other way.
 * - per message, `raw`: only the IRC path records the verbatim wire line, so it
 *   is how a legacy file's IRC messages are told from its VOD comments — the
 *   VOD path (api.go utf16Len) emitted UTF-16 from the start and must not move.
 *
 * Run ONCE per file at load, before any rendering and before mergePartChats
 * (which keeps no header scalars, so a multi-part job has to correct each part
 * against its own header).
 *
 * Returns the same object, mutated in place.
 * @param {{platform?:string, emoteOffsets?:string, messages?:Array}} data
 */
export function correctLegacyTwitchEmotes(data) {
  if (!data || data.platform !== "twitch" || data.emoteOffsets === "utf16") return data;
  for (const m of data.messages || []) {
    if (!m || !m.raw) continue;
    let text = typeof m.message === "string" ? m.message : "";
    if (text.startsWith(TWITCH_ACTION_PREFIX)) {
      text = text.slice(TWITCH_ACTION_PREFIX.length);
      if (text.endsWith(TWITCH_SOH)) text = text.slice(0, -1);
      m.message = text;
      // Parity with the Go writer's TwitchChatMessage.IsAction, which is the
      // file format's truth; no renderer in web/ reads it yet.
      m.isAction = true;
    }
    const emotes = m.emotes;
    if (!Array.isArray(emotes) || emotes.length === 0) continue;
    // cpToUnit[i] is the UTF-16 index at which code point i begins; the
    // sentinel at the end holds the total length, so `end` maps without a
    // special case for a span that reaches the last character.
    const cps = [...text];
    const cpToUnit = new Array(cps.length + 1);
    let units = 0;
    for (let i = 0; i < cps.length; i++) {
      cpToUnit[i] = units;
      units += cps[i].length; // 1, or 2 for a surrogate pair
    }
    cpToUnit[cps.length] = units;
    for (const e of emotes) {
      const s = Number(e.start);
      const en = Number(e.end);
      // Same bounds rule as the Go producer: a malformed wire range keeps the
      // offsets it was sent with and renders as plain text.
      if (!Number.isInteger(s) || !Number.isInteger(en)) continue;
      if (s < 0 || s > en || en >= cps.length) continue;
      e.name = cps.slice(s, en + 1).join("");
      e.start = cpToUnit[s];
      e.end = cpToUnit[en + 1] - 1;
    }
  }
  return data;
}
