// Tests for web/public/modules/utils.js

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  formatTimestamp,
  formatBytes,
  formatDurationSeconds,
  formatMsToTime,
  safePlay,
  applyChannelOverrides,
  canResumeJob,
  channelTermsForSave,
  resolveConfigPath,
  snapshotRestartValues,
  restartValuesChanged,
  streamUrl,
} from "../public/modules/utils.js";

test("formatTimestamp: zero and invalid inputs", () => {
  assert.equal(formatTimestamp(0), "0:00");
  assert.equal(formatTimestamp(null), "0:00");
  assert.equal(formatTimestamp(undefined), "0:00");
  assert.equal(formatTimestamp(NaN), "0:00");
  assert.equal(formatTimestamp(Infinity), "0:00");
  assert.equal(formatTimestamp(-5), "0:00");
});

test("formatTimestamp: minutes:seconds", () => {
  assert.equal(formatTimestamp(30), "0:30");
  assert.equal(formatTimestamp(59), "0:59");
  assert.equal(formatTimestamp(60), "1:00");
  assert.equal(formatTimestamp(125), "2:05");
});

test("formatTimestamp: hours:minutes:seconds", () => {
  assert.equal(formatTimestamp(3600), "1:00:00");
  assert.equal(formatTimestamp(3665), "1:01:05");
  assert.equal(formatTimestamp(36000), "10:00:00");
});

test("formatBytes: each unit boundary", () => {
  assert.equal(formatBytes(0), "0B");
  assert.equal(formatBytes(512), "512B");
  assert.equal(formatBytes(1024), "1.0KB");
  assert.equal(formatBytes(1536), "1.5KB");
  assert.equal(formatBytes(1024 * 1024), "1.0MB");
  assert.equal(formatBytes(1024 * 1024 * 1024), "1.0GB");
  assert.equal(formatBytes(1024 * 1024 * 1024 * 1024), "1.0TB");
});

test("formatBytes: invalid inputs coerce to 0B", () => {
  assert.equal(formatBytes(null), "0B");
  assert.equal(formatBytes(NaN), "0B");
  assert.equal(formatBytes(-100), "0B");
});

test("formatDurationSeconds", () => {
  assert.equal(formatDurationSeconds(0), "0s");
  assert.equal(formatDurationSeconds(45), "45s");
  assert.equal(formatDurationSeconds(90), "1m 30s");
  assert.equal(formatDurationSeconds(3661), "1h 1m 1s");
  assert.equal(formatDurationSeconds(null), "0s");
});

test("formatMsToTime: positive values", () => {
  assert.equal(formatMsToTime(0), "0:00");
  assert.equal(formatMsToTime(30_000), "0:30");
  assert.equal(formatMsToTime(90_500), "1:30");
  assert.equal(formatMsToTime(3_600_000), "1:00:00");
});

test("formatMsToTime: negative (pre-stream waiting-room chat)", () => {
  assert.equal(formatMsToTime(-30_000), "-0:30");
  assert.equal(formatMsToTime(-125_000), "-2:05");
  assert.equal(formatMsToTime(-3_600_000), "-1:00:00");
});

test("formatMsToTime: invalid inputs return 0:00", () => {
  assert.equal(formatMsToTime(null), "0:00");
  assert.equal(formatMsToTime(NaN), "0:00");
  assert.equal(formatMsToTime(Infinity), "0:00");
});

test("safePlay swallows a rejected play() promise and tolerates a void return", async () => {
  let rejections = 0;
  const onUnhandled = () => { rejections++; };
  process.on("unhandledRejection", onUnhandled);
  try {
    safePlay({ play: () => Promise.reject(new Error("AbortError")) });
    safePlay({ play: () => undefined });
    // phase-2-review.md §4 mutant MU6: the `media && media.play` guard is the
    // documented tolerance, so pin it — a bare `media.play()` throws on both.
    safePlay(null);
    safePlay({});
    await new Promise((r) => setImmediate(r));
    assert.equal(rejections, 0);
  } finally {
    process.off("unhandledRejection", onUnhandled);
  }
});

test("applyChannelOverrides: values set, blanks clear, existing preserved", () => {
  const existing = { id: "UC1", name: "N", num_desc_lookbehind: 5, output_directory: "D:/old", archive_window_days: 7, archive_slots: 2 };
  const r = applyChannelOverrides({ ...existing }, {
    numDescLookbehind: undefined, outputDirectory: "", archiveWindowDays: 14, archiveSlots: 4,
  });
  assert.equal(r.error, null);
  assert.equal("num_desc_lookbehind" in r.channel, false, "blank clears the key");
  assert.equal("output_directory" in r.channel, false, "blank clears the key");
  assert.equal(r.channel.archive_window_days, 14);
  assert.equal(r.channel.archive_slots, 4);
  assert.equal(r.channel.name, "N", "unrelated keys untouched");
});

