import React, { useState, useCallback, useEffect } from "react";
import { Box, Text, useInput } from "ink";
import { ConfigManager, type NormalizedConfig } from "../../core/config.js";
import type { ChannelConfig, NotificationConfig, MoomboxConfig } from "../../types/config.js";
import { Logger } from "../../core/logger.js";
import { DownloadWorker } from "../../core/worker/index.js";
import { ChannelEditor } from "./ChannelEditor.js";
import { NotificationEditor } from "./NotificationEditor.js";
import { SecurityEditor } from "./SecurityEditor.js";
import { useMouse, MouseEvent as TuiMouseEvent } from "../hooks/useMouse.js";
import { readClipboard } from "../clipboard.js";
import { getErrorMessage } from "../../types/errors.js";

// Settings that require a full process restart to take effect
const RESTART_REQUIRED_KEYS = new Set([
  "port", "network_access", "database_path",
  "log_file_path", "log_max_file_size", "log_max_files",
]);

type FieldType = "text" | "number" | "toggle" | "cycle";

interface FieldDef {
  key: string;
  label: string;
  type: FieldType;
  options?: string[];
  configPath: string[];
  help?: string;
}

const GENERAL_FIELDS: FieldDef[] = [
  { key: "port", label: "Port", type: "number", configPath: ["port"], help: "1-65535" },
  { key: "network_access", label: "Network access", type: "cycle", options: ["localhost", "lan", "external"], configPath: ["network_access"], help: "localhost, lan, or external" },
  { key: "log_level", label: "Log level", type: "cycle", options: ["DEBUG", "INFO", "WARN", "ERROR"], configPath: ["log_level"], help: "DEBUG, INFO, WARN, ERROR" },
  { key: "log_file_path", label: "Log file path", type: "text", configPath: ["log_file_path"] },
  { key: "log_max_file_size", label: "Max log file size", type: "number", configPath: ["log_max_file_size"], help: "bytes" },
  { key: "log_max_files", label: "Max log files", type: "number", configPath: ["log_max_files"] },
  { key: "database_path", label: "Database path", type: "text", configPath: ["database_path"], help: "path to moombox.json" },
  { key: "max_feed_items", label: "Max feed items", type: "number", configPath: ["max_feed_items"], help: "RSS items per feed" },
  { key: "feed_check_interval", label: "Feed check interval", type: "number", configPath: ["feed_check_interval"], help: "minutes" },
  { key: "decapi_check_interval", label: "DECAPI check interval", type: "number", configPath: ["decapi_check_interval"], help: "seconds (0=dynamic)" },
  { key: "twitch_check_interval", label: "Twitch check interval", type: "number", configPath: ["twitch_check_interval"], help: "seconds (default: 15)" },
  { key: "hide_finished_age_days", label: "Hide finished after", type: "number", configPath: ["tasklist", "hide_finished_age_days"], help: "days" },
];

const DOWNLOADER_FIELDS: FieldDef[] = [
  { key: "output_directory", label: "Output directory", type: "text", configPath: ["downloader", "output_directory"], help: "where finished files go" },
  { key: "output_template", label: "Output template", type: "text", configPath: ["downloader", "output_template"], help: "${title} ${id} ${channel} ${start_date} ${start_time}" },
  { key: "staging_directory", label: "Staging directory", type: "text", configPath: ["downloader", "staging_directory"], help: "temp files during download" },
  { key: "ffmpeg_path", label: "FFmpeg path", type: "text", configPath: ["downloader", "ffmpeg_path"], help: "empty = system PATH" },
  { key: "cookie_file", label: "Cookie file", type: "text", configPath: ["downloader", "cookie_file"], help: "Netscape format cookies.txt" },
  { key: "max_video_resolution", label: "Max resolution", type: "number", configPath: ["downloader", "max_video_resolution"], help: "pixels" },
  { key: "num_parallel_downloads", label: "Parallel downloads", type: "number", configPath: ["downloader", "num_parallel_downloads"], help: "concurrent jobs" },
  { key: "download_chat", label: "Download chat", type: "toggle", options: ["Yes", "No"], configPath: ["downloader", "download_chat"] },
  { key: "prefer_60fps", label: "Prefer 60fps", type: "toggle", options: ["Yes", "No"], configPath: ["downloader", "prefer_60fps"] },
  { key: "segment_retry_delay_cap", label: "Segment retry cap", type: "number", configPath: ["downloader", "segment_retry_delay_cap"], help: "seconds" },
  { key: "segment_live_check_retries", label: "Live check retries", type: "number", configPath: ["downloader", "segment_live_check_retries"], help: "before marking stream ended" },
];

