import React, { useState, useCallback } from "react";
import { Box, Text, useInput } from "ink";
import { ConfigManager } from "../../core/config.js";
import { installYtdlpPlugin } from "../../core/ytdlpPlugin.js";
import { AutoCookieService } from "../../core/autoCookies.js";
import type { MoomboxConfig } from "../../types/config.js";
import { useMouse, MouseEvent as TuiMouseEvent } from "../hooks/useMouse.js";
import { readClipboard } from "../clipboard.js";
import { getErrorMessage } from "../../types/errors.js";

interface SetupWizardProps {
  width: number;
  height: number;
  onComplete: () => void;
}

type FieldType = "text" | "number" | "toggle" | "cycle";

interface FieldDef {
  key: string;
  label: string;
  defaultDisplay: string;
  help?: string;
  type: FieldType;
  options?: string[]; // For toggle (Yes/No) or cycle (DEBUG/INFO/WARN/ERROR)
}

interface StepDef {
  title: string;
  subtitle?: string;
  fields: FieldDef[];
  footer?: string;
}

const STEPS: StepDef[] = [
  {
    title: "Output & Paths",
    subtitle: "Leave fields empty to use defaults",
    fields: [
      {
        key: "outputDir",
        label: "Output directory",
        defaultDisplay: "./output",
        help: "Where finished downloads are saved",
        type: "text",
      },
      {
        key: "outputTemplate",
        label: "Filename template",
        defaultDisplay: "${channel}/${start_date} ${title} [${id}]",
        help: "Variables: ${title} ${id} ${channel} ${start_date} ${start_time}",
        type: "text",
      },
      {
        key: "stagingDir",
        label: "Staging directory",
        defaultDisplay: "./staging",
        help: "Temporary directory for segments during download",
        type: "text",
      },
      {
        key: "databasePath",
        label: "Database path",
        defaultDisplay: "./moombox.json",
        help: "Job database file location",
        type: "text",
      },
      {
        key: "ffmpegPath",
        label: "FFmpeg path",
        defaultDisplay: "system PATH",
        help: "Leave empty to use system ffmpeg",
        type: "text",
      },
    ],
  },
  {
    title: "Download Preferences",
    subtitle: "Leave fields empty to use defaults",
    fields: [
      {
        key: "maxRes",
        label: "Max resolution",
        defaultDisplay: "1080",
        help: "Maximum video dimension (width or height)",
        type: "number",
      },
      {
        key: "prefer60fps",
        label: "Prefer 60fps",
        defaultDisplay: "Yes",
        help: "When same resolution, prefer 60fps. Resolution always wins",
        type: "toggle",
        options: ["Yes", "No"],
      },
      {
        key: "numParallel",
        label: "Parallel downloads",
        defaultDisplay: "2",
        help: "How many streams to download simultaneously",
        type: "number",
      },
      {
        key: "downloadChat",
        label: "Download chat",
        defaultDisplay: "Yes",
        help: "Save live chat as JSON alongside video",
        type: "toggle",
        options: ["Yes", "No"],
      },
      {
        key: "autoCookies",
        label: "Auto cookie login",
        defaultDisplay: "No",
        help: "Opens browser for Google login, grabs cookies automatically",
        type: "toggle",
        options: ["No", "Yes"],
      },
      {
        key: "cookieFile",
        label: "Cookie file (manual)",
        defaultDisplay: "empty = none",
        help: "Netscape-format cookie file. Not needed if auto-login is enabled",
        type: "text",
      },
    ],
  },
  {
    title: "Logging",
    subtitle: "Leave fields empty to use defaults",
    fields: [
      {
        key: "logLevel",
        label: "Log level",
        defaultDisplay: "INFO",
        help: "Verbosity of log output",
        type: "cycle",
        options: ["DEBUG", "INFO", "WARN", "ERROR"],
      },
      {
        key: "logFilePath",
        label: "Log file path",
        defaultDisplay: "./moombox.log",
        help: "Where log file is written",
        type: "text",
      },
      {
        key: "logMaxSize",
        label: "Max log file size",
        defaultDisplay: "10485760",
        help: "Maximum bytes before log rotation (10MB)",
        type: "number",
      },
      {
        key: "logMaxFiles",
        label: "Max log files",
        defaultDisplay: "5",
        help: "Number of rotated log files to keep",
        type: "number",
      },
    ],
  },
  {
    title: "Display & Monitoring",
    subtitle: "Leave fields empty to use defaults",
    fields: [
      {
        key: "hideAge",
        label: "Hide finished after (days)",
        defaultDisplay: "30",
        help: "Finished jobs older than this move to Archived",
        type: "number",
      },
      {
        key: "maxFeedItems",
        label: "Max feed items",
        defaultDisplay: "15",
        help: "RSS feed items to check per channel",
        type: "number",
      },
      {
        key: "port",
        label: "Dashboard port",
        defaultDisplay: "774",
        help: "Web dashboard & API port (tries nearby if busy)",
        type: "number",
      },
      {
        key: "networkAccess",
        label: "Network access",
        defaultDisplay: "Localhost",
        help: "Who can access the dashboard",
        type: "cycle",
        options: ["Localhost", "LAN", "External"],
      },
      {
        key: "installYtdlpPlugin",
        label: "Install yt-dlp plugin",
        defaultDisplay: "No",
        help: "Install a yt-dlp plugin so it uses Moombox for PO tokens",
        type: "toggle",
        options: ["Yes", "No"],
      },
    ],
  },
  {
    title: "Monitor a Channel (Optional)",
    subtitle: "Leave empty to skip channel monitoring",
    fields: [
      {
        key: "channelId",
        label: "Channel ID",
        defaultDisplay: "empty = skip",
        help: "YouTube channel ID (starts with UC)",
        type: "text",
      },
      {
        key: "channelName",
        label: "Display name",
        defaultDisplay: "empty",
        help: "Friendly name shown in task list",
        type: "text",
      },
      {
        key: "channelTerms",
        label: "Filter regex",
        defaultDisplay: "empty = all streams",
        help: "Only archive streams matching this pattern",
        type: "text",
      },
      {
        key: "includeNonLive",
        label: "Include non-live",
        defaultDisplay: "No",
        help: "Also download regular uploaded videos",
        type: "toggle",
        options: ["No", "Yes"],
      },
    ],
    footer: "Examples: (?i)karaoke  |  (?i)(singing|\u6B4C\u679A)  |  (?i)unarchived",
  },
];

