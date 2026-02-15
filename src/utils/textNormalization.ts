/**
 * Text Normalization Utilities
 *
 * Provides text normalization and fuzzy matching for handling non-ASCII
 * characters in RSS feeds and other text-based comparisons.
 */

/**
 * Normalize text by removing diacritics, converting to lowercase,
 * and normalizing whitespace.
 *
 * @param text - Text to normalize
 * @returns Normalized text suitable for fuzzy matching
 */
export function normalizeText(text: string): string {
  // Normalize to NFD (decompose accented characters)
  let normalized = text.normalize("NFD");

  // Remove diacritical marks (combining characters)
  normalized = normalized.replace(/[\u0300-\u036f]/g, "");

  // Convert to lowercase
  normalized = normalized.toLowerCase();

  // Normalize whitespace (collapse multiple spaces to one, trim)
  normalized = normalized.replace(/\s+/g, " ").trim();

  return normalized;
}

/**
 * Test if text matches a pattern using fuzzy matching
 * (normalized text comparison).
 *
 * @param text - Text to test
 * @param pattern - Regular expression pattern
 * @returns True if normalized text matches the pattern
 */
export function fuzzyMatch(text: string, pattern: RegExp): boolean {
  return pattern.test(normalizeText(text));
}
