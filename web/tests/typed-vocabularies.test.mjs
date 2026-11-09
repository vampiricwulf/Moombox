// Closed vocabularies the dashboard mirrors from Go (audit rule 1, Arc E).
// cmd/moombox's vocabulary_parity_test.go compares the tables' keys with the
// Go constants; these pin what the dashboard does with each value.
import { test } from "node:test";
import assert from "node:assert/strict";

import { CHAT_STATUS_VARIANTS, chatStatusVariant } from "../public/modules/utils.js";

test("chatStatusVariant colours each chat_status the worker writes", () => {
  assert.deepEqual(
    Object.keys(CHAT_STATUS_VARIANTS).sort(),
    ["downloading", "finished", "incomplete", "pending", "unavailable"],
  );
  assert.equal(chatStatusVariant("downloading"), "primary");
  assert.equal(chatStatusVariant("finished"), "success");
  assert.equal(chatStatusVariant("incomplete"), "warning");
  assert.equal(chatStatusVariant("pending"), "neutral");
  assert.equal(chatStatusVariant("unavailable"), "neutral");
});

test("a value nothing writes, or an inherited key, is neutral", () => {
  assert.equal(chatStatusVariant("error"), "neutral");
  assert.equal(chatStatusVariant("toString"), "neutral");
  assert.equal(chatStatusVariant(undefined), "neutral");
  assert.throws(() => { "use strict"; CHAT_STATUS_VARIANTS.error = "danger"; });
});

test("isBackfillRetained keeps a scan in flight and a failed one, as monitor.BackfillState.Retained does", async () => {
  const { BACKFILL_STATE, isBackfillRetained } = await import("../public/modules/utils.js");
  assert.deepEqual(Object.values(BACKFILL_STATE).sort(), ["done", "error", "idle", "scanning"]);
  const retained = Object.values(BACKFILL_STATE).filter(isBackfillRetained).sort();
  assert.deepEqual(retained, ["error", "scanning"]);
  assert.equal(isBackfillRetained(undefined), false);
});
