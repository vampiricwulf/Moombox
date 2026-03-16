/**
 * Shared utility functions for the Moombox dashboard.
 */

/**
 * Format seconds into H:MM:SS or M:SS timestamp.
 */
export function formatTimestamp(seconds) {
  if (seconds == null || !isFinite(seconds)) return "0:00";
  if (seconds < 0) seconds = 0;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  return `${m}:${String(s).padStart(2, "0")}`;
}

/**
 * Format bytes into human-readable size (e.g. 1.5MB).
 */
export function formatBytes(bytes) {
  if (bytes == null || !isFinite(bytes) || bytes < 0) bytes = 0;
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)}GB`;
}

/**
 * Format total seconds into human-readable duration (e.g. 2h 15m 30s).
 */
export function formatDurationSeconds(totalSeconds) {
  if (totalSeconds == null || !isFinite(totalSeconds) || totalSeconds < 0) totalSeconds = 0;
  const total = Math.floor(totalSeconds);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m ${seconds}s`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

/**
 * Format an ISO date string as relative time (e.g. "5m ago").
 */
export function formatRelativeTime(isoDate) {
  const date = new Date(isoDate);
  if (isNaN(date.getTime())) return "unknown";
  const now = new Date();
  const diffMs = now - date;
  const diffSecs = Math.floor(diffMs / 1000);

  if (diffSecs <= 0) return "just now";
  if (diffSecs < 60) return `${diffSecs}s ago`;
  const diffMins = Math.floor(diffSecs / 60);
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d ago`;
}

/**
 * Format milliseconds to H:MM:SS or M:SS (used in player chat timestamps).
 * Supports negative offsets.
 */
export function formatMsToTime(ms) {
  if (ms == null || !isFinite(ms)) return "0:00";
  const negative = ms < 0;
  const absTotalSec = Math.floor(Math.abs(ms) / 1000);
  const h = Math.floor(absTotalSec / 3600);
  const m = Math.floor((absTotalSec % 3600) / 60);
  const s = absTotalSec % 60;

  const prefix = negative ? "-" : "";
  if (h > 0) {
    return `${prefix}${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  }
  return `${prefix}${m}:${String(s).padStart(2, "0")}`;
}