test("applyChannelOverrides: rejects out-of-range and non-integer values", () => {
  const cases = [
    [{ numDescLookbehind: -1 }, /lookbehind/i],
    [{ numDescLookbehind: 1.5 }, /lookbehind/i],
    [{ archiveWindowDays: 0 }, /window/i],
    [{ archiveWindowDays: 3651 }, /window/i],
    [{ archiveSlots: 0 }, /slots/i],
    [{ archiveSlots: 101 }, /slots/i],
  ];
  for (const [ov, re] of cases) {
    const r = applyChannelOverrides({ id: "UC1" }, ov);
    assert.match(r.error ?? "", re, JSON.stringify(ov));
  }
  const ok = applyChannelOverrides({ id: "UC1" }, { numDescLookbehind: 0, archiveWindowDays: 3650, archiveSlots: 100, outputDirectory: " D:/x " });
  assert.equal(ok.error, null);
  assert.equal(ok.channel.output_directory, "D:/x", "trimmed");
});

test("canResumeJob: the single-job gate, applied everywhere", () => {
  const yt = (status, extra = {}) => ({ status, platform: "youtube", hasStaging: true, ...extra });
  assert.equal(canResumeJob(yt("Error")), true);
  assert.equal(canResumeJob(yt("Cancelled")), true);
  assert.equal(canResumeJob(yt("COOKIES?")), true);
  assert.equal(canResumeJob(yt("Finished", { incompleteTail: true })), true);
  assert.equal(canResumeJob(yt("Finished")), false, "a complete Finished job is not resumable");
  assert.equal(canResumeJob(yt("Downloading")), false);
  assert.equal(canResumeJob({ ...yt("Error"), platform: "twitch" }), false, "resume is YouTube-only");
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: false }), false, "no staging, nothing to resume");
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: undefined }), true, "unknown staging (list row) defers to the server");
});

test("canResumeJob: requireKnownStaging hides the button until the details fetch lands", () => {
  const yt = (status, extra = {}) => ({ status, platform: "youtube", hasStaging: true, ...extra });
  const strict = { requireKnownStaging: true };
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: undefined }, strict), false, "the details view renders before _fetchStagingFields resolves");
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: true }, strict), true);
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: false }, strict), false);
  assert.equal(canResumeJob(yt("Downloading"), strict), false, "the status gate still applies");
  // Default mode is unchanged for the batch sites, which never carry staging.
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: undefined }, {}), true);
  assert.equal(canResumeJob({ ...yt("Error"), hasStaging: undefined }), true);
});

// The sixteen restart-required paths, copied literally from
// RESTART_REQUIRED_FIELDS in web/public/modules/settings.js. They cannot be
// imported (the goja harness in internal/tui/settings_js_vm_test.go strips
// `export` from that module), so they are duplicated here; the Go test
// TestRestartRequiredListsAgree pins the Web list against the TUI's.
const RESTART_FIELDS = [
  { path: "network.port" },
  { path: "network.network_access" },
  { path: "network.https_enabled" },
  { path: "network.tls_cert_path" },
  { path: "network.tls_key_path" },
  { path: "paths.database_path" },
  { path: "paths.log_file_path" },
  { path: "logs.log_max_file_size" },
  { path: "logs.log_max_files" },
  { path: "cookies.cookie_file" },
  { path: "cookies.refresh_interval" },
  { path: "cookies.auto_enabled" },
  { path: "cookies.browser_profile_dir" },
  { path: "connectivity.probe_targets" },
  { path: "memory.sidecar_hard_limit_mb" },
  { path: "bgutils.use_sidecar" },
];

// A fresh default-shaped config every call, so two snapshots hold DISTINCT
// array instances — an identity comparison inside restartValuesChanged would
// then report probe_targets as changed.
const makeRestartConfig = () => ({
  network: { port: 774, network_access: "localhost", https_enabled: false, tls_cert_path: "", tls_key_path: "" },
  paths: { database_path: "./moombox.db", log_file_path: "./logs/moombox.log" },
  logs: { log_max_file_size: 10, log_max_files: 5 },
  cookies: { cookie_file: "./cookies.txt", refresh_interval: 360, auto_enabled: false, browser_profile_dir: "" },
  connectivity: { probe_targets: ["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"] },
  memory: { sidecar_hard_limit_mb: 512 },
  bgutils: { use_sidecar: true },
});

test("resolveConfigPath: dotted lookup, undefined for absent branches", () => {
  const cfg = makeRestartConfig();
  assert.equal(resolveConfigPath(cfg, "network.port"), 774);
  assert.deepEqual(resolveConfigPath(cfg, "connectivity.probe_targets"), ["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"]);
  assert.equal(resolveConfigPath(cfg, "nope.missing"), undefined);
  assert.equal(resolveConfigPath(cfg, "network.missing"), undefined);
  assert.equal(resolveConfigPath(undefined, "network.port"), undefined);
});

test("restartValuesChanged: an unchanged save over all sixteen paths prompts nothing", () => {
  assert.equal(RESTART_FIELDS.length, 16, "the Web restart list has sixteen paths");
  const snap = snapshotRestartValues(makeRestartConfig(), RESTART_FIELDS);
  assert.equal(Object.keys(snap).length, 16, "every path is snapshotted, not just the network/log nine");
  const current = snapshotRestartValues(makeRestartConfig(), RESTART_FIELDS);
  assert.equal(restartValuesChanged(snap, current, RESTART_FIELDS), false, "an unchanged save must not prompt for a restart");
});

