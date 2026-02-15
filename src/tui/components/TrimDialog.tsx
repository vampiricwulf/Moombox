/**
 * Trim dialog for creating and deleting trimmed versions of videos.
 *
 * Modes:
 * - CREATE (cyan): Enter start time → end time → confirmation
 * - DELETE (magenta): Arrow keys to navigate → Enter to delete → confirm
 *
 * M toggles between modes, Esc goes back/cancels
 */

import React, { useState, useCallback } from "react";
import { Box, Text, useInput } from "ink";
import { parseTimeToSeconds } from "../../core/worker/timeUtils.js";
import type { Job } from "../../types/jobs.js";

interface TrimDialogProps {
  job: Job;
  width: number;
  height: number;
  onComplete: (message: { text: string; color: string }) => void;
  onCancel: () => void;
}

type Mode = "create" | "delete";

export function TrimDialog({
  job,
  width,
  height,
  onComplete,
  onCancel,
}: TrimDialogProps): React.ReactElement {
  // Extract primitives from job to avoid dependency issues
  const jobId = job.id;
  const maxDuration = job.lengthSeconds;
  const trims = job.trims || [];

  const [mode, setMode] = useState<Mode>("create");
  const [step, setStep] = useState(0); // CREATE: 0=times (both start/end), 1=confirm
  const [startInput, setStartInput] = useState("");
  const [endInput, setEndInput] = useState("");
  const [timeInputFocus, setTimeInputFocus] = useState<"start" | "end">("start"); // Which time field is focused
  const [deleteConfirmId, setDeleteConfirmId] = useState<string | null>(null);
  const [selectedTrimIndex, setSelectedTrimIndex] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const borderColor = mode === "create" ? "cyan" : "magenta";

  // Keyboard handler
  useInput(
    useCallback(
      (input, key) => {
        if (loading) return; // Block input during loading

        // Tab: Switch between start/end time fields (CREATE step 0 only)
        if (key.tab && mode === "create" && step === 0) {
          setTimeInputFocus((prev) => (prev === "start" ? "end" : "start"));
          setError(null);
          return;
        }

        // M: Switch to DELETE (management) mode
        if (input === "m" || input === "M") {
          setMode("delete");
          setStep(0);
          setError(null);
          setDeleteConfirmId(null);
          setSelectedTrimIndex(0);
          setStartInput("");
          setEndInput("");
          setTimeInputFocus("start");
          return;
        }

        // Esc: Back/cancel
        if (key.escape) {
          if (mode === "create") {
            if (step > 0) {
              setStep(step - 1);
              setError(null);
            } else {
              onCancel();
            }
          } else {
            // DELETE mode: cancel confirmation or switch back to CREATE
            if (deleteConfirmId) {
              setDeleteConfirmId(null);
            } else {
              setMode("create");
            }
          }
          return;
        }

        if (mode === "create") {
          handleCreateInput(input, key);
        } else {
          handleDeleteInput(input, key);
        }
      },
      [mode, step, loading, startInput, endInput, timeInputFocus, deleteConfirmId, trims, selectedTrimIndex, jobId, maxDuration, onComplete, onCancel],
    ),
  );

  const handleCreateInput = useCallback(
    (input: string, key: any) => {
      if (step === 0) {
        // Step 0: Both start and end time inputs (Tab to switch focus)
        if (key.return) {
          const startParsed = parseTimeToSeconds(startInput);
          const endParsed = parseTimeToSeconds(endInput);

          if (startParsed === null) {
            setError("Invalid start time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }

          if (endParsed === null) {
            setError("Invalid end time format (use HH:MM:SS, MM:SS, or seconds)");
            return;
          }

          // Validate range
          const validationError = validateTrimRange(
            startParsed,
            endParsed,
            maxDuration,
          );
          if (validationError) {
            setError(validationError);
            return;
          }

          setStep(1);
          setError(null);
        } else if (key.backspace || key.delete) {
          if (timeInputFocus === "start") {
            setStartInput((prev) => prev.slice(0, -1));
          } else {
            setEndInput((prev) => prev.slice(0, -1));
          }
        } else if (input && /^[0-9:.]$/.test(input)) {
          if (timeInputFocus === "start") {
            setStartInput((prev) => prev + input);
          } else {
            setEndInput((prev) => prev + input);
          }
        }
      } else if (step === 1) {
        // Step 1: Confirmation
        if (key.return) {
          const start = parseTimeToSeconds(startInput)!;
          const end = parseTimeToSeconds(endInput)!;

          setLoading(true);

          // API call
          const port = process.env.MOOMBOX_PORT || "774";
          fetch(`http://127.0.0.1:${port}/api/jobs/${jobId}/trims`, {
            method: "POST",
            headers: {
              "Content-Type": "application/json",
              "Origin": `http://127.0.0.1:${port}`,
            },
            body: JSON.stringify({ startTime: start, endTime: end }),
          })
            .then((res) => {
              if (!res.ok) {
                return res.json().then((data) => {
                  throw new Error(data.error || `HTTP ${res.status}`);
                }).catch(() => {
                  throw new Error(`HTTP ${res.status}: ${res.statusText}`);
                });
              }
              return res.json();
            })
            .then(() => {
              onComplete({
                text: `Trim created: ${formatTime(start)} - ${formatTime(end)}`,
                color: "green",
              });
            })
            .catch((err) => {
              setError(err.message || "Failed to create trim");
              setLoading(false);
            });
        }
      }
    },
    [step, startInput, endInput, timeInputFocus, jobId, maxDuration, onComplete],
  );

  const handleDeleteInput = useCallback(
    (input: string, key: any) => {
      // If no trims available, renderDeleteMode already shows appropriate message
      if (trims.length === 0) {
        return;
      }

      // Arrow key navigation
      if (key.upArrow) {
        setSelectedTrimIndex((prev) => (prev > 0 ? prev - 1 : trims.length - 1));
        setDeleteConfirmId(null); // Clear confirmation when navigating
        setError(null);
        return;
      }

      if (key.downArrow) {
        setSelectedTrimIndex((prev) => (prev < trims.length - 1 ? prev + 1 : 0));
        setDeleteConfirmId(null); // Clear confirmation when navigating
        setError(null);
        return;
      }

      // Enter key to delete (with confirmation)
      if (key.return) {
        const trim = trims[selectedTrimIndex];
        if (!trim) return;

        if (deleteConfirmId === trim.id) {
          // Second press: confirm deletion
          setLoading(true);

          const port = process.env.MOOMBOX_PORT || "774";
          fetch(
            `http://127.0.0.1:${port}/api/jobs/${jobId}/trims/${trim.id}`,
            {
              method: "DELETE",
              headers: {
                "Origin": `http://127.0.0.1:${port}`,
              },
            },
          )
            .then((res) => {
              if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
              return res.json();
            })
            .then(() => {
              onComplete({
                text: `Trim deleted: ${trim.filename}`,
                color: "yellow",
              });
            })
            .catch((err) => {
              setError(err.message || "Failed to delete trim");
              setLoading(false);
            });
        } else {
          // First press: show confirmation
          setDeleteConfirmId(trim.id);
          setError(null);
        }
      }
    },
    [trims, deleteConfirmId, selectedTrimIndex, jobId, onComplete],
  );

  const renderCreateMode = () => {
    if (step === 0) {
      // Step 0: Both start and end times (Tab to switch focus)
      return (
        <Box flexDirection="column" paddingX={1}>
          <Box marginTop={1}>
            <Text>
              <Text color="gray">Video: </Text>
              <Text bold>{job.title}</Text>
            </Text>
          </Box>
          <Box>
            <Text color="gray">
              Duration: {formatTime(job.lengthSeconds || 0)}
            </Text>
          </Box>
          {trims.length > 0 && (
            <Box>
              <Text color="gray">Existing trims: {trims.length}</Text>
            </Box>
          )}

          <Box marginTop={1}>
            <Text bold>Step 1/2: Enter Time Range</Text>
          </Box>
          <Box>
            <Text color="gray">
              (HH:MM:SS, MM:SS, or seconds - Tab to switch fields)
            </Text>
          </Box>

          {/* Start time input */}
          <Box marginTop={1}>
            <Text color={timeInputFocus === "start" ? "cyan" : "gray"}>
              Start:{" "}
            </Text>
            <Text>{startInput || "0"}</Text>
            {timeInputFocus === "start" && <Text color="cyan">█</Text>}
          </Box>

          {/* End time input */}
          <Box>
            <Text color={timeInputFocus === "end" ? "cyan" : "gray"}>
              End:   {" "}
            </Text>
            <Text>{endInput || (job.lengthSeconds ? formatTime(job.lengthSeconds) : "end")}</Text>
            {timeInputFocus === "end" && <Text color="cyan">█</Text>}
          </Box>

          <Box marginTop={1}>
            <Text color="gray" dimColor>
              Tab = switch field, Enter = continue, Esc = cancel
            </Text>
          </Box>
        </Box>
      );
    } else {
      // Step 1: Confirmation
      const start = parseTimeToSeconds(startInput)!;
      const end = parseTimeToSeconds(endInput)!;
      const duration = end - start;
      const estimatedSize = job.fileSize
        ? (job.fileSize / (job.lengthSeconds || 1)) * duration
        : 0;

      return (
        <Box flexDirection="column" paddingX={1}>
          <Box marginTop={1}>
            <Text bold>Step 2/2: Confirmation</Text>
          </Box>

          <Box marginTop={1}>
            <Text color="gray">Time Range: </Text>
            <Text color="green">
              {formatTime(start)} - {formatTime(end)}
            </Text>
          </Box>
          <Box>
            <Text color="gray">Duration: </Text>
            <Text>{formatDuration(duration)}</Text>
          </Box>
          {estimatedSize > 0 && (
            <Box>
              <Text color="gray">Estimated Size: </Text>
              <Text>~{formatBytes(estimatedSize)}</Text>
            </Box>
          )}

          <Box marginTop={1}>
            <Text color="yellow">
              ⚠ This will create a new file with re-encoded video/audio
            </Text>
          </Box>

          <Box marginTop={1}>
            <Text color="gray" dimColor>
              Press Enter to create trim, Esc to go back
            </Text>
          </Box>
        </Box>
      );
    }
  };

  const renderDeleteMode = () => {
    if (trims.length === 0) {
      return (
        <Box flexDirection="column" paddingX={1}>
          <Box marginTop={1}>
            <Text color="gray">No trims available to delete.</Text>
          </Box>
          <Box marginTop={1}>
            <Text color="gray" dimColor>
              Press Esc to go back to CREATE mode
            </Text>
          </Box>
        </Box>
      );
    }

    return (
      <Box flexDirection="column" paddingX={1}>
        <Box marginTop={1}>
          <Text>Navigate with arrow keys, press Enter to delete:</Text>
        </Box>

        <Box flexDirection="column" marginTop={1}>
          {trims.map((trim, idx) => {
            const isCurrent = idx === selectedTrimIndex;
            const isConfirmed = deleteConfirmId === trim.id;
            const range = `${formatTime(trim.startTime)} - ${formatTime(trim.endTime)}`;
            const duration = formatDuration(trim.duration);
            const size = trim.fileSize ? formatBytes(trim.fileSize) : "?";

            return (
              <Box key={trim.id}>
                <Text color={isConfirmed ? "yellow" : isCurrent ? "magenta" : "white"}>
                  {isCurrent ? "▸ " : "  "}
                  {range} ({duration}, {size})
                </Text>
              </Box>
            );
          })}
        </Box>

        {deleteConfirmId && (
          <Box marginTop={1}>
            <Text color="yellow">
              ⚠ Press Enter again to confirm deletion
            </Text>
          </Box>
        )}

        <Box marginTop={1}>
          <Text color="gray" dimColor>
            ↑/↓ to navigate, Enter to delete, Esc to cancel
          </Text>
        </Box>
      </Box>
    );
  };

  return (
    <Box
      flexDirection="column"
      width={width}
      height={height}
      borderStyle="round"
      borderColor={borderColor}
    >
      {/* Header */}
      <Box paddingX={1}>
        <Text color={borderColor} bold>
          Trim Video - {mode === "create" ? "CREATE" : "DELETE"}
        </Text>
        <Text color="gray"> (M for management)</Text>
      </Box>

      {/* Content */}
      {mode === "create" ? renderCreateMode() : renderDeleteMode()}

      {/* Error display */}
      {error && (
        <Box paddingX={1} marginTop={1}>
          <Text color="red">❌ {error}</Text>
        </Box>
      )}

      {/* Loading indicator */}
      {loading && (
        <Box paddingX={1} marginTop={1}>
          <Text color="yellow">⏳ Creating trim, please wait...</Text>
        </Box>
      )}
    </Box>
  );
}

// Helper functions

function validateTrimRange(
  start: number,
  end: number,
  maxDuration?: number,
): string | null {
  if (start < 0) return "Start time cannot be negative";
  if (end <= start) return "End time must be after start time";
  if (maxDuration && end > maxDuration) {
    return `End exceeds duration (${formatTime(maxDuration)})`;
  }
  if (end - start < 1) return "Trim must be at least 1 second";
  return null;
}

function formatTime(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0)
    return `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  return `${m}:${String(s).padStart(2, "0")}`;
}

function formatDuration(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}h ${m}m ${s}s`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes}B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)}KB`;
  if (bytes < 1024 * 1024 * 1024)
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)}GB`;
}
