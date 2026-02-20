/**
 * Multi-step wizard dialog for adding videos with advanced options.
 *
 * Flow (URL mode):
 * 1. Enter URL/ID (Tab = cycle mode: URL → Import → URL, Enter = proceed)
 *    - UI turns lilac/magenta when advanced mode is enabled
 * 2. (Advanced) Select video format (numbered list, 'a' = auto, 'n' = none)
 * 3. (Advanced) Select audio format
 * 4. (Advanced) Timestamps - Start and End time combined (Tab = switch field)
 * 5. (Advanced) Confirmation
 *
 * Flow (Import mode):
 * 1. Enter file path to .zip (Tab = cycle mode, Enter = validate + proceed)
 * 2. Optional metadata (title + channel name)
 * 3. Uploading (reads file, POSTs to /api/import)
 */

import React, { useState, useCallback, useEffect } from "react";
import { Box, Text, useInput } from "ink";
import fs from "fs-extra";
import path from "path";
import { Database } from "../../core/database.js";
import { Logger } from "../../core/logger.js";
import { NotificationManager, NotificationType } from "../../core/notifications.js";
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

type InputMode = "url" | "import";

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

  // Import mode state
  const [inputMode, setInputMode] = useState<InputMode>("url");
  const [importPath, setImportPath] = useState("");
  const [importTitle, setImportTitle] = useState("");
  const [importChannel, setImportChannel] = useState("");
  const [importStep, setImportStep] = useState(0); // 0=path, 1=metadata, 2=uploading
  const [importMetaFocus, setImportMetaFocus] = useState<"title" | "channel">("title");

  const logger = Logger.getInstance();

  // Reset error on input change
  useEffect(() => {
    if (error) setError(null);
  }, [input, importPath, error]);

  // Right-click paste
  useMouse({
    onMouseEvent: useCallback(
      (event: TuiMouseEvent) => {
        if (event.type === "click" && event.button === "right") {
          if (inputMode === "import") {
            readClipboard().then((text) => {
              if (text) {
                const firstLine = text.split(/[\r\n]/)[0].trim();
                if (firstLine) {
                  if (importStep === 0) {
                    setImportPath((prev) => prev + firstLine);
                  } else if (importStep === 1) {
                    if (importMetaFocus === "title") {
                      setImportTitle((prev) => prev + firstLine);
                    } else {
                      setImportChannel((prev) => prev + firstLine);
                    }
                  }
                }
              }
            });
          } else if (step === 0) {
            readClipboard().then((text) => {
              if (text) {
                const firstLine = text.split(/[\r\n]/)[0].trim();
                if (firstLine) setInput((prev) => prev + firstLine);
              }
            });
          } else if (step === 3) {
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
      [step, inputMode, importStep, importMetaFocus, timeInputFocus],
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
      }, 2_000);
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

  const submitImport = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const fileData = await fs.readFile(importPath);
      const port = process.env.MOOMBOX_PORT || "774";
      const headers: Record<string, string> = {
        "Content-Type": "application/octet-stream",
        "Origin": `http://127.0.0.1:${port}`,
      };
      if (importTitle.trim()) {
        headers["X-Import-Title"] = importTitle.trim();
      }
      if (importChannel.trim()) {
        headers["X-Import-Channel"] = importChannel.trim();
      }

      const res = await fetch(`http://127.0.0.1:${port}/api/import`, {
        method: "POST",
        headers,
        body: fileData,
      });

      if (res.ok) {
        const data = await res.json();
        const title = data?.title || importTitle.trim() || path.basename(importPath, ".zip");
        onComplete({ text: `Imported: ${title}`, color: "green" });
      } else {
        const data = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
        setError(data.error || `Import failed (HTTP ${res.status})`);
        setLoading(false);
        setImportStep(1); // Go back to metadata step
      }
    } catch (e: unknown) {
      const msg = getErrorMessage(e);
      logger.error(`[AddVideoDialog] Import failed: ${msg}`);
      setError(`Import failed: ${msg}`);
      setLoading(false);
      setImportStep(1); // Go back to metadata step
    }
  }, [importPath, importTitle, importChannel, onComplete, logger]);

  useInput((char, key) => {
    // Esc: go back or cancel
    if (key.escape) {
      if (inputMode === "import") {
        if (importStep > 0 && !loading) {
          setImportStep((s) => s - 1);
          setError(null);
        } else if (importStep === 0) {
          // Switch back to URL mode
          setInputMode("url");
          setError(null);
        }
        return;
      }
      if (step === 0) {
        onCancel();
      } else {
        setStep((s) => Math.max(0, s - 1));
        setError(null);
      }
      return;
    }

    // Import mode input handling
    if (inputMode === "import") {
      if (loading) return;

      // Import step 0: File path input
      if (importStep === 0) {
        // Tab: switch back to URL mode
        if (key.tab) {
          setInputMode("url");
          setError(null);
          return;
        }

        if (key.return) {
          const trimmedPath = importPath.trim();
          if (!trimmedPath) {
            setError("Please enter a file path");
            return;
          }
          // Validate file exists and is .zip
          const resolved = path.resolve(trimmedPath);
          if (!resolved.toLowerCase().endsWith(".zip")) {
            setError("File must be a .zip archive");
            return;
          }
          fs.pathExists(resolved).then((exists) => {
            if (!exists) {
              setError("File not found");
            } else {
              setImportPath(resolved);
              setImportStep(1);
              setError(null);
            }
          });
          return;
        }
        if (key.backspace || key.delete) {
          setImportPath((prev) => prev.slice(0, -1));
          return;
        }
        if (key.ctrl && char === "v") {
          readClipboard().then((text) => {
            if (text) {
              const firstLine = text.split(/[\r\n]/)[0].trim();
              if (firstLine) setImportPath((prev) => prev + firstLine);
            }
          });
          return;
        }
        if (char && !key.ctrl && !key.meta) {
          if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
          if (char.length > 1 && char[0] === "[") return;
          setImportPath((prev) => prev + char);
        }
        return;
      }

      // Import step 1: Metadata
      if (importStep === 1) {
        if (key.tab) {
          setImportMetaFocus((prev) => prev === "title" ? "channel" : "title");
          return;
        }
        if (key.return) {
          setImportStep(2);
          // Trigger upload
          submitImport();
          return;
        }
        if (key.backspace || key.delete) {
          if (importMetaFocus === "title") {
            setImportTitle((prev) => prev.slice(0, -1));
          } else {
            setImportChannel((prev) => prev.slice(0, -1));
          }
          return;
        }
        if (key.ctrl && char === "v") {
          readClipboard().then((text) => {
            if (text) {
              const firstLine = text.split(/[\r\n]/)[0].trim();
              if (firstLine) {
                if (importMetaFocus === "title") {
                  setImportTitle((prev) => prev + firstLine);
                } else {
                  setImportChannel((prev) => prev + firstLine);
                }
              }
            }
          });
          return;
        }
        if (char && !key.ctrl && !key.meta) {
          if (char.charCodeAt(0) === 0x1b || char.charCodeAt(0) === 0x9b) return;
          if (char.length > 1 && char[0] === "[") return;
          if (importMetaFocus === "title") {
            setImportTitle((prev) => prev + char);
          } else {
            setImportChannel((prev) => prev + char);
          }
        }
        return;
      }

      // Import step 2: Uploading — no input accepted
      return;
    }

    // URL mode: Step 0: Enter URL/ID
    if (step === 0) {
      // Tab: cycle mode (URL → Advanced URL → Import → URL)
      if (key.tab) {
        if (!advancedEnabled) {
          setAdvancedEnabled(true);
        } else {
          setAdvancedEnabled(false);
          setInputMode("import");
          setError(null);
        }
        return;
      }

      // Ctrl+I: Toggle advanced options (may not fire in all terminals — same as Tab)
      if (key.ctrl && char === "i") {
        setAdvancedEnabled(prev => !prev);
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
        }
        // Non-advanced: useEffect fires submitJob() when videoId is set
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
      // 'a' key toggles advanced when not typing a URL
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
          setError("Cannot select both video-only and audio-only");
          return;
        }
        // Validate: end time must be after start time
        if (startTime != null && endTime != null && endTime <= startTime) {
          setError("End time must be after start time");
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

  // Border color: green for import, magenta for advanced, cyan for normal
  const isImport = inputMode === "import";
  const borderColor = isImport ? "green" : (advancedMode || advancedEnabled ? "magenta" : "cyan");

  // Title
  let titleText: string;
  let stepText: string | null = null;
  if (isImport) {
    titleText = "Import Archive";
    if (importStep > 0) stepText = `Step ${importStep + 1}/3`;
  } else {
    titleText = `Add Video${advancedMode || advancedEnabled ? " (Advanced Mode)" : ""}`;
    if (advancedMode && step > 0) stepText = `Step ${step}/${4}`;
  }

  return (
    <Box flexDirection="column" width={width} height={height}>
      <Box height={Math.max(0, Math.floor((height - boxHeight) / 2))} />
      <Box paddingLeft={leftPad}>
        <Box
          flexDirection="column"
          width={boxWidth}
          height={boxHeight}
          borderStyle="round"
          borderColor={borderColor}
        >
          {/* Title */}
          <Box paddingX={1} justifyContent="space-between">
            <Text color={borderColor} bold>
              {titleText}
            </Text>
            {stepText && (
              <Text color={borderColor}>
                {stepText}
              </Text>
            )}
          </Box>

          {/* Import mode steps */}
          {isImport && importStep === 0 && (
            <StepImportPath importPath={importPath} error={error} />
          )}
          {isImport && importStep === 1 && (
            <StepImportMeta
              importPath={importPath}
              importTitle={importTitle}
              importChannel={importChannel}
              importMetaFocus={importMetaFocus}
              error={error}
            />
          )}
          {isImport && importStep === 2 && (
            <StepImportUploading error={error} />
          )}

          {/* URL mode steps */}
          {!isImport && step === 0 && <StepUrl input={input} error={error} advancedEnabled={advancedEnabled} />}
          {!isImport && step === 1 && (
            <StepVideoFormat
              formats={formats}
              loading={loading}
              error={error}
              scrollOffset={scrollOffset}
              boxHeight={boxHeight}
            />
          )}
          {!isImport && step === 2 && (
            <StepAudioFormat
              formats={formats}
              loading={loading}
              error={error}
              scrollOffset={scrollOffset}
              boxHeight={boxHeight}
            />
          )}
          {!isImport && step === 3 && (
            <StepTimestamps
              startTimeInput={startTimeInput}
              endTimeInput={endTimeInput}
              timeInputFocus={timeInputFocus}
              error={error}
            />
          )}
          {!isImport && step === 4 && (
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
            {isImport ? (
              <>
                <Text color="gray">{importStep > 0 ? "Esc: Back" : "Esc: URL mode"}</Text>
                {importStep === 0 && (
                  <Text color="gray">Tab: URL mode | Enter: Continue</Text>
                )}
                {importStep === 1 && (
                  <Text color="gray">Tab: Switch Field | Enter: Import</Text>
                )}
                {importStep === 2 && (
                  <Text color="green">Uploading...</Text>
                )}
              </>
            ) : (
              <>
                <Text color="gray">{step > 0 ? "Esc: Back" : "Esc: Cancel"}</Text>
                {step === 0 && (
                  <Text color="gray">Tab: Cycle mode | Enter: Continue</Text>
                )}
                {step === 3 && (
                  <Text color="gray">Tab: Switch Field | Enter: Continue</Text>
                )}
                {step > 0 && step < 3 && <Text color="gray">Enter: Skip (auto)</Text>}
                {step === 4 && <Text color="cyan" bold>Enter: Submit</Text>}
              </>
            )}
          </Box>
        </Box>
      </Box>
    </Box>
  );
}

// Import Step 0: File path input
function StepImportPath({
  importPath,
  error,
}: {
  importPath: string;
  error: string | null;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1}>
        <Text color="white" bold>
          Import ZIP Archive
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="green">&gt; File path: </Text>
          <Text>{importPath}</Text>
          <Text color="green">_</Text>
        </Box>
        <Box marginBottom={1} marginTop={1}>
          <Text color="gray" dimColor>
            Enter path to a .zip file containing video (+ optional chat JSON)
          </Text>
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

// Import Step 1: Optional metadata
function StepImportMeta({
  importPath,
  importTitle,
  importChannel,
  importMetaFocus,
  error,
}: {
  importPath: string;
  importTitle: string;
  importChannel: string;
  importMetaFocus: "title" | "channel";
  error: string | null;
}): React.ReactElement {
  const basename = path.basename(importPath);
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="green" bold>
          Metadata (Optional)
        </Text>
        <Text color="green" dimColor>
          Step 2/3
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        <Box marginBottom={1}>
          <Text color="gray">File: </Text>
          <Text>{basename.length > 60 ? basename.slice(0, 57) + "..." : basename}</Text>
        </Box>
        <Box marginBottom={1}>
          <Text color={importMetaFocus === "title" ? "green" : "gray"}>
            {importMetaFocus === "title" ? "> " : "  "}Title:
          </Text>
          <Text>{importTitle}</Text>
          {importMetaFocus === "title" && <Text color="green">_</Text>}
        </Box>
        <Box marginBottom={1}>
          <Text color={importMetaFocus === "channel" ? "green" : "gray"}>
            {importMetaFocus === "channel" ? "> " : "  "}Channel:
          </Text>
          <Text>{importChannel}</Text>
          {importMetaFocus === "channel" && <Text color="green">_</Text>}
        </Box>
        <Box marginTop={1}>
          <Text color="gray" dimColor>
            Leave blank to auto-detect from archive contents
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

// Import Step 2: Uploading
function StepImportUploading({
  error,
}: {
  error: string | null;
}): React.ReactElement {
  return (
    <>
      <Box paddingX={1} flexDirection="column">
        <Text color="green" bold>
          Importing
        </Text>
        <Text color="green" dimColor>
          Step 3/3
        </Text>
      </Box>
      <Box paddingX={1} height={1}>
        <Text color="gray">{"\u2500".repeat(74)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        {!error && (
          <Box marginTop={1}>
            <Text color="green">Reading file and importing...</Text>
          </Box>
        )}
        {error && (
          <Box marginTop={1}>
            <Text color="red">{error}</Text>
          </Box>
        )}
      </Box>
    </>
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
            [{advancedEnabled ? "\u2713" : " "}] Advanced Options
          </Text>
          <Text color="gray" dimColor> (Tab to cycle)</Text>
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
            [{"\u2191"} more above]
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
            [{"\u2193"} more below]
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
