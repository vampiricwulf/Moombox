/**
 * The INPUTS behind web/tests/fixtures/app-job-items.json.
 *
 * app.test.mjs feeds these to the real renderers and compares the result to
 * the snapshot; the generator that first wrote the snapshot read the same
 * objects. Keeping them here (rather than inline in the test) is what makes
 * "regenerate the snapshot" a one-liner and keeps the two halves in step —
 * the recipe is in web/tests/README.md, "The app harness".
 *
 * Everything is fixed: ids, titles, sizes, and every timestamp is expressed as
 * an offset from the harness's frozen clock, so `formatRelativeTime` and
 * `toLocaleString` render the same strings on every machine.
 *
 * The titles/channels carry `<`, `&` and `"` on purpose — escapeHtml is part
 * of what the snapshot pins.
 */
import { agoISO } from "../helpers/app-dom.mjs";

const BASE = {
  id: "job-<1>",
  videoId: "vid&1",
  title: 'A "quoted" <title> & more',
  channelName: "Chan & Co <b>",
  platform: "youtube",
  thumbnailUrl: "https://example.test/thumb.jpg?a=1&b=2",
};

/** One job per JobStatus the dashboard can render. */
export const JOBS = {
  // Relative "Last check" text + the data-timestamp attribute path.
  Upcoming: { ...BASE, status: "Upcoming", lastRecheckAt: agoISO(300) },
  // Twitch: the TW badge, the channel-avatar thumbnail and its 1:1 box.
  Live: {
    ...BASE, status: "Live", platform: "twitch", thumbnailUrl: null,
    channelAvatarUrl: "https://example.test/avatar.png", progress: "Seq: 42 C: 7",
  },
  // The progress string the brief names, plus a progress bar.
  Downloading: { ...BASE, status: "Downloading", progress: "V:1234 A:1234 C:5678", percent: 37 },
  Muxing: { ...BASE, status: "Muxing", progress: "Muxing 50%", percent: 100 },
  // Local thumbnail route + both Finished-only overlays (watched, missing tail).
  Finished: {
    ...BASE, status: "Finished", filename: "out.mp4", thumbnailFile: "thumb.jpg",
    watched: true, incompleteTail: true,
  },
  // Long enough to hit formatProgressHtml's 50-char truncation.
  Error: {
    ...BASE, status: "Error",
    error: 'download failed after 3 attempts: unexpected <status> 403 from the CDN & no fallback',
  },
  Cancelled: { ...BASE, status: "Cancelled" },
  // Membership park: the remedy text, not the generic "needs cookie refresh".
  "COOKIES?": {
    ...BASE, status: "COOKIES?", parkReason: "membership",
    error: "switch the browser to the account that holds the membership",
  },
  Queued: { ...BASE, status: "Queued" },
};

/** Two orphaned files — one staging, one output, to pin the type sort too. */
export const FILES = [
  {
    type: "output", path: "D:\\out\\Stream & <one>.mp4", relPath: "Stream & <one>.mp4",
    size: 1234567, modified: agoISO(3600), jobId: "job-<1>", jobTitle: 'A "quoted" <title>',
    jobStatus: "Finished",
  },
  {
    type: "staging", path: "D:\\staging\\job-2", relPath: "job-2", size: 42,
    modified: agoISO(90000), jobId: "", jobTitle: "", jobStatus: "",
  },
];

/** Two orphaned history rows. */
export const HISTORY = [
  { videoId: "abc<123>", addedAt: agoISO(7200) },
  { videoId: "def&456", addedAt: agoISO(172800) },
];
