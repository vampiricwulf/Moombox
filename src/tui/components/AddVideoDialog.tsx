/**
 * Multi-step wizard dialog for adding videos with advanced options.
 *
 * Flow:
 * 1. Enter URL/ID (Enter = quick add, Shift+Enter = advanced options)
 * 2. (Advanced) Select video format (numbered list, 'a' = auto, 'n' = none)
 * 3. (Advanced) Select audio format
 * 4. (Advanced) Start time (HH:MM:SS, MM:SS, or seconds)
 * 5. (Advanced) End time
 * 6. (Advanced) Confirmation
 */

import React, { useState, useCallback, useEffect } from "react";
import { Box, Text, useInput } from "ink";
import { Database } from "../../core/database.js";
import { Logger } from "../../core/logger.js";
import { NotificationManager, NotificationType } from "../../core/notifications.js";
import { extractVideoId } from "../../utils/youtube.js";
import { parseTimeToSeconds, formatSecondsToTimestamp } from "../../core/worker/timeUtils.js";
import { useMouse, MouseEvent as TuiMouseEvent } from "../hooks/useMouse.js";
import { readClipboard } from "../clipboard.js";

interface AddVideoDialogProps {
  width: number;
  height: number;
  onComplete: (message: { text: string; color: string }) => void;
  onCancel: () => void;
}

interface VideoFormat {
  itag: number;
  mimeType?: string;
  width?: number;
  height?: number;
  fps?: number;
  bitrate: number;
  qualityLabel?: string;
}

interface AudioFormat {
  itag: number;
  mimeType?: string;
  bitrate: number;
  audioQuality?: string;
  audioSampleRate?: string;
}

interface FormatsData {
  videoId: string;
  title: string;
  channelName: string;
  lengthSeconds?: number;
  streamStatus?: string;
  videoFormats: VideoFormat[];
  audioFormats: AudioFormat[];
  bestItags: {
    bestWebmVideo: number | null;
    bestMp4Video: number | null;
    bestOpusAudio: number | null;
    bestAacAudio: number | null;
  };
}