const TOTAL_STEPS = STEPS.length;

export function SetupWizard({ width, height, onComplete }: SetupWizardProps): React.ReactElement {
  const [step, setStep] = useState(0);
  const [focusedField, setFocusedField] = useState(0);
  const [values, setValues] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const currentStep = STEPS[step];
  const fieldCount = currentStep.fields.length;

  const setValue = useCallback((key: string, val: string) => {
    setValues((prev) => ({ ...prev, [key]: val }));
  }, []);

  const getFieldValue = useCallback(
    (field: FieldDef): string => {
      const val = values[field.key];
      if (val !== undefined) return val;
      // Initialize toggles/cycles to their default
      if (field.type === "toggle" || field.type === "cycle") {
        return field.defaultDisplay;
      }
      return "";
    },
    [values],
  );

  const cycleOption = useCallback(
    (field: FieldDef, direction: number) => {
      if (!field.options) return;
      const current = getFieldValue(field);
      const idx = field.options.indexOf(current);
      const next = (idx + direction + field.options.length) % field.options.length;
      const newVal = field.options[next];
      setValue(field.key, newVal);
    },
    [getFieldValue, setValue],
  );

  // Right-click paste into text/number fields
  useMouse({
    onMouseEvent: useCallback((event: TuiMouseEvent) => {
      if (event.type === "click" && event.button === "right") {
        const field = STEPS[step]?.fields[focusedField];
        if (field && field.type !== "toggle" && field.type !== "cycle") {
          readClipboard().then((text) => {
            if (text) {
              const firstLine = text.split(/[\r\n]/)[0].trim();
              if (firstLine) {
                // For number fields, only paste digits
                const filtered = field.type === "number"
                  ? firstLine.replace(/\D/g, "")
                  : firstLine;
                if (filtered) {
                  setValue(field.key, (values[field.key] || "") + filtered);
                }
              }
            }
          });
        }
      }
    }, [step, focusedField, values, setValue]),
  });

  const finishSetup = useCallback(async () => {
    setSaving(true);
    setError(null);

    const v = (key: string): string => (values[key] || "").trim();
    const vNum = (key: string): number | undefined => {
      const s = v(key);
      return s ? parseInt(s, 10) : undefined;
    };
    const vBool = (key: string, defaultVal: boolean): boolean => {
      const s = values[key];
      if (s === undefined) return defaultVal;
      return s === "Yes";
    };

    // Map TUI display values to config values
    const netAccessMap: Record<string, string> = {
      "Localhost": "localhost",
      "LAN": "lan",
      "External": "external",
    };
    const networkAccess = netAccessMap[values.networkAccess || "Localhost"] || "localhost";

    const config: MoomboxConfig = {
      port: vNum("port"),
      network_access: networkAccess,
      log_level: values.logLevel || undefined,
      log_file_path: v("logFilePath") || undefined,
      log_max_file_size: vNum("logMaxSize"),
      log_max_files: vNum("logMaxFiles"),
      database_path: v("databasePath") || undefined,
      max_feed_items: vNum("maxFeedItems"),
      downloader: {
        output_directory: v("outputDir") || undefined,
        output_template: v("outputTemplate") || undefined,
        staging_directory: v("stagingDir") || undefined,
        num_parallel_downloads: vNum("numParallel"),
        max_video_resolution: vNum("maxRes"),
        prefer_60fps: vBool("prefer60fps", true),
        download_chat: vBool("downloadChat", true),
        ffmpeg_path: v("ffmpegPath") || undefined,
        cookie_file: v("cookieFile") || undefined,
      },
      tasklist: {
        hide_finished_age_days: vNum("hideAge"),
      },
      auto_cookies: {
        enabled: vBool("autoCookies", false),
      },
    };

    const channelId = v("channelId");
    if (channelId) {
      config.channels = [
        {
          id: channelId,
          name: v("channelName") || undefined,
          terms: v("channelTerms") || undefined,
          include_non_live_content: vBool("includeNonLive", false) || undefined,
        },
      ];
    }

    try {
      await ConfigManager.getInstance().save(config);

      // Install yt-dlp plugin if requested
      if (vBool("installYtdlpPlugin", false)) {
        try {
          const port = vNum("port") || 774;
          await installYtdlpPlugin(port, true);
        } catch {
          // Non-fatal — don't block setup completion
        }
      }

      // Start auto-cookie browser setup if enabled
      if (vBool("autoCookies", false)) {
        try {
          const autoCookies = AutoCookieService.getInstance();
          await autoCookies.startSetup();
          // User will close the browser themselves; finishSetup() is called
          // from the web dashboard or next startup
        } catch {
          // Non-fatal — user can run setup later from Settings
        }
      }

      onComplete();
    } catch (e: unknown) {
      const msg = getErrorMessage(e);
      setError(`Failed to save: ${msg}`);
      setSaving(false);
    }
  }, [values, onComplete]);

  useInput((input, key) => {
    if (saving) return;

    // Clear error on any input
    if (error) setError(null);

    const field = currentStep.fields[focusedField];

    // Esc: go back one step (no-op on step 0)
    if (key.escape) {
      if (step > 0) {
        setStep((s) => s - 1);
        setFocusedField(0);
      }
      return;
    }

    // Left/Right: cycle toggle/cycle fields
    if (field && (field.type === "toggle" || field.type === "cycle")) {
      if (key.leftArrow) {
        cycleOption(field, -1);
        return;
      }
      if (key.rightArrow) {
        cycleOption(field, 1);
        return;
      }
    }

    // Enter: advance step (Left/Right already cycle toggle/cycle options)
    if (key.return) {
      if (step < TOTAL_STEPS - 1) {
        setStep((s) => s + 1);
        setFocusedField(0);
      } else {
        finishSetup();
      }
      return;
    }

    // Tab: next step (same as Enter for non-toggle fields)
    if (key.tab) {
      if (step < TOTAL_STEPS - 1) {
        setStep((s) => s + 1);
        setFocusedField(0);
      } else {
        finishSetup();
      }
      return;
    }

    // Up/Down: navigate fields
    if (key.upArrow) {
      setFocusedField((f) => Math.max(0, f - 1));
      return;
    }
    if (key.downArrow) {
      setFocusedField((f) => Math.min(fieldCount - 1, f + 1));
      return;
    }

    // Text/number editing on focused field
    if (!field) return;
    if (field.type === "toggle" || field.type === "cycle") return;

    const currentVal = getFieldValue(field);

    if (key.backspace || key.delete) {
      setValue(field.key, currentVal.slice(0, -1));
      return;
    }

    // Ctrl+V: paste from clipboard
    if (key.ctrl && input === "v") {
      readClipboard().then((text) => {
        if (text) {
          const firstLine = text.split(/[\r\n]/)[0].trim();
          const filtered = field.type === "number"
            ? firstLine.replace(/\D/g, "")
            : firstLine;
          if (filtered) setValue(field.key, currentVal + filtered);
        }
      });
      return;
    }

    if (input && !key.ctrl && !key.meta) {
      // Guard: reject mouse escape sequence fragments that leaked through stdin filter
      if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
      if (input.length > 1 && input[0] === "[") return;
      // Number fields only accept digits
      if (field.type === "number" && !/^\d$/.test(input)) return;
      setValue(field.key, currentVal + input);
    }
  });

  // Layout
  const boxWidth = Math.min(width, 72);
  const contentHeight = height - 2; // border
  const leftPad = Math.max(0, Math.floor((width - boxWidth) / 2));

  return (
    <Box flexDirection="column" width={width} height={height}>
      {/* Center the wizard box */}
      <Box height={Math.max(0, Math.floor((height - contentHeight) / 2))} />
      <Box paddingLeft={leftPad}>
        <Box
          flexDirection="column"
          width={boxWidth}
          height={Math.min(contentHeight, height)}
          borderStyle="round"
          borderColor="cyan"
        >
          {/* Title */}
          <Box paddingX={1} justifyContent="space-between">
            <Text color="cyan" bold>Moombox Setup</Text>
            <Text color="gray">
              Step {step + 1}/{TOTAL_STEPS}
            </Text>
          </Box>

          {/* Step indicator */}
          <Box paddingX={1} height={1}>
            {STEPS.map((s, i) => {
              const isComplete = i < step;
              const isCurrent = i === step;
              const marker = isComplete ? "+" : isCurrent ? ">" : " ";
              const color = isComplete ? "green" : isCurrent ? "cyan" : "gray";
              return (
                <React.Fragment key={i}>
                  {i > 0 && <Text color="gray"> - </Text>}
                  <Text color={color}>
                    [{marker}] {i + 1}
                  </Text>
                </React.Fragment>
              );
            })}
          </Box>

          {/* Step title + subtitle */}
          <Box paddingX={1} flexDirection="column">
            <Text color="white" bold>{currentStep.title}</Text>
            {currentStep.subtitle && (
              <Text color="gray" dimColor>{currentStep.subtitle}</Text>
            )}
          </Box>

          <Box paddingX={1} height={1}>
            <Text color="gray">{"\u2500".repeat(boxWidth - 4)}</Text>
          </Box>

          {/* Fields */}
          <Box flexDirection="column" paddingX={1} flexGrow={1}>
            {currentStep.fields.map((field, idx) => {
              const isFocused = idx === focusedField;
              const val = getFieldValue(field);
              const isToggleOrCycle = field.type === "toggle" || field.type === "cycle";

              return (
                <Box key={field.key} flexDirection="column" marginBottom={idx < fieldCount - 1 ? 1 : 0}>
                  <Box>
                    <Text color={isFocused ? "cyan" : "white"}>
                      {isFocused ? "> " : "  "}
                    </Text>
                    <Text color={isFocused ? "cyan" : "white"} bold={isFocused}>
                      {field.label}
                    </Text>
                    <Text color="gray" dimColor> (default: {field.defaultDisplay})</Text>
                  </Box>
                  <Box paddingLeft={2}>
                    {isToggleOrCycle ? (
                      <OptionSelector
                        options={field.options!}
                        selected={val}
                        focused={isFocused}
                      />
                    ) : (
                      <>
                        <Text color="gray">[</Text>
                        {!val && !isFocused ? (
                          <Text color="gray" dimColor>{field.defaultDisplay}</Text>
                        ) : (
                          <Text>{val}</Text>
                        )}
                        {isFocused && <Text color="cyan">_</Text>}
                        <Text color="gray">]</Text>
                      </>
                    )}
                  </Box>
                  {field.help && (
                    <Box paddingLeft={3}>
                      <Text color="gray" dimColor>{field.help}</Text>
                    </Box>
                  )}
                  {field.key === "networkAccess" && val === "External" && (
                    <Box paddingLeft={3}>
                      <Text color="yellow" bold>Warning: </Text>
                      <Text color="yellow">Security measures were not audited by a specialist. No authentication — anyone who reaches this port has full access.</Text>
                    </Box>
                  )}
                </Box>
              );
            })}
          </Box>

          {/* Step footer (e.g. regex examples) */}
          {currentStep.footer && (
            <Box paddingX={1}>
              <Text color="gray" dimColor>{currentStep.footer}</Text>
            </Box>
          )}

          {/* Error */}
          {error && (
            <Box paddingX={1}>
              <Text color="red">{error}</Text>
            </Box>
          )}

          {/* Saving indicator */}
          {saving && (
            <Box paddingX={1}>
              <Text color="cyan">Saving configuration...</Text>
            </Box>
          )}

          {/* Navigation hints */}
          <Box paddingX={1} height={1} justifyContent="space-between">
            <Box>
              {step > 0 && <Text color="gray">Esc: Back</Text>}
            </Box>
            <Box>
              <Text color="gray">{"\u2191/\u2193: Fields"}</Text>
              <Text color="gray">{"  \u2190/\u2192: Toggle"}</Text>
              {step < TOTAL_STEPS - 1 ? (
                <Text color="gray">{"  Tab/Enter: Next"}</Text>
              ) : (
                <Text color="cyan" bold>{"  Tab: Finish"}</Text>
              )}
            </Box>
          </Box>
        </Box>
      </Box>
    </Box>
  );
}

interface OptionSelectorProps {
  options: string[];
  selected: string;
  focused: boolean;
}

function OptionSelector({ options, selected, focused }: OptionSelectorProps): React.ReactElement {
  return (
    <Box>
      {options.map((opt, i) => {
        const isSelected = opt === selected;
        return (
          <React.Fragment key={opt}>
            {i > 0 && <Text color="gray"> / </Text>}
            {isSelected ? (
              <Text color={focused ? "cyan" : "white"} bold>[{opt}]</Text>
            ) : (
              <Text color="gray" dimColor>{opt}</Text>
            )}
          </React.Fragment>
        );
      })}
    </Box>
  );
}
