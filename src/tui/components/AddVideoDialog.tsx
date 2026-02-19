/**
 * Multi-step wizard dialog for adding videos with advanced options.
 *
 * Flow:
 * 1. Enter URL/ID (Tab = toggle advanced mode, Enter = proceed)
 *    - UI turns lilac/magenta when advanced mode is enabled
 * 2. (Advanced) Select video format (numbered list, 'a' = auto, 'n' = none)
 * 3. (Advanced) Select audio format
 * 4. (Advanced) Timestamps - Start and End time combined (Tab = switch field)
 * 5. (Advanced) Confirmation
 */

import React, { useState, useCallback, useEffect } from "react";
import { Box, Text, useInput } from "ink";
import { Database } from "../../core/database.js";
import { Logger } from "../../core/logger.js";
import { NotificationManager, NotificationType } from "../../core/notifications.js";
import { extractVideoId } from "../../utils/youtube.js";
import { extractMediaId } from "../../utils/mediaId.js";
import { getErrorMessage } from "../../types/errors.js";
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
  const [step, setStep] = useState(0); // 0-4 (now 5 steps instead of 6)
  const [input, setInput] = useState("");
  const [videoId, setVideoId] = useState<string | null>(null);
  const [url, setUrl] = useState("");
  const [platform, setPlatform] = useState<"youtube" | "twitch">("youtube");
  const [advancedMode, setAdvancedMode] = useState(false);
  const [advancedEnabled, setAdvancedEnabled] = useState(false); // Checkbox state in step 0
  const [formats, setFormats] = useState<FormatsData | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selectedVideoItag, setSelectedVideoItag] = useState<number | undefined>(undefined);
  const [selectedAudioItag, setSelectedAudioItag] = useState<number | undefined>(undefined);
  const [startTime, setStartTime] = useState<number | undefined>(undefined);
  const [endTime, setEndTime] = useState<number | undefined>(undefined);
  const [scrollOffset, setScrollOffset] = useState(0);
  const [timeInputFocus, setTimeInputFocus] = useState<"start" | "end">("start"); // For combined time step
  const [startTimeInput, setStartTimeInput] = useState("");
  const [endTimeInput, setEndTimeInput] = useState("");

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
    } catch (e: unknown) {
      logger.warn(`[AddVideoDialog] Failed to fetch formats: ${getErrorMessage(e)}`);
      setError("Failed to fetch formats. Proceeding with auto selection.");
      setFormats(null);
      setLoading(false);
      // Auto-advance after showing error
      setTimeout(() => {
        setStep(4); // Skip to confirmation
        setAdvancedMode(false);
      }, 2000);
    }
  }, [logger]);

  const submitJob = useCallback(async () => {
    if (!videoId) return;

    try {
      const isTwitch = platform === "twitch";

      if (isTwitch) {
        // Route Twitch jobs through the REST API for proper metadata resolution
        const port = process.env.MOOMBOX_PORT || "774";
        const twitchType = videoId.startsWith("tw_v") ? "vod" : "channel";
        const twitchId = twitchType === "vod" ? videoId.replace("tw_v", "") : videoId;
        const res = await fetch(`http://localhost:${port}/api/jobs`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ platform: "twitch", videoId: twitchId, twitchType }),
        });
        if (res.ok) {
          onComplete({ text: `Added Twitch ${twitchType} to queue`, color: "green" });
        } else if (res.status === 409) {
          onComplete({ text: `Job already exists`, color: "yellow" });
        } else {
          const data = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
          onComplete({ text: data.error || `Failed to add job`, color: "red" });
        }
        return;
      }

      // YouTube path: direct DB access (already has metadata from format fetch)
      const db = await Database.getInstance();
      const thumbnailUrl = `https://i.ytimg.com/vi/${videoId}/maxresdefault.jpg`;
      const job = await db.addJob({
        videoId,
        url,
        title: formats?.title || "Manual Add",
        channelName: formats?.channelName || "Manual",
        thumbnailUrl,
        manuallyAdded: true,
        selectedVideoItag, selectedAudioItag, startTime, endTime,
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
    } catch (e: unknown) {
      logger.error(`[AddVideoDialog] Failed to add job: ${e instanceof Error ? (e.stack || e.message) : String(e)}`);
      onComplete({ text: `Error adding ${videoId}`, color: "red" });
    }
  }, [videoId, url, platform, formats, selectedVideoItag, selectedAudioItag, startTime, endTime, onComplete, logger]);

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
      // Tab: toggle advanced mode directly
      if (key.tab) {
        setAdvancedEnabled((prev) => !prev);
        return;
      }

      if (key.return) {
        const target = extractMediaId(input);
        if (!target) {
          setError("Invalid video ID or URL");
          return;
        }

        if (target.platform === "twitch") {
          // Twitch: no advanced options
          const twitchTarget = target.target;
          let twitchVideoId: string;
          let twitchUrl: string;
          if (twitchTarget.type === "vod") {
            twitchVideoId = `tw_v${twitchTarget.vodId}`;
            twitchUrl = `https://www.twitch.tv/videos/${twitchTarget.vodId}`;
          } else if (twitchTarget.type === "channel") {
            twitchVideoId = twitchTarget.login; // will be resolved by the API
            twitchUrl = `https://www.twitch.tv/${twitchTarget.login}`;
          } else {
            setError("Twitch clips are not supported");
            return;
          }
          setVideoId(twitchVideoId);
          setUrl(twitchUrl);
          setPlatform("twitch");
          setAdvancedMode(false);
          setAdvancedEnabled(false);
          return; // submitJob will fire via the useEffect
        }

        // YouTube path
        const vid = target.videoId;
        setVideoId(vid);
        setPlatform("youtube");
        setUrl(input.includes("http") ? input : `https://www.youtube.com/watch?v=${vid}`);

        if (advancedEnabled) {
          // Advanced path: fetch formats and show wizard
          setAdvancedMode(true);
          setStep(1);
          fetchFormats(vid);
        } else {
          // Quick add: submit immediately with auto settings
          setAdvancedMode(false);
          setTimeout(() => submitJob(), 100);
        }
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

    // Step 3: Start and End time (combined)
    if (step === 3) {
      // Tab: switch between start and end input fields
      if (key.tab) {
        setTimeInputFocus((prev) => prev === "start" ? "end" : "start");
        return;
      }

      // Enter: validate and proceed to confirmation
      if (key.return) {
        // Validate start time
        if (startTimeInput.trim()) {
          const startSeconds = parseTimeToSeconds(startTimeInput);
          if (startSeconds === null) {
            setError("Invalid start time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }
          setStartTime(startSeconds);
        } else {
          setStartTime(undefined);
        }

        // Validate end time
        if (endTimeInput.trim()) {
          const endSeconds = parseTimeToSeconds(endTimeInput);
          if (endSeconds === null) {
            setError("Invalid end time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }
          // Validate start < end
          const finalStartTime = startTimeInput.trim() ? parseTimeToSeconds(startTimeInput) : 0;
          if (finalStartTime !== null && endSeconds <= finalStartTime) {
            setError("End time must be after start time");
            return;
          }
          setEndTime(endSeconds);
        } else {
          setEndTime(undefined);
        }

        setStep(4); // Go to confirmation (now step 4 instead of 5)
        return;
      }

      if (key.backspace || key.delete) {
        if (timeInputFocus === "start") {
          setStartTimeInput((prev) => prev.slice(0, -1));
        } else {
          setEndTimeInput((prev) => prev.slice(0, -1));
        }
        return;
      }

      if (key.ctrl && char === "v") {
        readClipboard().then((text) => {
          if (text) {
            const firstLine = text.split(/[\r\n]/)[0].trim();
            if (firstLine) {
              if (timeInputFocus === "start") {
                setStartTimeInput((prev) => prev + firstLine);
              } else {
                setEndTimeInput((prev) => prev + firstLine);
              }
            }
          }
        });
        return;
      }

      if (char && !key.ctrl && !key.meta) {
        if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
        if (char.length > 1 && char[0] === "[") return;
        if (timeInputFocus === "start") {
          setStartTimeInput((prev) => prev + char);
        } else {
          setEndTimeInput((prev) => prev + char);
        }
      }
      return;
    }

    // Step 4: Confirmation (was step 5)
    if (step === 4) {
      if (key.return) {
        // Validate: can't have both video and audio set to "none"
        if (selectedVideoItag === -1 && selectedAudioItag === -1) {
          setError("❌ Cannot select both video-only and audio-only");
          return;
        }
        // Validate: end time must be after start time
        if (startTime != null && endTime != null && endTime <= startTime) {
          setError("❌ End time must be after start time");
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
          borderColor={advancedMode || advancedEnabled ? "magenta" : "cyan"}
        >
          {/* Title */}
          <Box paddingX={1} justifyContent="space-between">
            <Text color={advancedMode || advancedEnabled ? "magenta" : "cyan"} bold>
              Add Video {(advancedMode || advancedEnabled) ? "(Advanced Mode)" : ""}
            </Text>
            {advancedMode && step > 0 && (
              <Text color="magenta">
                Step {step}/{4}
              </Text>
            )}
          </Box>

          {step === 0 && <StepUrl input={input} error={error} advancedEnabled={advancedEnabled} />}
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
          {step === 3 && (
            <StepTimestamps
              startTimeInput={startTimeInput}
              endTimeInput={endTimeInput}
              timeInputFocus={timeInputFocus}
              error={error}
            />
          )}
          {step === 4 && (
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
              <Text color="gray">Tab: Toggle Advanced | Enter: Continue</Text>
            )}
            {step === 3 && (
              <Text color="gray">Tab: Switch Field | Enter: Continue</Text>
            )}
            {step > 0 && step < 3 && <Text color="gray">Enter: Skip (auto)</Text>}
            {step === 4 && <Text color="cyan" bold>Enter: Submit</Text>}
          </Box>
        </Box>
      </Box>
    </Box>
  );
}

// Step 0: Enter URL/ID
function StepUrl({
  input,
  error,
  advancedEnabled
}: {
  input: string;
  error: string | null;
  advancedEnabled: boolean;
}): React.ReactElement {
  const accentColor = advancedEnabled ? "magenta" : "cyan";
  return (
    <>
      <Box paddingX={1}>
        <Text color="white" bold>
          Enter YouTube/Twitch URL or Video ID
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color={accentColor}>&gt; URL/ID: </Text>
          <Text>{input}</Text>
          <Text color={accentColor}>_</Text>
        </Box>
        <Box marginBottom={1} marginTop={1}>
          <Text color={accentColor}>
            [{advancedEnabled ? "✓" : " "}] Advanced Options
          </Text>
          <Text color="gray" dimColor> (press Tab to toggle)</Text>
        </Box>
        <Box marginBottom={1}>
          {advancedEnabled ? (
            <Box flexDirection="column">
              <Text color="magenta" dimColor>  → Format selection (video/audio)</Text>
              <Text color="magenta" dimColor>  → Timestamp selection (start/end)</Text>
            </Box>
          ) : (
            <Text color="gray" dimColor>Quick add with auto settings</Text>
          )}
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
        <Text color="magenta" bold>
          Select Video Format
        </Text>
        <Text color="magenta" dimColor>
          Step 1/4
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
        <Text color="magenta" bold>
          Select Audio Format
        </Text>
        <Text color="magenta" dimColor>
          Step 2/4
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
        <Text color="magenta" bold>
          [a] Auto (best quality)
        </Text>
      </Box>
      <Box marginBottom={1}>
        <Text color="magenta">
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
              <Text color="magenta">[{displayNum}] </Text>
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
              <Text color="magenta">[{displayNum}] </Text>
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

// Step 3: Start and End time (combined)
function StepTimestamps({
  startTimeInput,
  endTimeInput,
  timeInputFocus,
  error,
}: {
  startTimeInput: string;
  endTimeInput: string;
  timeInputFocus: "start" | "end";
  error: string | null;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="magenta" bold>
          Timestamps (Optional)
        </Text>
        <Text color="magenta" dimColor>
          Step 3/4
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color={timeInputFocus === "start" ? "magenta" : "gray"}>
            {timeInputFocus === "start" ? ">" : " "} Start:
          </Text>
          <Text>{startTimeInput}</Text>
          {timeInputFocus === "start" && <Text color="magenta">_</Text>}
        </Box>
        <Box marginBottom={1}>
          <Text color={timeInputFocus === "end" ? "magenta" : "gray"}>
            {timeInputFocus === "end" ? ">" : " "} End:
          </Text>
          <Text>{endTimeInput}</Text>
          {timeInputFocus === "end" && <Text color="magenta">_</Text>}
        </Box>
        <Box marginTop={1}>
          <Text color="gray" dimColor>
            Format: HH:MM:SS, MM:SS, or seconds (blank = default)
          </Text>
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

// Step 4: Confirmation (was step 5)
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
        <Text color="magenta" bold>
          Confirmation
        </Text>
        <Text color="magenta" dimColor>
          Step 4/4
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