export function AddVideoDialog({
  width,
  height,
  onComplete,
  onCancel,
}: AddVideoDialogProps): React.ReactElement {
  const [step, setStep] = useState(0); // 0-5
  const [input, setInput] = useState("");
  const [videoId, setVideoId] = useState<string | null>(null);
  const [url, setUrl] = useState("");
  const [advancedMode, setAdvancedMode] = useState(false);
  const [formats, setFormats] = useState<FormatsData | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selectedVideoItag, setSelectedVideoItag] = useState<number | undefined>(undefined);
  const [selectedAudioItag, setSelectedAudioItag] = useState<number | undefined>(undefined);
  const [startTime, setStartTime] = useState<number | undefined>(undefined);
  const [endTime, setEndTime] = useState<number | undefined>(undefined);
  const [scrollOffset, setScrollOffset] = useState(0);

  const logger = Logger.getInstance();

  // Reset error on input change
  useEffect(() => {
    if (error) setError(null);
  }, [input, error]);

  // Right-click paste
  useMouse({
    onMouseEvent: useCallback(
      (event: TuiMouseEvent) => {
        if (event.type === "click" && event.button === "right") {
          if (step === 0 || step === 3 || step === 4) {
            readClipboard().then((text) => {
              if (text) {
                const firstLine = text.split(/[\r\n]/)[0].trim();
                if (firstLine) setInput((prev) => prev + firstLine);
              }
            });
          }
        } else if (event.type === "scroll") {
          // Scroll format lists
          if (step === 1 || step === 2) {
            if (event.button === "scrollUp") {
              setScrollOffset((prev) => Math.max(0, prev - 3));
            } else if (event.button === "scrollDown") {
              setScrollOffset((prev) => prev + 3);
            }
          }
        }
      },
      [step],
    ),
  });

  const fetchFormats = useCallback(async (vid: string) => {
    setLoading(true);
    setError(null);
    try {
      const port = process.env.MOOMBOX_PORT || "774";
      const res = await fetch(`http://localhost:${port}/api/formats/${vid}`);
      if (!res.ok) {
        throw new Error(`HTTP ${res.status}`);
      }
      const data: FormatsData = await res.json();
      setFormats(data);
      setLoading(false);
    } catch (e: any) {
      logger.warn(`[AddVideoDialog] Failed to fetch formats: ${e.message}`);
      setError("Failed to fetch formats. Proceeding with auto selection.");
      setFormats(null);
      setLoading(false);
      // Auto-advance after showing error
      setTimeout(() => {
        setStep(5); // Skip to confirmation
        setAdvancedMode(false);
      }, 2000);
    }
  }, [logger]);

  const submitJob = useCallback(async () => {
    if (!videoId) return;

    try {
      const db = await Database.getInstance();
      const job = await db.addJob({
        videoId,
        url,
        title: formats?.title || "Manual Add",
        channelName: formats?.channelName || "Manual",
        thumbnailUrl: `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`,
        manuallyAdded: true,
        selectedVideoItag,
        selectedAudioItag,
        startTime,
        endTime,
      });

      if (job) {
        onComplete({ text: `Added ${videoId} to queue`, color: "green" });

        // Build notification fields with advanced options
        const fields: Array<{ name: string; value: string; inline?: boolean }> = [
          { name: "Video ID", value: videoId, inline: true },
        ];
        if (formats?.channelName) {
          fields.push({ name: "Channel", value: formats.channelName, inline: true });
        }

        // Add format selection fields if specified
        if (selectedVideoItag != null) {
          const formatLabel = selectedVideoItag === -1
            ? "None (audio only)"
            : `itag ${selectedVideoItag}`;
          fields.push({ name: "Video Format", value: formatLabel, inline: true });
        }
        if (selectedAudioItag != null) {
          const formatLabel = selectedAudioItag === -1
            ? "None (video only)"
            : `itag ${selectedAudioItag}`;
          fields.push({ name: "Audio Format", value: formatLabel, inline: true });
        }

        // Add time range if specified
        if (startTime != null || endTime != null) {
          const startStr = formatSecondsToTimestamp(startTime || 0);
          const endStr = endTime != null ? formatSecondsToTimestamp(endTime) : "end";
          const duration = endTime != null ? endTime - (startTime || 0) : null;
          const rangeValue = duration != null
            ? `${startStr} - ${endStr} (${formatSecondsToTimestamp(duration)})`
            : `${startStr} - ${endStr}`;
          fields.push({ name: "Time Range", value: rangeValue, inline: false });
        }

        NotificationManager.getInstance().send(
          "Video Added",
          `Manually added: ${formats?.title || videoId}`,
          NotificationType.INFO,
          fields,
          { url, event: "added" },
        );
      } else {
        onComplete({ text: `${videoId} already exists`, color: "yellow" });
      }
    } catch (e: any) {
      logger.error(`[AddVideoDialog] Failed to add job: ${e.stack || e.message}`);
      onComplete({ text: `Error adding ${videoId}`, color: "red" });
    }
  }, [videoId, url, formats, selectedVideoItag, selectedAudioItag, startTime, endTime, onComplete, logger]);

  useInput((char, key) => {
    // Esc: go back or cancel
    if (key.escape) {
      if (step === 0) {
        onCancel();
      } else {
        setStep((s) => Math.max(0, s - 1));
        setError(null);
      }
      return;
    }

    // Step 0: Enter URL/ID
    if (step === 0) {
      if (key.return && !key.shift) {
        // Quick add (auto)
        const vid = extractVideoId(input);
        if (!vid) {
          setError("Invalid video ID or URL");
          return;
        }
        setVideoId(vid);
        setUrl(input.includes("http") ? input : `https://www.youtube.com/watch?v=${vid}`);
        setAdvancedMode(false);
        // Submit immediately
        setTimeout(() => submitJob(), 100);
        return;
      }
      if (key.return && key.shift) {
        // Advanced options
        const vid = extractVideoId(input);
        if (!vid) {
          setError("Invalid video ID or URL");
          return;
        }
        setVideoId(vid);
        setUrl(input.includes("http") ? input : `https://www.youtube.com/watch?v=${vid}`);
        setAdvancedMode(true);
        setStep(1);
        fetchFormats(vid);
        return;
      }
      if (key.backspace || key.delete) {
        setInput((prev) => prev.slice(0, -1));
        return;
      }
      if (key.ctrl && char === "v") {
        readClipboard().then((text) => {
          if (text) {
            const firstLine = text.split(/[\r\n]/)[0].trim();
            if (firstLine) setInput((prev) => prev + firstLine);
          }
        });
        return;
      }
      if (char && !key.ctrl && !key.meta) {
        // Guard: reject mouse escape sequences
        if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
        if (char.length > 1 && char[0] === "[") return;
        setInput((prev) => prev + char);
      }
      return;
    }

    // Loading state: ignore input
    if (loading) return;

    // Step 1: Select video format
    if (step === 1) {
      if (key.return) {
        // Skip (auto)
        setSelectedVideoItag(undefined);
        setStep(2);
        setScrollOffset(0);
        return;
      }
      if (key.upArrow) {
        setScrollOffset((prev) => Math.max(0, prev - 1));
        return;
      }
      if (key.downArrow) {
        setScrollOffset((prev) => prev + 1);
        return;
      }
      if (char === "a" || char === "A") {
        setSelectedVideoItag(undefined);
        setStep(2);
        setScrollOffset(0);
        return;
      }
      if (char === "n" || char === "N") {
        setSelectedVideoItag(-1);
        setStep(2);
        setScrollOffset(0);
        return;
      }
      if (/^\d$/.test(char)) {
        const idx = parseInt(char, 10) - 1;
        if (formats && idx >= 0 && idx < formats.videoFormats.length) {
          setSelectedVideoItag(formats.videoFormats[idx].itag);
          setStep(2);
          setScrollOffset(0);
        }
        return;
      }
      return;
    }

    // Step 2: Select audio format
    if (step === 2) {
      if (key.return) {
        setSelectedAudioItag(undefined);
        setStep(3);
        setScrollOffset(0);
        return;
      }
      if (key.upArrow) {
        setScrollOffset((prev) => Math.max(0, prev - 1));
        return;
      }
      if (key.downArrow) {
        setScrollOffset((prev) => prev + 1);
        return;
      }
      if (char === "a" || char === "A") {
        setSelectedAudioItag(undefined);
        setStep(3);
        setScrollOffset(0);
        setInput("");
        return;
      }
      if (char === "n" || char === "N") {
        setSelectedAudioItag(-1);
        setStep(3);
        setScrollOffset(0);
        setInput("");
        return;
      }
      if (/^\d$/.test(char)) {
        const idx = parseInt(char, 10) - 1;
        if (formats && idx >= 0 && idx < formats.audioFormats.length) {
          setSelectedAudioItag(formats.audioFormats[idx].itag);
          setStep(3);
          setScrollOffset(0);
          setInput("");
        }
        return;
      }
      return;
    }

    // Step 3: Start time
    if (step === 3) {
      if (key.return || key.tab) {
        if (!input.trim()) {
          setStartTime(undefined);
        } else {
          const seconds = parseTimeToSeconds(input);
          if (seconds === null) {
            setError("Invalid time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }
          setStartTime(seconds);
        }
        setStep(4);
        setInput("");
        return;
      }
      if (key.backspace || key.delete) {
        setInput((prev) => prev.slice(0, -1));
        return;
      }
      if (key.ctrl && char === "v") {
        readClipboard().then((text) => {
          if (text) {
            const firstLine = text.split(/[\r\n]/)[0].trim();
            if (firstLine) setInput((prev) => prev + firstLine);
          }
        });
        return;
      }
      if (char && !key.ctrl && !key.meta) {
        if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
        if (char.length > 1 && char[0] === "[") return;
        setInput((prev) => prev + char);
      }
      return;
    }

    // Step 4: End time
    if (step === 4) {
      if (key.return || key.tab) {
        if (!input.trim()) {
          setEndTime(undefined);
        } else {
          const seconds = parseTimeToSeconds(input);
          if (seconds === null) {
            setError("Invalid time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }
          // Validate start < end
          if (startTime !== undefined && seconds <= startTime) {
            setError("End time must be after start time");
            return;
          }
          setEndTime(seconds);
        }
        setStep(5);
        setInput("");
        return;
      }
      if (key.backspace || key.delete) {
        setInput((prev) => prev.slice(0, -1));
        return;
      }
      if (key.ctrl && char === "v") {
        readClipboard().then((text) => {
          if (text) {
            const firstLine = text.split(/[\r\n]/)[0].trim();
            if (firstLine) setInput((prev) => prev + firstLine);
          }
        });
        return;
      }
      if (char && !key.ctrl && !key.meta) {
        if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
        if (char.length > 1 && char[0] === "[") return;
        setInput((prev) => prev + char);
      }
      return;
    }

    // Step 5: Confirmation
    if (step === 5) {
      if (key.return) {
        // Validate: can't have both video and audio set to "none"
        if (selectedVideoItag === -1 && selectedAudioItag === -1) {
          setError("Cannot download video-only and audio-only at the same time");
          return;
        }
        submitJob();
        return;
      }
    }
  });

  // Trigger submission after quick add
  useEffect(() => {
    if (!advancedMode && videoId) {
      submitJob();
    }
  }, [advancedMode, videoId, submitJob]);

  const boxWidth = Math.min(width, 80);
  const boxHeight = Math.min(height - 4, 30);
  const leftPad = Math.max(0, Math.floor((width - boxWidth) / 2));

  return (
    <Box flexDirection="column" width={width} height={height}>
      <Box height={Math.max(0, Math.floor((height - boxHeight) / 2))} />
      <Box paddingLeft={leftPad}>
        <Box
          flexDirection="column"
          width={boxWidth}
          height={boxHeight}
          borderStyle="round"
          borderColor="cyan"
        >
          {/* Title */}
          <Box paddingX={1} justifyContent="space-between">
            <Text color="cyan" bold>
              Add Video
            </Text>
            {advancedMode && (
              <Text color="gray">
                Step {step}/{5}
              </Text>
            )}
          </Box>

          {step === 0 && <StepUrl input={input} error={error} />}
          {step === 1 && (
            <StepVideoFormat
              formats={formats}
              loading={loading}
              error={error}
              scrollOffset={scrollOffset}
              boxHeight={boxHeight}
            />
          )}
          {step === 2 && (
            <StepAudioFormat
              formats={formats}
              loading={loading}
              error={error}
              scrollOffset={scrollOffset}
              boxHeight={boxHeight}
            />
          )}
          {step === 3 && <StepStartTime input={input} error={error} />}
          {step === 4 && <StepEndTime input={input} error={error} />}
          {step === 5 && (
            <StepConfirmation
              videoId={videoId}
              url={url}
              formats={formats}
              selectedVideoItag={selectedVideoItag}
              selectedAudioItag={selectedAudioItag}
              startTime={startTime}
              endTime={endTime}
              error={error}
            />
          )}

          {/* Footer */}
          <Box paddingX={1} height={1} justifyContent="space-between">
            <Text color="gray">{step > 0 ? "Esc: Back" : "Esc: Cancel"}</Text>
            {step === 0 && (
              <Text color="gray">Enter: Quick Add | Shift+Enter: Advanced</Text>
            )}
            {step > 0 && step < 5 && <Text color="gray">Enter: Skip (auto)</Text>}
            {step === 5 && <Text color="cyan" bold>Enter: Submit</Text>}
          </Box>
        </Box>
      </Box>
    </Box>
  );
}

// Step 0: Enter URL/ID
function StepUrl({ input, error }: { input: string; error: string | null }): React.ReactElement {
  return (
    <>
      <Box paddingX={1}>
        <Text color="white" bold>
          Enter URL or Video ID
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="cyan">&gt; URL/ID: </Text>
          <Text>{input}</Text>
          <Text color="cyan">_</Text>
        </Box>
        <Text color="gray" dimColor>
          (Paste with Ctrl+V or right-click)
        </Text>
        {error && (
          <Box marginTop={1}>
            <Text color="red">{error}</Text>
          </Box>
        )}
      </Box>
    </>
  );
}

// Step 1: Select video format
function StepVideoFormat({
  formats,
  loading,
  error,
  scrollOffset,
  boxHeight,
}: {
  formats: FormatsData | null;
  loading: boolean;
  error: string | null;
  scrollOffset: number;
  boxHeight: number;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="white" bold>
          Select Video Format
        </Text>
        <Text color="gray" dimColor>
          Step 2/6
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        {loading && <Text color="cyan">Fetching formats...</Text>}
        {error && <Text color="yellow">{error}</Text>}
        {!loading && formats && (
          <FormatList
            formats={formats.videoFormats}
            type="video"
            bestItags={formats.bestItags}
            scrollOffset={scrollOffset}
            boxHeight={boxHeight}
          />
        )}
        {!loading && !formats && !error && (
          <Text color="gray">No formats available, using auto selection</Text>
        )}
      </Box>
    </>
  );
}

// Step 2: Select audio format
function StepAudioFormat({
  formats,
  loading,
  error,
  scrollOffset,
  boxHeight,
}: {
  formats: FormatsData | null;
  loading: boolean;
  error: string | null;
  scrollOffset: number;
  boxHeight: number;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="white" bold>
          Select Audio Format
        </Text>
        <Text color="gray" dimColor>
          Step 3/6
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        {loading && <Text color="cyan">Fetching formats...</Text>}
        {error && <Text color="yellow">{error}</Text>}
        {!loading && formats && (
          <FormatList
            formats={formats.audioFormats}
            type="audio"
            bestItags={formats.bestItags}
            scrollOffset={scrollOffset}
            boxHeight={boxHeight}
          />
        )}
        {!loading && !formats && !error && (
          <Text color="gray">No formats available, using auto selection</Text>
        )}
      </Box>
    </>
  );
}

// Format list renderer
function FormatList({
  formats,
  type,
  bestItags,
  scrollOffset,
  boxHeight,
}: {
  formats: VideoFormat[] | AudioFormat[];
  type: "video" | "audio";
  bestItags: FormatsData["bestItags"];
  scrollOffset: number;
  boxHeight: number;
}): React.ReactElement {
  const maxVisible = Math.max(5, boxHeight - 10);
  const visibleFormats = formats.slice(scrollOffset, scrollOffset + maxVisible);
  const hasMore = formats.length > scrollOffset + maxVisible;
  const hasPrev = scrollOffset > 0;

  return (
    <>
      <Box marginBottom={1}>
        <Text color="cyan" bold>
          [a] Auto (best quality)
        </Text>
      </Box>
      <Box marginBottom={1}>
        <Text color="gray">
          [n] None ({type === "video" ? "audio only" : "video only"})
        </Text>
      </Box>
      {hasPrev && (
        <Box marginBottom={1}>
          <Text color="gray" dimColor>
            [\u2191 more above]
          </Text>
        </Box>
      )}
      {visibleFormats.map((fmt, idx) => {
        const actualIdx = scrollOffset + idx;
        const displayNum = actualIdx + 1;

        if (type === "video") {
          const vfmt = fmt as VideoFormat;
          const isBestWebm = vfmt.itag === bestItags.bestWebmVideo;
          const isBestMp4 = vfmt.itag === bestItags.bestMp4Video;
          const badge = isBestWebm ? " [Best WEBM]" : isBestMp4 ? " [Best MP4]" : "";
          const container = vfmt.mimeType?.includes("webm") ? "WEBM" : "MP4";
          const fpsStr = vfmt.fps ? `@${vfmt.fps}` : "";

          return (
            <Box key={vfmt.itag} marginBottom={1}>
              <Text color="cyan">[{displayNum}] </Text>
              <Text>
                {vfmt.width}x{vfmt.height}
                {fpsStr} {container} {Math.round(vfmt.bitrate / 1000)}kbps
                {badge && <Text color="green">{badge}</Text>}
              </Text>
            </Box>
          );
        } else {
          const afmt = fmt as AudioFormat;
          const isBestOpus = afmt.itag === bestItags.bestOpusAudio;
          const isBestAac = afmt.itag === bestItags.bestAacAudio;
          const badge = isBestOpus ? " [Best OPUS]" : isBestAac ? " [Best AAC]" : "";
          const container = afmt.mimeType?.includes("webm") ? "OPUS" : "AAC";
          const sampleRate = afmt.audioSampleRate ? ` ${parseInt(afmt.audioSampleRate) / 1000}kHz` : "";

          return (
            <Box key={afmt.itag} marginBottom={1}>
              <Text color="cyan">[{displayNum}] </Text>
              <Text>
                {Math.round(afmt.bitrate / 1000)}kbps{sampleRate} {container}
                {badge && <Text color="green">{badge}</Text>}
              </Text>
            </Box>
          );
        }
      })}
      {hasMore && (
        <Box marginTop={1}>
          <Text color="gray" dimColor>
            [\u2193 more below]
          </Text>
        </Box>
      )}
      <Box marginTop={1}>
        <Text color="gray" dimColor>
          Type number, 'a' for auto, 'n' for none
        </Text>
      </Box>
    </>
  );
}

// Step 3: Start time
function StepStartTime({
  input,
  error,
}: {
  input: string;
  error: string | null;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="white" bold>
          Start Time (Optional)
        </Text>
        <Text color="gray" dimColor>
          Step 4/6
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="cyan">&gt; Time: </Text>
          <Text>{input}</Text>
          <Text color="cyan">_</Text>
        </Box>
        <Text color="gray" dimColor>
          Format: HH:MM:SS, MM:SS, or seconds (blank = beginning)
        </Text>
        {error && (
          <Box marginTop={1}>
            <Text color="red">{error}</Text>
          </Box>
        )}
      </Box>
    </>
  );
}

// Step 4: End time
function StepEndTime({
  input,
  error,
}: {
  input: string;
  error: string | null;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="white" bold>
          End Time (Optional)
        </Text>
        <Text color="gray" dimColor>
          Step 5/6
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="cyan">&gt; Time: </Text>
          <Text>{input}</Text>
          <Text color="cyan">_</Text>
        </Box>
        <Text color="gray" dimColor>
          Format: HH:MM:SS, MM:SS, or seconds (blank = end of video)
        </Text>
        {error && (
          <Box marginTop={1}>
            <Text color="red">{error}</Text>
          </Box>
        )}
      </Box>
    </>
  );
}

// Step 5: Confirmation
function StepConfirmation({
  videoId,
  url,
  formats,
  selectedVideoItag,
  selectedAudioItag,
  startTime,
  endTime,
  error,
}: {
  videoId: string | null;
  url: string;
  formats: FormatsData | null;
  selectedVideoItag: number | undefined;
  selectedAudioItag: number | undefined;
  startTime: number | undefined;
  endTime: number | undefined;
  error: string | null;
}): React.ReactElement {
  const videoFmt = formats?.videoFormats.find((f) => f.itag === selectedVideoItag);
  const audioFmt = formats?.audioFormats.find((f) => f.itag === selectedAudioItag);

  const videoLabel =
    selectedVideoItag === -1
      ? "None (audio only)"
      : selectedVideoItag === undefined
        ? "Auto (best quality)"
        : videoFmt
          ? `${videoFmt.width}x${videoFmt.height}${videoFmt.fps ? `@${videoFmt.fps}` : ""} ${videoFmt.mimeType?.includes("webm") ? "WEBM" : "MP4"}`
          : `itag ${selectedVideoItag}`;

  const audioLabel =
    selectedAudioItag === -1
      ? "None (video only)"
      : selectedAudioItag === undefined
        ? "Auto (best quality)"
        : audioFmt
          ? `${Math.round(audioFmt.bitrate / 1000)}kbps ${audioFmt.mimeType?.includes("webm") ? "OPUS" : "AAC"}`
          : `itag ${selectedAudioItag}`;

  const startLabel = startTime !== undefined ? formatSecondsToTimestamp(startTime) : "0:00 (beginning)";
  const endLabel = endTime !== undefined ? formatSecondsToTimestamp(endTime) : "(end of video)";

  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="white" bold>
          Confirmation
        </Text>
        <Text color="gray" dimColor>
          Step 6/6
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="gray">Video ID:     </Text>
          <Text>{videoId}</Text>
        </Box>
        <Box marginBottom={1}>
          <Text color="gray">URL:          </Text>
          <Text>{url.length > 50 ? url.slice(0, 47) + "..." : url}</Text>
        </Box>
        {formats && (
          <Box marginBottom={1}>
            <Text color="gray">Title:        </Text>
            <Text>{formats.title.length > 50 ? formats.title.slice(0, 47) + "..." : formats.title}</Text>
          </Box>
        )}
        <Box marginBottom={1} />
        <Box marginBottom={1}>
          <Text color="gray">Video Format: </Text>
          <Text>{videoLabel}</Text>
        </Box>
        <Box marginBottom={1}>
          <Text color="gray">Audio Format: </Text>
          <Text>{audioLabel}</Text>
        </Box>
        <Box marginBottom={1} />
        <Box marginBottom={1}>
          <Text color="gray">Start Time:   </Text>
          <Text>{startLabel}</Text>
        </Box>
        <Box marginBottom={1}>
          <Text color="gray">End Time:     </Text>
          <Text>{endLabel}</Text>
        </Box>
        {error && (
          <Box marginTop={1}>
            <Text color="red">{error}</Text>
          </Box>
        )}
      </Box>
    </>
  );
}
