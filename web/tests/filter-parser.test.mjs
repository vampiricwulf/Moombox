// Tests for web/public/modules/filter-parser.js — run with:
//   node --test web/tests/
//
// Uses Node's built-in test runner (no devDependencies). Modules under
// web/public/modules/ are vanilla ES modules; .mjs extension here tells Node
// to treat these test files as modules for import resolution.

import { test } from "node:test";
import assert from "node:assert/strict";

import { parseFilterQuery, serializeToken } from "../public/modules/filter-parser.js";

test("parseFilterQuery: empty string returns []", () => {
  assert.deepEqual(parseFilterQuery(""), []);
  assert.deepEqual(parseFilterQuery("   "), []);
  assert.deepEqual(parseFilterQuery(null), []);
});

test("parseFilterQuery: single text term", () => {
  assert.deepEqual(parseFilterQuery("hello"), [
    { type: "text", value: "hello", negate: false },
  ]);
});

test("parseFilterQuery: negated text term", () => {
  assert.deepEqual(parseFilterQuery("-spam"), [
    { type: "text", value: "spam", negate: true },
  ]);
});

test("parseFilterQuery: multiple terms (AND)", () => {
  assert.deepEqual(parseFilterQuery("foo bar -baz"), [
    { type: "text", value: "foo", negate: false },
    { type: "text", value: "bar", negate: false },
    { type: "text", value: "baz", negate: true },
  ]);
});

test("parseFilterQuery: namespaced term", () => {
  assert.deepEqual(parseFilterQuery("status:active"), [
    { type: "status", value: "active", negate: false },
  ]);
});

test("parseFilterQuery: quoted namespaced value", () => {
  assert.deepEqual(parseFilterQuery('channel:"shachi too"'), [
    { type: "channel", value: "shachi too", negate: false },
  ]);
});

test("parseFilterQuery: OR via pipe", () => {
  assert.deepEqual(parseFilterQuery("foo|bar"), [
    {
      type: "or",
      terms: [
        { type: "text", value: "foo", negate: false },
        { type: "text", value: "bar", negate: false },
      ],
    },
  ]);
});

test("parseFilterQuery: unknown namespace falls back to text type", () => {
  assert.deepEqual(parseFilterQuery("foo:bar"), [
    { type: "text", value: "foo:bar", negate: false },
  ]);
});

test("parseFilterQuery: pipe inside quotes is literal, not OR", () => {
  assert.deepEqual(parseFilterQuery('channel:"a|b"'), [
    { type: "channel", value: "a|b", negate: false },
  ]);
});

test("parseFilterQuery: negated namespaced", () => {
  assert.deepEqual(parseFilterQuery("-platform:twitch"), [
    { type: "platform", value: "twitch", negate: true },
  ]);
});

test("serializeToken: round-trips plain text", () => {
  const token = { type: "text", value: "hello", negate: false };
  assert.equal(serializeToken(token), "hello");
});

test("serializeToken: round-trips negated text", () => {
  assert.equal(serializeToken({ type: "text", value: "spam", negate: true }), "-spam");
});

test("serializeToken: round-trips namespaced with space → quoted", () => {
  assert.equal(
    serializeToken({ type: "channel", value: "shachi too", negate: false }),
    'channel:"shachi too"',
  );
});

test("serializeToken: round-trips OR group", () => {
  const or = {
    type: "or",
    terms: [
      { type: "text", value: "foo", negate: false },
      { type: "text", value: "bar", negate: true },
    ],
  };
  assert.equal(serializeToken(or), "foo|-bar");
});

test("parse → serialize → parse is identity for common queries", () => {
  const queries = [
    "foo",
    "-spam",
    "status:active",
    'channel:"shachi too"',
    "foo|bar",
    "foo bar -baz status:active",
  ];
  for (const q of queries) {
    const parsed = parseFilterQuery(q);
    const serialized = parsed.map(serializeToken).join(" ");
    const reparsed = parseFilterQuery(serialized);
    assert.deepEqual(reparsed, parsed, `identity failed for: ${q}`);
  }
});
