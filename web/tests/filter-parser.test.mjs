// Tests for web/public/modules/filter-parser.js — run with:
//   node --test web/tests/
//
// Uses Node's built-in test runner (no devDependencies). Modules under
// web/public/modules/ are vanilla ES modules; .mjs extension here tells Node
// to treat these test files as modules for import resolution.

import { test } from "node:test";
import assert from "node:assert/strict";

import { openToken, parseFilterQuery, serializeToken } from "../public/modules/filter-parser.js";

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

test("serializeToken: round-trips a value holding a pipe", () => {
  // Unquoted, channel:a|b re-parses as an OR group — a different query.
  const token = parseFilterQuery('channel:"a|b"')[0];
  assert.equal(serializeToken(token), 'channel:"a|b"');
  const reparsed = parseFilterQuery(serializeToken(token));
  assert.equal(reparsed.length, 1);
  assert.equal(reparsed[0].type, "channel");
  assert.equal(reparsed[0].value, "a|b");
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

// The values Go's TestSerializeRoundTrips (internal/jobfilter) runs through
// its twin: each used to serialize to a form that parsed back as something
// else — cut in two at an inner quote, swallowing the rest of the query,
// losing a literal quote pair, flipping to negated, or turning into a filter.
//
// Mutant: quoteValue reduced to the old space/pipe rule.
test("serializeToken round-trips values the old quoting broke", () => {
  const values = [`foo" bar`, `"foo bar`, `"quoted"`, `'single'`, `-dash`, `status:live`, `Channel:x`, `mori's set`, `a|b`, `plain`];
  for (const type of ["text", "channel"]) {
    for (const negate of [false, true]) {
      for (const value of values) {
        const q = [serializeToken({ type, value, negate }), "status:active"].join(" ");
        const got = parseFilterQuery(q);
        assert.deepEqual(got, [{ type, value, negate }, { type: "status", value: "active", negate: false }],
          `${type} ${JSON.stringify(value)} (negate ${negate}) serialized to ${q}`);
      }
    }
  }
});

// The filter bar's debounce chips only closed tokens; openToken names the one
// still being typed, exactly as typed. A token is open until an unquoted space
// follows it — an open quote keeps it open past spaces, and a closed quote
// does not close it (one more character would still join it).
//
// Mutants: openToken returning the last token whatever follows it — the
// trailing-space cases fail; returning "" always — every open case fails.
test("openToken: the token still being typed, as typed", () => {
  const cases = [
    ["", ""],
    ["   ", ""],
    ["status:", "status:"],
    ["status:li", "status:li"],
    ["status:live ", ""],
    ["night status:live", "status:live"],
    ["night status:live  ", ""],
    ['channel:"Shachi', 'channel:"Shachi'],
    ['channel:"Shachi ', 'channel:"Shachi '],
    ['channel:"Shachi Too"', 'channel:"Shachi Too"'],
    ['channel:"Shachi Too" ', ""],
    ["-", "-"],
    ["a|b", "a|b"],
    ["mori's", "mori's"],
  ];
  for (const [query, want] of cases) {
    assert.equal(openToken(query), want, `openToken(${JSON.stringify(query)})`);
  }
});
