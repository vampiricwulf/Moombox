/**
 * Stdin Mouse Filter
 *
 * Intercepts raw stdin data to strip SGR mouse escape sequences before Ink
 * processes them. Mouse data is routed to a separate callback so useMouse
 * can parse it without Ink treating it as typed characters.
 *
 * Patches both `push` (data entry into the readable stream) and `emit`
 * (event dispatch) to cover all Node.js internal data paths.
 *
 * Also handles split sequences: if a chunk ends with a partial escape
 * sequence, it is buffered and prepended to the next chunk.
 */

import { EventEmitter } from "events";

const SGR_MOUSE_RE = /\x1b\[<\d+;\d+;\d+[Mm]/g;

// Partial sequence: \x1b possibly followed by [, <, digits, semicolons
// (the sequence hasn't terminated with M or m yet)
const PARTIAL_MOUSE_RE = /\x1b(?:\[(?:<(?:\d+(?:;\d+(?:;\d+)?)?)?)?)?$/;

/** Global emitter for raw mouse data chunks */
export const mouseDataBus = new EventEmitter();

let installed = false;
let pendingBuf = "";

function filterMouseData(raw: string): string {
  // Prepend any buffered partial sequence from the previous chunk
  const str = pendingBuf + raw;
  pendingBuf = "";

  // Check for partial sequence at the end (split across chunks)
  const partialMatch = str.match(PARTIAL_MOUSE_RE);
  let toProcess = str;
  if (partialMatch) {
    pendingBuf = partialMatch[0];
    toProcess = str.slice(0, -pendingBuf.length);
  }

  // Extract complete mouse sequences
  const mouseMatches = toProcess.match(SGR_MOUSE_RE);
  if (mouseMatches) {
    for (const seq of mouseMatches) {
      mouseDataBus.emit("data", Buffer.from(seq));
    }
  }

  // Strip mouse sequences — remainder is for Ink
  const clean = toProcess.replace(SGR_MOUSE_RE, "");
  return clean;
}

/**
 * Flush any pending partial buffer (e.g. a lone \x1b that turned out not to
 * be a mouse sequence). Called on a short timer so real escape keypresses
 * still reach Ink promptly.
 */
let flushTimer: ReturnType<typeof setTimeout> | null = null;

function scheduleFlush(origPush: (chunk: any, encoding?: any) => boolean): void {
  if (flushTimer) clearTimeout(flushTimer);
  if (!pendingBuf) return;
  flushTimer = setTimeout(() => {
    if (pendingBuf) {
      const buf = pendingBuf;
      pendingBuf = "";
      origPush(Buffer.from(buf));
    }
    flushTimer = null;
  }, 50); // 50ms — long enough to catch the rest of a mouse sequence
}

/**
 * Install stdin filter. Must be called ONCE before Ink's render().
 */
export function installStdinMouseFilter(): void {
  if (installed) return;
  installed = true;

  // Patch push() — this is how data enters the readable stream's internal buffer.
  const origPush = process.stdin.push.bind(process.stdin);
  (process.stdin as any).push = function (chunk: any, encoding?: any) {
    if (chunk !== null && chunk !== undefined) {
      const str: string = Buffer.isBuffer(chunk)
        ? chunk.toString()
        : String(chunk);

      const clean = filterMouseData(str);
      scheduleFlush(origPush);

      if (clean.length === 0) {
        // All data was mouse sequences (or buffered as partial) — don't push
        return true;
      }
      return origPush(Buffer.from(clean), encoding);
    }
    return origPush(chunk, encoding);
  };

  // Also patch emit('data') as a secondary catch for any code path that
  // bypasses push() (e.g. some stream implementations emit data directly).
  const origEmit = process.stdin.emit.bind(process.stdin);
  (process.stdin as any).emit = function (event: string | symbol, ...args: any[]) {
    if (event === "data" && args[0]) {
      const str: string = Buffer.isBuffer(args[0])
        ? args[0].toString()
        : String(args[0]);

      // Check for mouse sequences in this chunk
      const mouseMatches = str.match(SGR_MOUSE_RE);
      if (mouseMatches) {
        for (const seq of mouseMatches) {
          mouseDataBus.emit("data", Buffer.from(seq));
        }

        const clean = str.replace(SGR_MOUSE_RE, "");
        if (clean.length === 0) return true;
        return origEmit(event, Buffer.from(clean));
      }
    }
    return origEmit(event, ...args);
  };
}
