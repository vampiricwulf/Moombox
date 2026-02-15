/**
 * Centralized time formatting utilities.
 * Consolidates 7+ implementations scattered across the codebase.
 */

/**
 * Format duration in milliseconds to human-readable string with h/m/s labels.
 * Examples: "2h 15m 30s", "5m 12s", "45s"
 */
export function formatDuration(ms: number): string {
  const totalSeconds = Math.floor(ms / 1000);
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = totalSeconds % 60;

  if (h > 0) return `${h}h ${m}m ${s}s`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

/**
 * Format seconds to HH:MM:SS or MM:SS timestamp.
 * @param seconds - Duration in seconds
 * @param includeHours - If false and hours is 0, returns MM:SS format
 */
export function formatTime(seconds: number, includeHours = true): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);

  const mm = String(m).padStart(2, "0");
  const ss = String(s).padStart(2, "0");

  if (h > 0 || includeHours) {
    const hh = String(h).padStart(2, "0");
    return `${hh}:${mm}:${ss}`;
  }

  return `${m}:${ss}`;
}

/**
 * Format milliseconds to HH:MM:SS or MM:SS, handling negative values.
 * Used for video player timestamps.
 */
export function formatMsToTime(ms: number): string {
  const isNegative = ms < 0;
  const absMs = Math.abs(ms);
  const totalSeconds = Math.floor(absMs / 1000);
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = totalSeconds % 60;

  const mm = String(m).padStart(2, "0");
  const ss = String(s).padStart(2, "0");
  const prefix = isNegative ? "-" : "";

  if (h > 0) {
    const hh = String(h).padStart(2, "0");
    return `${prefix}${hh}:${mm}:${ss}`;
  }

  return `${prefix}${m}:${ss}`;
}

/**
 * Format ISO date string or Date object to YYYY-MM-DD HH:MM:SS.
 */
export function formatDateTime(dateStr: string | Date): string {
  const date = typeof dateStr === "string" ? new Date(dateStr) : dateStr;
  const y = date.getFullYear();
  const mo = String(date.getMonth() + 1).padStart(2, "0");
  const d = String(date.getDate()).padStart(2, "0");
  const h = String(date.getHours()).padStart(2, "0");
  const mi = String(date.getMinutes()).padStart(2, "0");
  const s = String(date.getSeconds()).padStart(2, "0");
  return `${y}-${mo}-${d} ${h}:${mi}:${s}`;
}

/**
 * Format microseconds timestamp to HH:MM:SS (used for chat timestamps).
 */
export function formatTimestampFromUsec(usec: string): string {
  const date = new Date(parseInt(usec, 10) / 1000);
  return date.toISOString().slice(11, 19);
}

/**
 * Format elapsed time in milliseconds to compact human-readable string.
 * Examples: "45s", "5m 12s", "2h 15m"
 */
export function formatElapsed(ms: number): string {
  const secs = Math.floor(ms / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  const remSecs = secs % 60;
  if (mins < 60) return `${mins}m ${remSecs}s`;
  const hrs = Math.floor(mins / 60);
  const remMins = mins % 60;
  return `${hrs}h ${remMins}m`;
}

/**
 * Format bytes to human-readable size with units.
 * Examples: "1.5 KB", "2.3 MB", "1.45 GB"
 */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

/**
 * Format download speed in bytes per second to human-readable string.
 * Examples: "150 B/s", "1.5 KB/s", "2.3 MB/s"
 */
export function formatSpeed(bytesPerSec: number): string {
  if (bytesPerSec < 1024) return `${bytesPerSec.toFixed(0)} B/s`;
  if (bytesPerSec < 1024 * 1024)
    return `${(bytesPerSec / 1024).toFixed(1)} KB/s`;
  return `${(bytesPerSec / 1024 / 1024).toFixed(1)} MB/s`;
}
