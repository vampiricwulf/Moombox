/**
 * String sanitization utilities for filenames and templates.
 */

/**
 * Sanitize string for filesystem use (Windows/Unix compatible).
 * Removes characters that are invalid in filenames: < > : " / \ | ? *
 * @param s - String to sanitize
 * @returns Sanitized string safe for use in filenames
 */
export function sanitizeForFilename(s: string): string {
  return s
    .replace(/[<>:"/\\|?*]/g, "_")
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * Sanitize string for config templates (keeps CJK characters).
 * Removes most special characters but preserves Unicode letters, digits, spaces, and hyphens.
 * @param s - String to sanitize
 * @returns Sanitized string safe for templates
 */
export function sanitizeForTemplate(s: string): string {
  // Keep: word chars, spaces, hyphens, and CJK ranges
  return s
    .replace(
      /[^\w\s\-\u3000-\u303F\u3040-\u309F\u30A0-\u30FF\uFF00-\uFFEF\u4E00-\u9FAF]/g,
      "",
    )
    .trim();
}
