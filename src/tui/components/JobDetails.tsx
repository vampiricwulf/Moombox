import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { displayWidth, truncateToWidth, wordWrapWidth } from "../textWidth.js";
import { getStatusColor } from "./TaskItem.js";

interface JobDetailsProps {
  job: Job | undefined;
  width: number;
  height: number;
  focused: boolean;
  scrollOffset: number;
}

export function JobDetails({ job, width, height, focused, scrollOffset }: JobDetailsProps): React.ReactElement {
  const contentWidth = width - 4; // borders + padding
  const contentHeight = height - 3; // borders (2) + title row (1)

  const borderColor = focused ? "cyan" : "gray";
  const titleColor = focused ? "cyan" : "white";

  if (!job) {
    return (
      <Box
        flexDirection="column"
        width={width}
        height={height}
        borderStyle="round"
        borderColor={borderColor}
      >
        <Box paddingX={1}>
          <Text color={titleColor} bold>Details</Text>
        </Box>
        <Box paddingX={1} paddingY={1}>
          <Text color="gray">No task selected.</Text>
        </Box>
      </Box>
    );
  }

  const rows = buildDetailRows(job, contentWidth);
  const maxScroll = Math.max(0, rows.length - contentHeight);
  const actualOffset = Math.min(scrollOffset, maxScroll);
  const visibleRows = rows.slice(actualOffset, actualOffset + contentHeight);

  const scrollPercent = rows.length > contentHeight
    ? Math.round((actualOffset / maxScroll) * 100)
    : 100;

  return (
    <Box
      flexDirection="column"
      width={width}
      height={height}
      borderStyle="round"
      borderColor={borderColor}
    >
      <Box paddingX={1}>
        <Text color={titleColor} bold>Details</Text>
        {rows.length > contentHeight && (
          <Text color="gray"> [{scrollPercent}%]</Text>
        )}
      </Box>

      <Box flexDirection="column" paddingX={1} overflow="hidden">
        {visibleRows.map((row, idx) => (
          <Box key={actualOffset + idx} height={1} width={contentWidth}>
            {row.type === "header" ? (
              <Text color="cyan" bold>{row.text}</Text>
            ) : row.type === "separator" ? (
              <Text color="gray">{"\u2500".repeat(Math.min(contentWidth, 40))}</Text>
            ) : (
              <>
                <Text color="gray">{row.label!.padEnd(14)}</Text>
                <Text color={row.color || "white"} wrap="truncate">{row.value}</Text>
              </>
            )}
          </Box>
        ))}
      </Box>
    </Box>
  );
}

type DetailRow =
  | { type: "field"; label: string; value: string; color?: string }
  | { type: "header"; text: string }
  | { type: "separator" };