test("restartValuesChanged: an omitted false boolean is not a change", () => {
  const snap = snapshotRestartValues(makeRestartConfig(), RESTART_FIELDS);
  const served = makeRestartConfig();
  delete served.network.https_enabled; // the server omits a false field
  const current = snapshotRestartValues(served, RESTART_FIELDS);
  assert.equal(restartValuesChanged(snap, current, RESTART_FIELDS), false);
});

test("restartValuesChanged: real edits to the six formerly-unsnapshotted paths are detected", () => {
  const snap = snapshotRestartValues(makeRestartConfig(), RESTART_FIELDS);
  const probes = makeRestartConfig();
  probes.connectivity.probe_targets = ["1.1.1.1:443"];
  assert.equal(restartValuesChanged(snap, snapshotRestartValues(probes, RESTART_FIELDS), RESTART_FIELDS), true, "probe_targets");

  const sidecar = makeRestartConfig();
  sidecar.bgutils.use_sidecar = false;
  assert.equal(restartValuesChanged(snap, snapshotRestartValues(sidecar, RESTART_FIELDS), RESTART_FIELDS), true, "use_sidecar");

  const cookieFile = makeRestartConfig();
  cookieFile.cookies.cookie_file = "./other.txt";
  assert.equal(restartValuesChanged(snap, snapshotRestartValues(cookieFile, RESTART_FIELDS), RESTART_FIELDS), true, "cookie_file");

  const hardLimit = makeRestartConfig();
  hardLimit.memory.sidecar_hard_limit_mb = 1024;
  assert.equal(restartValuesChanged(snap, snapshotRestartValues(hardLimit, RESTART_FIELDS), RESTART_FIELDS), true, "sidecar_hard_limit_mb");

  // cookies.refresh_interval feeds a ticker built once at boot, so a save
  // without a restart changes nothing — the prompt is the only thing that says
  // so (WEB-7). MUTANT: drop its row from the list above — the snapshot stops
  // carrying the path, restartValuesChanged sees nothing move, and this fails.
  const refresh = makeRestartConfig();
  refresh.cookies.refresh_interval = 60;
  assert.equal(restartValuesChanged(snap, snapshotRestartValues(refresh, RESTART_FIELDS), RESTART_FIELDS), true, "refresh_interval");
});

test("channelTermsForSave: an untouched field keeps whatever shape the config holds", () => {
  // A named map with no `stream` key: the dialog shows "" for it, so an
  // untouched save must return it verbatim rather than clear the entry.
  const named = { live: "concert", vod: "archive" };
  assert.equal(channelTermsForSave(named, "", ""), named, "the same object, not a rebuilt one");
  // Same rule for a simple string the operator did not edit.
  assert.equal(channelTermsForSave("karaoke", "karaoke", "karaoke"), "karaoke");
  // And for a channel that never had terms.
  assert.equal(channelTermsForSave(undefined, "", ""), undefined);
});

test("channelTermsForSave: an edited field writes through, preserving a stream-keyed map", () => {
  const withStream = { stream: "karaoke", vod: "archive" };
  assert.deepEqual(
    channelTermsForSave(withStream, "karaoke", "singing"),
    { stream: "singing", vod: "archive" },
    "only the stream key is rewritten",
  );
  assert.equal(channelTermsForSave("karaoke", "karaoke", "singing"), "singing", "a simple string becomes the new string");
  assert.equal(channelTermsForSave(undefined, "", "singing"), "singing", "a new channel gets a simple string");
});

test("channelTermsForSave: clearing an edited field removes terms", () => {
  assert.equal(channelTermsForSave("karaoke", "karaoke", ""), undefined);
  assert.equal(channelTermsForSave({ stream: "karaoke" }, "karaoke", ""), undefined);
});

// streamUrl mirrors internal/tui/app_actions.go streamURL (the TUI's O C
// chord): an explicit url wins; else YouTube watch URL; Twitch VOD strips the
// tw_v prefix; Twitch live needs a channel name.
test("streamUrl: explicit url wins over derivation", () => {
  assert.equal(streamUrl({ url: "https://example/x", videoId: "abc", platform: "youtube" }), "https://example/x");
});
test("streamUrl: youtube derives the watch URL", () => {
  assert.equal(streamUrl({ videoId: "dQw4w9WgXcQ", platform: "youtube" }), "https://www.youtube.com/watch?v=dQw4w9WgXcQ");
});
test("streamUrl: twitch VOD strips the tw_v prefix", () => {
  assert.equal(streamUrl({ videoId: "tw_v123456", platform: "twitch", isVod: true }), "https://www.twitch.tv/videos/123456");
});
test("streamUrl: twitch live is the channel page, empty without a channel", () => {
  assert.equal(streamUrl({ videoId: "live1", platform: "twitch", channelName: "somestreamer" }), "https://www.twitch.tv/somestreamer");
  assert.equal(streamUrl({ videoId: "live1", platform: "twitch" }), "");
  assert.equal(streamUrl({ platform: "youtube" }), "");
  assert.equal(streamUrl(null), "");
});
