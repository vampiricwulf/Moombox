/**
 * Parse a filter query string into token objects.
 *
 * Syntax:
 *   - Space-separated tokens are AND (intersection)
 *   - Pipe | within a token is OR (union)
 *   - Prefix - negates a token
 *   - Namespace prefix type:value for structured filters
 *   - Quotes for values with spaces: channel:"shachi too"
 *
 * Token types:
 *   { type: "text"|"status"|"channel"|"platform", value: string, negate: boolean }
 *   { type: "or", terms: [{ type, value, negate }, ...] }
 */

const NAMESPACES = new Set(["status", "channel", "platform"]);

/**
 * Parse a single term (no pipes) into a token object.
 * @param {string} raw - e.g. "status:active", "-jelly", "channel:\"shachi too\""
 * @returns {{ type: string, value: string, negate: boolean }}
 */
function parseTerm(raw) {
  let negate = false;
  let s = raw;
  if (s.startsWith("-")) {
    negate = true;
    s = s.slice(1);
  }
  const colonIdx = s.indexOf(":");
  if (colonIdx > 0) {
    const ns = s.slice(0, colonIdx).toLowerCase();
    if (NAMESPACES.has(ns)) {
      let value = s.slice(colonIdx + 1);
      value = stripQuotePair(value);
      return { type: ns, value, negate };
    }
  }
  // Bare quoted phrases ("jelly fin") arrive as one token WITH the quotes —
  // strip them here too, or the engine would substring-match the literal
  // quote characters and never find anything.
  return { type: "text", value: stripQuotePair(s), negate };
}

/** Strip one surrounding matching quote pair, if present. */
function stripQuotePair(value) {
  if (value.length >= 2 &&
      ((value.startsWith('"') && value.endsWith('"')) ||
       (value.startsWith("'") && value.endsWith("'")))) {
    return value.slice(1, -1);
  }
  return value;
}

/**
 * Tokenize a raw query string, respecting quotes.
 * Returns array of raw string tokens split on unquoted spaces.
 */
function tokenize(query) {
  const tokens = [];
  let current = "";
  let inQuote = null;
  for (let i = 0; i < query.length; i++) {
    const ch = query[i];
    if (inQuote) {
      current += ch;
      if (ch === inQuote) inQuote = null;
    } else if ((ch === '"' || ch === "'") && (current === "" || current === "-" || current.endsWith(":"))) {
      // A quote only opens quoting at token start, after a bare negation
      // prefix (-"jelly fin"), or right after a filter-key colon
      // (channel:"shachi too"). Mid-token it's a literal character, so
      // natural apostrophes (mori's karaoke) don't swallow the rest of the
      // query.
      inQuote = ch;
      current += ch;
    } else if (ch === " ") {
      if (current) tokens.push(current);
      current = "";
    } else {
      current += ch;
    }
  }
  if (current) tokens.push(current);
  return tokens;
}

/**
 * Parse a full filter query string into an array of token objects.
 * @param {string} query
 * @returns {Array<{ type: string, value: string, negate: boolean } | { type: "or", terms: Array }>}
 */
export function parseFilterQuery(query) {
  if (!query || !query.trim()) return [];
  const rawTokens = tokenize(query.trim());
  return rawTokens.map(raw => {
    // Check for pipe (OR) — but not inside quotes
    if (raw.includes("|") && !raw.includes('"') && !raw.includes("'")) {
      const parts = raw.split("|").filter(Boolean);
      if (parts.length > 1) {
        return { type: "or", terms: parts.map(p => parseTerm(p)) };
      }
    }
    return parseTerm(raw);
  });
}

/**
 * Serialize a token back to query string form.
 * @param {{ type: string, value: string, negate: boolean } | { type: "or", terms: Array }} token
 * @returns {string}
 */
export function serializeToken(token) {
  if (token.type === "or") {
    return token.terms.map(t => serializeToken(t)).join("|");
  }
  const prefix = token.negate ? "-" : "";
  if (token.type === "text") {
    // Re-quote spaced phrases or the round-trip corrupts them: the chip
    // extractor rewrites leftover text tokens into the input, and an
    // unquoted `-jelly fin` re-tokenizes as TWO tokens — flipping half the
    // phrase from negated to required.
    const needsQuotes = token.value.includes(" ");
    const val = needsQuotes ? `"${token.value}"` : token.value;
    return `${prefix}${val}`;
  }
  const needsQuotes = token.value.includes(" ");
  const val = needsQuotes ? `"${token.value}"` : token.value;
  return `${prefix}${token.type}:${val}`;
}