function buildDetailRows(job: Job, maxWidth: number): DetailRow[] {
  const valueWidth = Math.max(10, maxWidth - 14);
  const truncate = (s: string) => displayWidth(s) > valueWidth ? truncateToWidth(s, valueWidth) : s;

  const rows: DetailRow[] = [];

  const isTwitch = job.platform === "twitch";

  // Basic info
  rows.push({ type: "field", label: "Title", value: truncate(job.title) });
  rows.push({ type: "field", label: "Channel", value: job.channelName || "Unknown" });
  rows.push({ type: "field", label: isTwitch ? "Stream ID" : "Video ID", value: job.videoId || job.id });
  rows.push({ type: "field", label: "Status", value: job.status, color: getStatusColor(job.status) });
  if (isTwitch) {
    rows.push({ type: "field", label: "Platform", value: "Twitch", color: "magenta" });
  }
  if (isTwitch && job.twitchCategory) {
    rows.push({ type: "field", label: "Category", value: truncate(job.twitchCategory) });
  }
  if (isTwitch && job.twitchQuality) {
    rows.push({ type: "field", label: "Quality", value: job.twitchQuality });
  }

  // Advanced Options section (YouTube only — Twitch doesn't have itag selection)
  if (!isTwitch && (job.selectedVideoItag != null || job.selectedAudioItag != null || job.startTime != null || job.endTime != null)) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: "Advanced Options" });

    if (job.selectedVideoItag != null) {
      const formatLabel = job.selectedVideoItag === -1 ? "None (audio only)" : `itag ${job.selectedVideoItag}`;
      rows.push({ type: "field", label: "Video Format", value: formatLabel, color: "magenta" });
    }

    if (job.selectedAudioItag != null) {
      const formatLabel = job.selectedAudioItag === -1 ? "None (video only)" : `itag ${job.selectedAudioItag}`;
      rows.push({ type: "field", label: "Audio Format", value: formatLabel, color: "magenta" });
    }

    if (job.startTime != null || job.endTime != null) {
      const startMin = Math.floor((job.startTime || 0) / 60);
      const startSec = (job.startTime || 0) % 60;
      const startStr = `${startMin}:${String(startSec).padStart(2, "0")}`;
      const endStr = job.endTime != null
        ? `${Math.floor(job.endTime / 60)}:${String(job.endTime % 60).padStart(2, "0")}`
        : "end";
      rows.push({ type: "field", label: "Time Range", value: `${startStr} - ${endStr}`, color: "magenta" });
    }
  }

  // Trims section (if any exist)
  if (job.trims && job.trims.length > 0) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: `Trims (${job.trims.length})` });

    for (const trim of job.trims) {
      const range = `${formatTime(trim.startTime)} - ${formatTime(trim.endTime)}`;
      const duration = `${Math.floor(trim.duration)}s`;
      const size = trim.fileSize ? formatBytes(trim.fileSize) : "?";
      rows.push({
        type: "field",
        label: range,
        value: `${duration}, ${size}`,
        color: "cyan",
      });
    }
  }

  rows.push({ type: "separator" });

  // Progress section
  rows.push({ type: "header", text: "Progress" });

  if (job.progress) {
    rows.push({ type: "field", label: "Progress", value: job.progress });
  }

  if (job.speed) {
    rows.push({ type: "field", label: "Speed", value: job.speed });
  }

  if (job.lastVideoSeq !== undefined) {
    const vTotal = job.totalVideoSeq ? `/${job.totalVideoSeq}` : "";
    rows.push({ type: "field", label: isTwitch ? "Segments" : "Video Segs", value: `${job.lastVideoSeq}${vTotal}` });
  }

  if (!isTwitch && job.lastAudioSeq !== undefined) {
    const aTotal = job.totalAudioSeq ? `/${job.totalAudioSeq}` : "";
    rows.push({ type: "field", label: "Audio Segs", value: `${job.lastAudioSeq}${aTotal}` });
  }

  if (job.totalChatMessages && job.totalChatMessages > 0) {
    rows.push({ type: "field", label: "Chat Msgs", value: job.totalChatMessages.toString() });
  }

  rows.push({ type: "separator" });

  // Timestamps
  rows.push({ type: "header", text: "Timestamps" });
  rows.push({ type: "field", label: "Created", value: formatDate(job.createdAt) });
  rows.push({ type: "field", label: "Updated", value: formatDate(job.updatedAt) });

  if (job.streamStartTime) {
    const isScheduled = job.status === "Upcoming" && new Date(job.streamStartTime).getTime() > Date.now();
    rows.push({ type: "field", label: isScheduled ? "Scheduled" : "Stream Start", value: formatDate(job.streamStartTime) });
  }

  if (job.streamEndTime) {
    rows.push({ type: "field", label: "Stream End", value: formatDate(job.streamEndTime) });
  }

  if (job.lengthSeconds && job.lengthSeconds > 0) {
    rows.push({ type: "field", label: "Video Length", value: formatDuration(job.lengthSeconds * 1000) });
  }

  // Duration
  const now = Date.now();
  if (["Live", "Downloading", "Muxing"].includes(job.status)) {
    const startMs = job.streamStartTime ? new Date(job.streamStartTime).getTime() : new Date(job.createdAt).getTime();
    rows.push({ type: "field", label: "Duration", value: formatDuration(now - startMs), color: "green" });
  } else if (job.status === "Finished") {
    // Prefer video length (actual content length) over job time for Finished display
    if (!job.lengthSeconds || job.lengthSeconds <= 0) {
      const totalMs = new Date(job.updatedAt).getTime() - new Date(job.createdAt).getTime();
      rows.push({ type: "field", label: "Job Time", value: formatDuration(totalMs) });
    }
  } else if (job.status === "Upcoming" && job.streamStartTime) {
    const startMs = new Date(job.streamStartTime).getTime();
    if (startMs > now) {
      rows.push({ type: "field", label: "Starts In", value: formatDuration(startMs - now), color: "blue" });
    }
  }

  // Error
  if (job.error) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: "Error" });
    // Split long errors across multiple lines
    const errorLines = wordWrapWidth(job.error, valueWidth);
    for (const line of errorLines) {
      rows.push({ type: "field", label: "", value: line, color: "red" });
    }
  }

  // Output file
  if (job.filename) {
    rows.push({ type: "separator" });
    rows.push({ type: "field", label: "File", value: truncate(job.filename) });
  }

  // Description
  if (job.description) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: "Description" });
    const descLines = wordWrapWidth(job.description, valueWidth);
    for (const line of descLines) {
      rows.push({ type: "field", label: "", value: line });
    }
  }

  return rows;
}

function formatDate(dateStr?: string): string {
  if (!dateStr) return "N/A";
  const date = new Date(dateStr);
  const y = date.getFullYear();
  const mo = String(date.getMonth() + 1).padStart(2, "0");
  const d = String(date.getDate()).padStart(2, "0");
  const h = String(date.getHours()).padStart(2, "0");
  const mi = String(date.getMinutes()).padStart(2, "0");
  const s = String(date.getSeconds()).padStart(2, "0");
  return `${y}-${mo}-${d} ${h}:${mi}:${s}`;
}


function formatDuration(millis: number): string {
  millis = Math.max(0, millis);
  const totalSeconds = Math.floor(millis / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  } else if (minutes > 0) {
    return `${minutes}m ${seconds}s`;
  }
  return `${seconds}s`;
}

function formatTime(seconds: number): string {
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)}GB`;
}

