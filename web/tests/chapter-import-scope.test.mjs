// Chapter import is offered only for a complete Finished job (owner decision,
// 2026-11-07; the server's importableJob): an Error, Cancelled or
// incomplete-tail job can still be resumed, and a resumed run rewrites the
// archive an import would be rewriting. The player hides its "Import
// chapters" button with chapterImportAllowed.
//
// Mutant: chapterImportAllowed accepting Error/Cancelled or ignoring
// incompleteTail.
import { test } from "node:test";
import assert from "node:assert/strict";
import { chapterImportAllowed } from "../public/modules/chapter-import.js";

test("chapter import is allowed only for a complete Finished job", () => {
  assert.equal(chapterImportAllowed({ status: "Finished" }), true);
  assert.equal(chapterImportAllowed({ status: "Finished", incompleteTail: true }), false);
  assert.equal(chapterImportAllowed({ status: "Error" }), false);
  assert.equal(chapterImportAllowed({ status: "Cancelled" }), false);
  assert.equal(chapterImportAllowed({ status: "Downloading" }), false);
  assert.equal(chapterImportAllowed(null), false);
});
