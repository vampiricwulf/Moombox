/**
 * Terminal display-width utilities for strings containing CJK/fullwidth characters.
 * CJK characters occupy 2 columns in a monospace terminal; ASCII occupies 1.
 */

function isFullwidth(code: number): boolean {
  return (
    (code >= 0x1100 && code <= 0x115f) || // Hangul Jamo
    (code >= 0x2e80 && code <= 0x303e) || // CJK Radicals, Kangxi, etc.
    (code >= 0x3040 && code <= 0x33bf) || // Hiragana, Katakana, CJK compat
    (code >= 0x3400 && code <= 0x4dbf) || // CJK Extension A
    (code >= 0x4e00 && code <= 0x9fff) || // CJK Unified Ideographs
    (code >= 0xa000 && code <= 0xa4cf) || // Yi
    (code >= 0xac00 && code <= 0xd7af) || // Hangul Syllables
    (code >= 0xf900 && code <= 0xfaff) || // CJK Compatibility Ideographs
    (code >= 0xfe10 && code <= 0xfe6f) || // CJK Compat Forms + Small Form Variants
    (code >= 0xff01 && code <= 0xff60) || // Fullwidth Forms
    (code >= 0xffe0 && code <= 0xffe6) || // Fullwidth Signs
    (code >= 0x20000 && code <= 0x2fa1f) // CJK Supplementary
  );
}

/** Returns the display column width of a string. */
export function displayWidth(str: string): number {
  let width = 0;
  for (const char of str) {
    width += isFullwidth(char.codePointAt(0)!) ? 2 : 1;
  }
  return width;
}

/** Truncate string to fit within `maxWidth` display columns, appending … if needed. */
export function truncateToWidth(str: string, maxWidth: number): string {
  if (displayWidth(str) <= maxWidth) return str;

  let width = 0;
  let result = "";
  for (const char of str) {
    const cw = isFullwidth(char.codePointAt(0)!) ? 2 : 1;
    if (width + cw > maxWidth - 1) break; // reserve 1 col for ellipsis
    result += char;
    width += cw;
  }
  return result + "\u2026";
}

/** Pad string with spaces to reach `targetWidth` display columns. */
export function padEndToWidth(str: string, targetWidth: number): string {
  const gap = targetWidth - displayWidth(str);
  return gap > 0 ? str + " ".repeat(gap) : str;
}

/** Word-wrap text to `maxWidth` display columns. */
export function wordWrapWidth(text: string, maxWidth: number): string[] {
  const lines: string[] = [];
  for (const paragraph of text.split("\n")) {
    if (displayWidth(paragraph) <= maxWidth) {
      lines.push(paragraph);
      continue;
    }
    let remaining = paragraph;
    while (displayWidth(remaining) > maxWidth) {
      // Find break point by walking codepoints (handles surrogate pairs)
      let width = 0;
      let breakIdx = 0;
      let lastSpace = -1;
      let i = 0;
      for (const char of remaining) {
        const cw = isFullwidth(char.codePointAt(0)!) ? 2 : 1;
        if (char === " ") lastSpace = i;
        if (width + cw > maxWidth) {
          breakIdx = lastSpace > 0 ? lastSpace : i;
          break;
        }
        width += cw;
        i += char.length; // 1 for BMP, 2 for surrogate pairs
        breakIdx = i;
      }
      if (breakIdx <= 0) breakIdx = 1;
      lines.push(remaining.substring(0, breakIdx));
      remaining = remaining.substring(breakIdx).trimStart();
    }
    if (remaining) lines.push(remaining);
  }
  return lines;
}
