/**
 * YouTube-related utility functions.
 */

/**
 * Extract video ID from various YouTube URL formats or validate raw ID.
 * Supports:
 * - 11-character IDs (e.g., "dQw4w9WgXcQ")
 * - youtube.com/watch?v=ID
 * - youtu.be/ID
 * - youtube.com/live/ID
 * - youtube.com/shorts/ID
 * - youtube.com/embed/ID
 * - youtube.com/v/ID
 */
export function extractVideoId(input: string): string | null {
  const trimmed = input.trim();

  // Direct 11-character ID
  if (/^[a-zA-Z0-9_-]{11}$/.test(trimmed)) {
    return trimmed;
  }

  // URL patterns
  const patterns = [
    /(?:youtube\.com\/watch\?v=|youtu\.be\/|youtube\.com\/embed\/|youtube\.com\/v\/|youtube\.com\/shorts\/)([a-zA-Z0-9_-]{11})/,
    /youtube\.com\/live\/([a-zA-Z0-9_-]{11})/,
  ];

  for (const pattern of patterns) {
    const match = trimmed.match(pattern);
    if (match) {
      return match[1];
    }
  }

  return null;
}
