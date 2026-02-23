import React, { useState, useEffect } from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { displayWidth, truncateToWidth, wordWrapWidth } from "../textWidth.js";
import { getStatusColor } from "./TaskItem.js";
import { getProgress, type ProgressData } from "../progressStore.js";

interface JobDetailsProps {
  job: Job | undefined;
  width: number;
  height: number;
  focused: boolean;
  scrollOffset: number;
  onMaxScroll?: (max: number) => void;
}

export function JobDetails({ job, width, height, focused, scrollOffset, onMaxScroll }: JobDetailsProps): React.ReactElement {
  // Tick for jobs with time-dependent or progress fields.
  // Scoped here so only JobDetails re-renders, not the full App tree.
  // Active downloads (Live/Downloading/Muxing): 100ms for real-time progress from progressStore.
  // Upcoming with scheduled time: 1s for "Starts In" countdown.
  const isActive = !!job && ["Live", "Downloading", "Muxing"].includes(job.status);
  const isUpcoming = !!job && job.status === "Upcoming" && !!job.streamStartTime;
  const needsTick = isActive || isUpcoming;
  const tickInterval = isActive ? 100 : 1_000;
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!needsTick) return;
    const timer = setInterval(() => setTick((t) => t + 1), tickInterval);
    return () => clearInterval(timer);
  }, [needsTick, tickInterval]);

  const contentWidth = width - 4; // borders + padding
  const contentHeight = height - 3; // borders (2) + title row (1)

  // Read progress data from store (updated outside React state at 10Hz)
  const progressData = job ? getProgress(job.id) : undefined;

  // Compute rows/scroll BEFORE early return so all hooks run unconditionally
  const rows = job ? buildDetailRows(job, contentWidth, progressData) : [];
  const maxScroll = Math.max(0, rows.length - contentHeight);
  const actualOffset = Math.min(scrollOffset, maxScroll);
  const visibleRows = rows.slice(actualOffset, actualOffset + contentHeight);

  // Report maxScroll so parent can clamp offset in scroll handlers
  useEffect(() => { onMaxScroll?.(maxScroll); }, [maxScroll, onMaxScroll]);

  const borderColor = focused ? "cyan" : "gray";
  const titleColor = focused ? "cyan" : "white";

  // Status-colored border when a job is selected
  const statusBorderColor = job && focused ? getStatusColor(job.status) : borderColor;
  const statusTitleColor = job && focused ? getStatusColor(job.status) : titleColor;

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

  const scrollPercent = rows.length > contentHeight
    ? Math.round((actualOffset / maxScroll) * 100)
    : 100;

  return (
    <Box
      flexDirection="column"
      width={width}
      height={height}
      borderStyle="round"
      borderColor={statusBorderColor}
    >
      <Box paddingX={1}>
        <Text color={statusTitleColor} bold>Details</Text>
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
            ) : row.type === "progress_bar" ? (
              <>{renderProgressBar(row, contentWidth)}</>
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
  | { type: "separator" }
  | { type: "progress_bar"; percent: number; color: string };

function buildDetailRows(job: Job, maxWidth: number, pd?: ProgressData): DetailRow[] {
  const valueWidth = Math.max(10, maxWidth - 14);
  const truncate = (s: string) => displayWidth(s) > valueWidth ? truncateToWidth(s, valueWidth) : s;

  const rows: DetailRow[] = [];

  const isTwitch = job.platform === "twitch";

  // Basic info
  rows.push({ type: "field", label: "Title", value: truncate(job.title) });
  rows.push({ type: "field", label: "Channel", value: job.channelName || "Unknown" });
  rows.push({ type: "field", label: isTwitch ? "Stream ID" : "Video ID", value: job.videoId || job.id });
  if (job.url) {
    rows.push({ type: "field", label: "URL", value: truncate(job.url) });
  }
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

  const isFinished = job.status === "Finished";

  // Read progress fields from store (real-time) with fallback to job (for finished/initial)
  const lastVideoSeq = pd?.lastVideoSeq ?? job.lastVideoSeq;
  const lastAudioSeq = pd?.lastAudioSeq ?? job.lastAudioSeq;
  const totalVideoSeq = pd?.totalVideoSeq ?? job.totalVideoSeq;
  const totalAudioSeq = pd?.totalAudioSeq ?? job.totalAudioSeq;
  const totalChatMessages = pd?.totalChatMessages ?? job.totalChatMessages;
  const chatStatus = pd?.chatStatus ?? job.chatStatus;
  const progress = pd?.progress ?? job.progress;
  const percent = pd?.percent ?? job.percent;
  const speed = pd?.speed ?? job.speed;
  const eta = pd?.eta ?? job.eta;

  // Helper: segment/chat/gap rows (shared between Progress and Media sections)
  const addSegmentRows = () => {
    if (lastVideoSeq !== undefined) {
      const vTotal = totalVideoSeq ? `/${totalVideoSeq}` : "";
      rows.push({ type: "field", label: isTwitch ? "Segments" : "Video Segs", value: `${lastVideoSeq}${vTotal}` });
    }

    if (!isTwitch && lastAudioSeq !== undefined) {
      const aTotal = totalAudioSeq ? `/${totalAudioSeq}` : "";
      rows.push({ type: "field", label: "Audio Segs", value: `${lastAudioSeq}${aTotal}` });
    }

    if (totalChatMessages && totalChatMessages > 0) {
      rows.push({ type: "field", label: "Chat Msgs", value: totalChatMessages.toString() });
    }

    if (job.gaps && job.gaps.length > 0) {
      const videoGaps = job.gaps.filter(g => g.stream === "video").length;
      const audioGaps = job.gaps.filter(g => g.stream === "audio").length;
      const parts: string[] = [];
      if (videoGaps > 0) parts.push(`video: ${videoGaps}`);
      if (audioGaps > 0) parts.push(`audio: ${audioGaps}`);
      const detail = parts.length > 0 ? ` (${parts.join(", ")})` : "";
      rows.push({ type: "field", label: "Gaps", value: `${job.gaps.length} segments${detail}`, color: "yellow" });
    }
  };

  // Progress section (hidden when Finished — contents merge into Media)
  if (!isFinished) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: "Progress" });

    if (progress) {
      rows.push({ type: "field", label: "Progress", value: progress });
    }

    if (percent > 0) {
      rows.push({ type: "progress_bar", percent, color: getStatusColor(job.status) });
    }

    if (speed) {
      rows.push({ type: "field", label: "Speed", value: speed });
    }

    if (eta) {
      rows.push({ type: "field", label: "ETA", value: eta });
    }

    addSegmentRows();

    if (chatStatus) {
      const chatColor = chatStatus === "downloading" ? "green"
        : chatStatus === "finished" ? "cyan"
        : chatStatus === "error" ? "red"
        : "gray";
      rows.push({ type: "field", label: "Chat", value: chatStatus, color: chatColor });
    }
  }

  // Media section
  const hasMedia = (job.videoWidth && job.videoHeight) || job.videoFps || job.fileSize;
  const hasFinishedStats = isFinished && (job.lastVideoSeq !== undefined || (job.totalChatMessages && job.totalChatMessages > 0) || (job.gaps && job.gaps.length > 0));
  if (hasMedia || hasFinishedStats) {
    rows.push({ type: "separator" });
    rows.push({ type: "header", text: "Media" });

    if (job.videoWidth && job.videoHeight) {
      rows.push({ type: "field", label: "Resolution", value: `${job.videoWidth}x${job.videoHeight}` });
    }

    if (job.videoFps) {
      rows.push({ type: "field", label: "FPS", value: String(job.videoFps) });
    }

    if (job.fileSize) {
      rows.push({ type: "field", label: "File Size", value: formatBytes(job.fileSize) });
    }

    // When Finished, segment/chat/gap stats appear here instead of Progress
    if (isFinished) {
      addSegmentRows();
    }
  }

  // Timestamps
  rows.push({ type: "header", text: "Timestamps" });
  rows.push({ type: "field", label: "Created", value: formatDate(job.createdAt) });
  if (job.downloadStartedAt) {
    rows.push({ type: "field", label: "DL Started", value: formatDate(job.downloadStartedAt) });
  }
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

function renderProgressBar(row: { percent: number; color: string }, contentWidth: number): React.ReactElement {
  const label = "              "; // 14-char label padding (matches field layout)
  const percentLabel = ` ${row.percent.toFixed(1)}%`;
  const barMaxWidth = Math.min(30, contentWidth - 14 - percentLabel.length - 2); // 2 for brackets
  if (barMaxWidth < 5) {
    return <Text color={row.color}>{label}{row.percent.toFixed(1)}%</Text>;
  }
  const filled = Math.round((row.percent / 100) * barMaxWidth);
  const empty = barMaxWidth - filled;
  return (
    <>
      <Text color="gray">{label}</Text>
      <Text color={row.color}>{"\u2588".repeat(filled)}</Text>
      <Text color="gray">{"\u2591".repeat(empty)}</Text>
      <Text color={row.color}>{percentLabel}</Text>
    </>
  );
}

