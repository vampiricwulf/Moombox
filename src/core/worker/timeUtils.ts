/**
 * Time parsing utility for timestamp selection.
 *
 * Supports:
 * - Seconds: "120" → 120
 * - HH:MM:SS: "1:30:00" → 5400
 * - MM:SS: "5:30" → 330
 * - Invalid input → null
 */

/**
 * Parse a time string into seconds.
 * If the string contains colons, parse as HH:MM:SS or MM:SS.
 * Otherwise, parse as raw seconds.
 * Returns null for invalid input.
 */
export function parseTimeToSeconds(input: string): number | null {
  const trimmed = input.trim();
  if (!trimmed) return null;

  if (trimmed.includes(":")) {
    // HH:MM:SS or MM:SS format
    const parts = trimmed.split(":");
    if (parts.length < 2 || parts.length > 3) return null;

    const nums = parts.map((p) => {
      const n = Number(p);
      return isNaN(n) || n < 0 ? NaN : n;
    });

    if (nums.some(isNaN)) return null;

    if (parts.length === 3) {
      // HH:MM:SS
      return nums[0] * 3600 + nums[1] * 60 + nums[2];
    } else {
      // MM:SS
      return nums[0] * 60 + nums[1];
    }
  }

  // Raw seconds
  const seconds = Number(trimmed);
  if (isNaN(seconds) || seconds < 0) return null;
  return seconds;
}

/**
 * Format seconds into HH:MM:SS string.
 */
export function formatSecondsToTimestamp(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  const frac = seconds % 1;

  let result = "";
  if (h > 0) {
    result = `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  } else {
    result = `${m}:${String(s).padStart(2, "0")}`;
  }

  if (frac > 0) {
    result += `.${frac.toFixed(3).slice(2)}`;
  }

  return result;
}