const FIELD_SECTIONS = [GENERAL_FIELDS, DOWNLOADER_FIELDS];
const SECTION_NAMES = ["General", "Downloader", "Channels", "Notifications", "Security"];
const TOTAL_SECTIONS = SECTION_NAMES.length;

/** Read a nested config value */
function getConfigValue(config: NormalizedConfig, fieldPath: string[]): unknown {
  let obj: unknown = config;
  for (const key of fieldPath) {
    if (obj == null || typeof obj !== "object") return undefined;
    obj = (obj as Record<string, unknown>)[key];
  }
  return obj;
}

/** Convert config value to display string */
function configToDisplay(field: FieldDef, config: NormalizedConfig): string {
  const val = getConfigValue(config, field.configPath);
  if (val === undefined || val === null) return "";
  if (field.type === "toggle") return val === true ? "Yes" : "No";
  return String(val);
}

interface SettingsPanelProps {
  width: number;
  height: number;
  onClose: (needsRestart: boolean) => void;
}

export function SettingsPanel({ width, height, onClose }: SettingsPanelProps): React.ReactElement {
  const [sectionIndex, setSectionIndex] = useState(0);
  const [focusedField, setFocusedField] = useState(0);
  const [values, setValues] = useState<Record<string, string>>({});
  const [originalValues, setOriginalValues] = useState<Record<string, string>>({});
  const [channels, setChannels] = useState<ChannelConfig[]>([]);
  const [notifications, setNotifications] = useState<NotificationConfig[]>([]);
  const [saveStatus, setSaveStatus] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [errorMsg, setErrorMsg] = useState("");
  const [confirmRestart, setConfirmRestart] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [subEditorMode, setSubEditorMode] = useState<"list" | "edit">("list");

  const isFieldSection = sectionIndex < 2;
  const fields = isFieldSection ? FIELD_SECTIONS[sectionIndex] : [];
  const fieldCount = fields.length;

  // Load current config on mount
  useEffect(() => {
    const config = ConfigManager.getInstance().get();
    const vals: Record<string, string> = {};
    for (const fieldList of FIELD_SECTIONS) {
      for (const field of fieldList) {
        vals[field.key] = configToDisplay(field, config);
      }
    }
    setValues(vals);
    setOriginalValues({ ...vals });
    setChannels(config.channels ? JSON.parse(JSON.stringify(config.channels)) : []);
    setNotifications(config.notifications ? JSON.parse(JSON.stringify(config.notifications)) : []);
  }, []);

  const setValue = useCallback((key: string, val: string) => {
    setValues(prev => ({ ...prev, [key]: val }));
    setSaveStatus("idle");
    setDirty(true);
  }, []);

  const getFieldValue = useCallback((field: FieldDef): string => {
    return values[field.key] ?? "";
  }, [values]);

  const cycleOption = useCallback((field: FieldDef, direction: number) => {
    if (!field.options) return;
    const current = getFieldValue(field);
    const idx = field.options.indexOf(current);
    const next = (idx + direction + field.options.length) % field.options.length;
    setValue(field.key, field.options[next]);
  }, [getFieldValue, setValue]);

  const switchSection = useCallback((newIdx: number) => {
    setSectionIndex(newIdx);
    setFocusedField(0);
    setSubEditorMode("list");
  }, []);

  const handleChannelsChange = useCallback((newChannels: ChannelConfig[]) => {
    setChannels(newChannels);
    setDirty(true);
    setSaveStatus("idle");
  }, []);

  const handleNotificationsChange = useCallback((newNotifs: NotificationConfig[]) => {
    setNotifications(newNotifs);
    setDirty(true);
    setSaveStatus("idle");
  }, []);

  /** Check if any restart-required fields changed */
  const hasRestartChanges = useCallback((): boolean => {
    for (const key of RESTART_REQUIRED_KEYS) {
      if (values[key] !== originalValues[key]) return true;
    }
    return false;
  }, [values, originalValues]);

  /** Build full config and save */
  const saveConfig = useCallback(async (): Promise<boolean> => {
    const config = ConfigManager.getInstance().get();

    // Validate external access requires password
    if (values.network_access === "external" && !config.password_hash) {
      setErrorMsg("Password required for external access. Set in Security section (5).");
      setSaveStatus("error");
      return false;
    }

    // Validate port range
    const port = parseInt(values.port, 10);
    if (isNaN(port) || port < 1 || port > 65535) {
      setErrorMsg("Port must be 1-65535");
      setSaveStatus("error");
      return false;
    }

    setSaveStatus("saving");

    const newConfig: Record<string, unknown> = {
      port: parseInt(values.port, 10) || config.port,
      network_access: values.network_access || config.network_access,
      password_hash: config.password_hash,
      log_level: values.log_level || config.log_level,
      log_file_path: values.log_file_path || config.log_file_path,
      log_max_file_size: parseInt(values.log_max_file_size, 10) || config.log_max_file_size,
      log_max_files: parseInt(values.log_max_files, 10) || config.log_max_files,
      database_path: values.database_path || config.database_path,
      max_feed_items: parseInt(values.max_feed_items, 10) || config.max_feed_items,
      feed_check_interval: parseInt(values.feed_check_interval, 10) || config.feed_check_interval,
      decapi_check_interval: parseInt(values.decapi_check_interval, 10) || undefined,
      twitch_check_interval: parseInt(values.twitch_check_interval, 10) || undefined,
      downloader: {
        output_directory: values.output_directory || config.downloader.output_directory,
        output_template: values.output_template || config.downloader.output_template,
        staging_directory: values.staging_directory || config.downloader.staging_directory,
        ffmpeg_path: values.ffmpeg_path || undefined,
        cookie_file: values.cookie_file || config.downloader.cookie_file,
        max_video_resolution: parseInt(values.max_video_resolution, 10) || config.downloader.max_video_resolution,
        num_parallel_downloads: parseInt(values.num_parallel_downloads, 10) || config.downloader.num_parallel_downloads,
        download_chat: values.download_chat === "Yes",
        prefer_60fps: values.prefer_60fps === "Yes",
        segment_retry_delay_cap: parseInt(values.segment_retry_delay_cap, 10) || config.downloader.segment_retry_delay_cap,
        segment_live_check_retries: parseInt(values.segment_live_check_retries, 10) || config.downloader.segment_live_check_retries,
        po_token: config.downloader.po_token,
        visitor_data: config.downloader.visitor_data,
        pot_provider_url: config.downloader.pot_provider_url,
      },
      tasklist: {
        hide_finished_age_days: parseInt(values.hide_finished_age_days, 10) || config.tasklist?.hide_finished_age_days,
      },
      auto_cookies: config.auto_cookies,
      notifications: notifications.length > 0 ? notifications : undefined,
      channels: channels.length > 0 ? channels : undefined,
    };

    try {
      await ConfigManager.getInstance().save(newConfig as unknown as MoomboxConfig);

      // Hot-reload runtime settings
      Logger.getInstance().refreshLogLevel();
      const newParallel = parseInt(values.num_parallel_downloads, 10);
      if (newParallel >= 1) {
        DownloadWorker.getInstance().setMaxDownloadSlots(newParallel);
      }

      setSaveStatus("saved");
      setOriginalValues({ ...values });
      setDirty(false);
      return true;
    } catch (e: unknown) {
      setErrorMsg(`Save failed: ${getErrorMessage(e)}`);
      setSaveStatus("error");
      return false;
    }
  }, [values, channels, notifications]);

  /** Handle close: save if dirty, check restart */
  const handleClose = useCallback(async () => {
    let saved = true;
    if (dirty && saveStatus !== "error" && saveStatus !== "saving") {
      saved = await saveConfig();
      if (!saved) return; // Show error, user can press Esc again to force-close
    }
    // Only prompt for restart if save succeeded (skip if force-closing after error)
    if (saved && saveStatus !== "error" && hasRestartChanges()) {
      setConfirmRestart(true);
    } else {
      onClose(false);
    }
  }, [dirty, saveStatus, saveConfig, hasRestartChanges, onClose]);

  // Right-click paste (field sections only)
  useMouse({
    onMouseEvent: useCallback((event: TuiMouseEvent) => {
      if (!isFieldSection) return;
      if (event.type === "click" && event.button === "right") {
        const field = fields[focusedField];
        if (field && field.type !== "toggle" && field.type !== "cycle") {
          readClipboard().then(text => {
            if (text) {
              const firstLine = text.split(/[\r\n]/)[0].trim();
              const filtered = field.type === "number" ? firstLine.replace(/\D/g, "") : firstLine;
              if (filtered) setValue(field.key, (values[field.key] || "") + filtered);
            }
          });
        }
      }
    }, [isFieldSection, fields, focusedField, values, setValue]),
  });

  useInput((input, key) => {
    // Restart confirmation
    if (confirmRestart) {
      if (key.return) onClose(true);
      else if (key.escape) onClose(false);
      return;
    }

    if (saveStatus === "error") {
      setSaveStatus("idle");
      setErrorMsg("");
    }

    // Esc: close settings (for field sections, or when Channels/Notifications is in list mode)
    // ChannelEditor/NotificationEditor handle their own Esc for edit→list transitions
    if (key.escape && isFieldSection) {
      handleClose();
      return;
    }

    // For Channels/Notifications sections, only handle section navigation here
    // The sub-editors handle their own up/down/enter/etc via useInput
    if (!isFieldSection) {
      // When sub-editor is in edit mode, don't intercept any keys
      // (the sub-editor handles Esc, typing, etc.)
      if (subEditorMode === "edit") return;

      // Esc from channels/notifications list mode closes settings
      if (key.escape) {
        handleClose();
        return;
      }
      // Left/Right arrows: switch sections
      if (key.leftArrow && sectionIndex > 0) {
        switchSection(sectionIndex - 1);
        return;
      }
      if (key.rightArrow && sectionIndex < TOTAL_SECTIONS - 1) {
        switchSection(sectionIndex + 1);
        return;
      }
      // Number keys: jump to section
      if (input >= "1" && input <= String(TOTAL_SECTIONS) && !key.ctrl && !key.meta) {
        const newIdx = parseInt(input, 10) - 1;
        if (newIdx !== sectionIndex) switchSection(newIdx);
      }
      return;
    }

    // Field section keyboard handling
    const field = fields[focusedField];

    // Left/Right: toggle/cycle or section nav
    if (key.leftArrow) {
      if (field && (field.type === "toggle" || field.type === "cycle")) {
        cycleOption(field, -1);
      } else if (sectionIndex > 0) {
        switchSection(sectionIndex - 1);
      }
      return;
    }
    if (key.rightArrow) {
      if (field && (field.type === "toggle" || field.type === "cycle")) {
        cycleOption(field, 1);
      } else if (sectionIndex < TOTAL_SECTIONS - 1) {
        switchSection(sectionIndex + 1);
      }
      return;
    }

    // Number keys: jump to section (only when focused on toggle/cycle field, not text/number)
    const isTextualField = field && field.type !== "toggle" && field.type !== "cycle";
    if (!isTextualField && input >= "1" && input <= String(TOTAL_SECTIONS) && !key.ctrl && !key.meta) {
      const newIdx = parseInt(input, 10) - 1;
      if (newIdx !== sectionIndex) switchSection(newIdx);
      return;
    }

    // Up/Down
    if (key.upArrow) {
      setFocusedField(prev => Math.max(0, prev - 1));
      return;
    }
    if (key.downArrow) {
      setFocusedField(prev => Math.min(fieldCount - 1, prev + 1));
      return;
    }

    // Text/number editing
    if (!field) return;
    if (field.type === "toggle" || field.type === "cycle") return;

    const currentVal = getFieldValue(field);

    if (key.backspace || key.delete) {
      setValue(field.key, currentVal.slice(0, -1));
      return;
    }

    if (key.ctrl && input === "v") {
      readClipboard().then(text => {
        if (text) {
          const firstLine = text.split(/[\r\n]/)[0].trim();
          const filtered = field.type === "number" ? firstLine.replace(/\D/g, "") : firstLine;
          if (filtered) setValue(field.key, currentVal + filtered);
        }
      });
      return;
    }

    if (input && !key.ctrl && !key.meta) {
      if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
      if (input.length > 1 && input[0] === "[") return;
      if (field.type === "number" && !/^\d$/.test(input)) return;
      setValue(field.key, currentVal + input);
    }
  });

  // Layout
  const boxWidth = Math.min(width, 78);
  const leftPad = Math.max(0, Math.floor((width - boxWidth) / 2));
  const innerWidth = boxWidth - 4;

  // Restart confirmation overlay
  if (confirmRestart) {
    return (
      <Box flexDirection="column" width={width} height={height}>
        <Box height={Math.max(0, Math.floor((height - 7) / 2))} />
        <Box paddingLeft={leftPad}>
          <Box flexDirection="column" width={boxWidth} borderStyle="round" borderColor="yellow">
            <Box paddingX={1} flexDirection="column">
              <Text color="yellow" bold>Restart Required</Text>
              <Text> </Text>
              <Text>Some settings require a restart to take effect:</Text>
              <Text color="gray">port, network access, database path, log settings</Text>
              <Text> </Text>
              <Box justifyContent="space-between">
                <Text color="cyan">Enter: Restart now</Text>
                <Text color="gray">Esc: Close without restart</Text>
              </Box>
            </Box>
          </Box>
        </Box>
      </Box>
    );
  }

  // Render section content
  let sectionContent: React.ReactElement;

  if (sectionIndex === 2) {
    // Channels
    sectionContent = (
      <ChannelEditor
        channels={channels}
        onChange={handleChannelsChange}
        focused={true}
        onModeChange={setSubEditorMode}
      />
    );
  } else if (sectionIndex === 3) {
    // Notifications
    sectionContent = (
      <NotificationEditor
        notifications={notifications}
        onChange={handleNotificationsChange}
        focused={true}
        onModeChange={setSubEditorMode}
      />
    );
  } else if (sectionIndex === 4) {
    // Security
    sectionContent = (
      <SecurityEditor
        focused={true}
        onModeChange={setSubEditorMode}
      />
    );
  } else {
    // General / Downloader field sections
    const maxLabel = Math.max(...fields.map(f => f.label.length));
    const dotPadWidth = maxLabel + 2;

    // Build info line for focused field
    const focusedFieldDef = fields[focusedField];
    const focusedVal = focusedFieldDef ? getFieldValue(focusedFieldDef) : "";
    const focusedChanged = focusedFieldDef ? focusedVal !== originalValues[focusedFieldDef.key] : false;
    const focusedRestart = focusedChanged && focusedFieldDef && RESTART_REQUIRED_KEYS.has(focusedFieldDef.key);

    const infoParts: string[] = [];
    if (focusedFieldDef?.help) infoParts.push(focusedFieldDef.help);
    if (focusedRestart) infoParts.push("[restart required]");
    else if (focusedChanged) infoParts.push("[modified]");

    sectionContent = (
      <Box flexDirection="column">
        {fields.map((field, idx) => {
          const isFocused = idx === focusedField;
          const val = getFieldValue(field);
          const isToggleOrCycle = field.type === "toggle" || field.type === "cycle";
          const isChanged = val !== originalValues[field.key];
          const needsRestart = isChanged && RESTART_REQUIRED_KEYS.has(field.key);

          return (
            <Box key={field.key} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={isFocused ? "cyan" : "white"} bold={isFocused}>
                {field.label.padEnd(dotPadWidth)}
              </Text>
              {isToggleOrCycle ? (
                <OptionSelector options={field.options!} selected={val} focused={isFocused} />
              ) : (
                <>
                  <Text>{val}</Text>
                  {isFocused && <Text color="cyan">_</Text>}
                </>
              )}
              {needsRestart && (
                <Text color="yellow" dimColor> *</Text>
              )}
              {isChanged && !needsRestart && (
                <Text color="green" dimColor> *</Text>
              )}
            </Box>
          );
        })}
        {/* Info area for focused field */}
        <Box paddingX={2} height={1} marginTop={1}>
          <Text color="gray">{"\u2500".repeat(innerWidth - 4)}</Text>
        </Box>
        <Box paddingX={2} height={1}>
          {infoParts.length > 0 ? (
            <Text color={focusedRestart ? "yellow" : "gray"} dimColor>{infoParts.join("  ")}</Text>
          ) : (
            <Text> </Text>
          )}
        </Box>
      </Box>
    );
  }

  // Hint text for the active section
  let hintText: string;
  if (isFieldSection) {
    hintText = `${"\u2190/\u2192"}: ${fields[focusedField]?.type === "toggle" || fields[focusedField]?.type === "cycle" ? "Toggle" : "Section"}  \u2191/\u2193: Navigate  1-5: Jump`;
  } else if (sectionIndex === 4) {
    hintText = `\u2190/\u2192: Section  S: Set password  R: Remove  1-5: Jump`;
  } else {
    hintText = `\u2190/\u2192: Section  \u2191/\u2193: Navigate  A: Add  Enter: Edit  D: Delete  1-5: Jump`;
  }

  return (
    <Box flexDirection="column" width={width} height={height}>
      <Box height={1} />
      <Box paddingLeft={leftPad}>
        <Box
          flexDirection="column"
          width={boxWidth}
          height={height - 1}
          borderStyle="round"
          borderColor="cyan"
        >
          {/* Header: section tabs */}
          <Box paddingX={1} justifyContent="space-between">
            <Box>
              <Text color="cyan" bold>Settings</Text>
              <Text color="gray"> {"\u2500"} </Text>
              {SECTION_NAMES.map((name, i) => (
                <React.Fragment key={i}>
                  {i > 0 && <Text color="gray"> {"\u2502"} </Text>}
                  <Text
                    color={i === sectionIndex ? "cyan" : "gray"}
                    bold={i === sectionIndex}
                    dimColor={i !== sectionIndex}
                  >
                    {name}
                  </Text>
                </React.Fragment>
              ))}
            </Box>
            <Text color="gray">{sectionIndex + 1}/{TOTAL_SECTIONS}</Text>
          </Box>

          {/* Divider */}
          <Box paddingX={1} height={1}>
            <Text color="gray">{"\u2500".repeat(innerWidth)}</Text>
          </Box>

          {/* Section content */}
          <Box flexDirection="column" paddingX={1} flexGrow={1}>
            {sectionContent}
          </Box>

          {/* Status line */}
          <Box paddingX={1} height={1}>
            {saveStatus === "saving" && <Text color="cyan">Saving...</Text>}
            {saveStatus === "saved" && <Text color="green">Saved</Text>}
            {saveStatus === "error" && <Text color="red">{errorMsg}</Text>}
            {dirty && saveStatus === "idle" && <Text color="yellow" dimColor>Unsaved changes</Text>}
          </Box>

          {/* Navigation hints */}
          <Box paddingX={1} height={1} justifyContent="space-between">
            <Text color="gray">Esc: Close</Text>
            <Text color="gray">{hintText}</Text>
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
