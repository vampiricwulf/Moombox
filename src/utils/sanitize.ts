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
    .replace(/[\x00-\x1F\x7F]/g, "") // Strip control characters
    .replace(/[<>:"/\\|?*]/g, "_")
    .replace(/\s+/g, " ")
    .trim();
}

