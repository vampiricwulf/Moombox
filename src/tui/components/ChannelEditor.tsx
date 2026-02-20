import React, { useState, useCallback } from "react";
import { Box, Text, useInput } from "ink";
import type { ChannelConfig } from "../../types/config.js";
import { readClipboard } from "../clipboard.js";

type ChannelFieldKey = "id" | "name" | "platform" | "enabled" | "terms" | "include_non_live_content" | "quality_preference";

interface ChannelFieldDef {
  key: ChannelFieldKey;
  label: string;
  type: "text" | "toggle" | "cycle";
  options?: string[];
  help?: string;
  /** Only show this field for certain platforms */
  platformFilter?: "youtube" | "twitch";
}

const CHANNEL_FIELDS: ChannelFieldDef[] = [
  { key: "id", label: "Channel ID", type: "text", help: "e.g. UC... or twitch login" },
  { key: "name", label: "Display name", type: "text" },
  { key: "platform", label: "Platform", type: "cycle", options: ["youtube", "twitch"] },
  { key: "enabled", label: "Enabled", type: "toggle", options: ["Yes", "No"] },
  { key: "terms", label: "Filter regex", type: "text", help: "e.g. (?i)karaoke" },
  { key: "include_non_live_content", label: "Include non-live", type: "toggle", options: ["No", "Yes"], platformFilter: "youtube" },
  { key: "quality_preference", label: "Quality preference", type: "cycle", options: ["best", "720p", "480p", "audio_only"], platformFilter: "twitch" },
];

function channelToValues(ch: ChannelConfig): Record<string, string> {
  const terms = typeof ch.terms === "string" ? ch.terms : ch.terms ? Object.values(ch.terms).join("|") : "";
  return {
    id: ch.id || "",
    name: ch.name || "",
    platform: ch.platform || "youtube",
    enabled: ch.enabled !== false ? "Yes" : "No",
    terms,
    include_non_live_content: ch.include_non_live_content ? "Yes" : "No",
    quality_preference: ch.quality_preference || "best",
  };
}

function valuesToChannel(vals: Record<string, string>): ChannelConfig {
  const ch: ChannelConfig = {
    id: vals.id,
    name: vals.name || undefined,
    platform: vals.platform as "youtube" | "twitch",
    enabled: vals.enabled === "Yes",
    terms: vals.terms || undefined,
  };
  if (vals.platform === "youtube") {
    ch.include_non_live_content = vals.include_non_live_content === "Yes" ? true : undefined;
  }
  if (vals.platform === "twitch") {
    ch.quality_preference = (vals.quality_preference as ChannelConfig["quality_preference"]) || undefined;
  }
  return ch;
}

interface ChannelEditorProps {
  channels: ChannelConfig[];
  onChange: (channels: ChannelConfig[]) => void;
  focused: boolean;
  onModeChange?: (mode: "list" | "edit") => void;
}

export function ChannelEditor({ channels, onChange, focused, onModeChange }: ChannelEditorProps): React.ReactElement {
  const [mode, _setMode] = useState<"list" | "edit">("list");
  const setMode = useCallback((m: "list" | "edit") => {
    _setMode(m);
    onModeChange?.(m);
  }, [onModeChange]);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const [editValues, setEditValues] = useState<Record<string, string>>({});
  const [editField, setEditField] = useState(0);
  const [deleteConfirm, setDeleteConfirm] = useState(false);

  const visibleFields = CHANNEL_FIELDS.filter(f => {
    if (!f.platformFilter) return true;
    return f.platformFilter === (editValues.platform || "youtube");
  });

  const setEditValue = useCallback((key: string, val: string) => {
    setEditValues(prev => ({ ...prev, [key]: val }));
  }, []);

  const cycleEditOption = useCallback((field: ChannelFieldDef, direction: number) => {
    if (!field.options) return;
    const current = editValues[field.key] || field.options[0];
    const idx = field.options.indexOf(current);
    const next = (idx + direction + field.options.length) % field.options.length;
    setEditValue(field.key, field.options[next]);
  }, [editValues, setEditValue]);

  const startEdit = useCallback((index: number) => {
    const ch = channels[index];
    if (!ch) return;
    setEditValues(channelToValues(ch));
    setEditField(0);
    setMode("edit");
  }, [channels]);

  const startAdd = useCallback(() => {
    setEditValues({
      id: "", name: "", platform: "youtube",
      enabled: "Yes", terms: "",
      include_non_live_content: "No",
      quality_preference: "best",
    });
    setEditField(0);
    setSelectedIndex(channels.length); // Will be the new index
    setMode("edit");
  }, [channels.length]);

  const saveEdit = useCallback(() => {
    if (!editValues.id?.trim()) return; // ID required
    const ch = valuesToChannel(editValues);
    const newChannels = [...channels];
    if (selectedIndex < channels.length) {
      newChannels[selectedIndex] = ch;
    } else {
      newChannels.push(ch);
    }
    onChange(newChannels);
    setMode("list");
  }, [editValues, channels, selectedIndex, onChange]);

  const deleteChannel = useCallback(() => {
    if (selectedIndex >= channels.length) return;
    const newChannels = channels.filter((_, i) => i !== selectedIndex);
    onChange(newChannels);
    setSelectedIndex(prev => Math.min(prev, Math.max(0, newChannels.length - 1)));
    setDeleteConfirm(false);
  }, [channels, selectedIndex, onChange]);

  useInput((input, key) => {
    if (!focused) return;

    if (mode === "list") {
      if (deleteConfirm) {
        if (input === "d" || input === "D") {
          deleteChannel();
        } else {
          setDeleteConfirm(false);
        }
        return;
      }

      if (key.escape) return; // Handled by parent
      if (key.upArrow) {
        setSelectedIndex(prev => Math.max(0, prev - 1));
      } else if (key.downArrow) {
        setSelectedIndex(prev => Math.min(channels.length - 1, prev + 1));
      } else if (key.return && channels.length > 0) {
        startEdit(selectedIndex);
      } else if (input === "a" || input === "A") {
        startAdd();
      } else if ((input === "d" || input === "D") && channels.length > 0) {
        setDeleteConfirm(true);
      }
      return;
    }

    // Edit mode
    if (key.escape) {
      setMode("list");
      return;
    }

    if (key.return) {
      saveEdit();
      return;
    }

    const field = visibleFields[editField];
    if (!field) return;

    if (key.upArrow) {
      setEditField(prev => Math.max(0, prev - 1));
      return;
    }
    if (key.downArrow) {
      setEditField(prev => Math.min(visibleFields.length - 1, prev + 1));
      return;
    }

    if (field.type === "toggle" || field.type === "cycle") {
      if (key.leftArrow) { cycleEditOption(field, -1); return; }
      if (key.rightArrow) { cycleEditOption(field, 1); return; }
      return;
    }

    // Text editing
    const currentVal = editValues[field.key] || "";
    if (key.backspace || key.delete) {
      setEditValue(field.key, currentVal.slice(0, -1));
      return;
    }

    if (key.ctrl && input === "v") {
      readClipboard().then(text => {
        if (text) {
          const firstLine = text.split(/[\r\n]/)[0].trim();
          if (firstLine) setEditValue(field.key, currentVal + firstLine);
        }
      });
      return;
    }

    if (input && !key.ctrl && !key.meta) {
      if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
      if (input.length > 1 && input[0] === "[") return;
      setEditValue(field.key, currentVal + input);
    }
  });

  if (mode === "edit") {
    const maxLabel = Math.max(...visibleFields.map(f => f.label.length));
    const padWidth = maxLabel + 2;

    return (
      <Box flexDirection="column">
        <Box height={1}>
          <Text color="cyan" bold>
            {selectedIndex < channels.length ? "Edit Channel" : "Add Channel"}
          </Text>
          <Text color="gray"> (Enter: save, Esc: cancel)</Text>
        </Box>
        {visibleFields.map((field, idx) => {
          const isFocused = idx === editField;
          const val = editValues[field.key] || "";
          const isToggleOrCycle = field.type === "toggle" || field.type === "cycle";

          return (
            <Box key={field.key} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={isFocused ? "cyan" : "white"} bold={isFocused}>
                {field.label.padEnd(padWidth)}
              </Text>
              {isToggleOrCycle ? (
                <OptionSelector options={field.options!} selected={val || field.options![0]} focused={isFocused} />
              ) : (
                <>
                  <Text>{val}</Text>
                  {isFocused && <Text color="cyan">_</Text>}
                </>
              )}
              {field.help && isFocused && (
                <Text color="gray" dimColor> ({field.help})</Text>
              )}
            </Box>
          );
        })}
      </Box>
    );
  }

  // List mode
  return (
    <Box flexDirection="column">
      <Box height={1}>
        <Text color="gray">A: Add  Enter: Edit  D: Delete  </Text>
        {deleteConfirm && <Text color="yellow">Press D again to confirm delete</Text>}
      </Box>
      {channels.length === 0 ? (
        <Box paddingLeft={2} height={1}>
          <Text color="gray" dimColor>No channels configured. Press A to add one.</Text>
        </Box>
      ) : (
        channels.map((ch, idx) => {
          const isFocused = idx === selectedIndex;
          const platformIcon = ch.platform === "twitch" ? "TW" : "YT";
          const platformColor = ch.platform === "twitch" ? "magenta" : "red";
          const enabled = ch.enabled !== false;
          const terms = typeof ch.terms === "string" ? ch.terms : "";

          return (
            <Box key={idx} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={platformColor}>[{platformIcon}]</Text>
              <Text> </Text>
              <Text color={enabled ? (isFocused ? "cyan" : "white") : "gray"} dimColor={!enabled}>
                {(ch.name || ch.id).slice(0, 20).padEnd(22)}
              </Text>
              <Text color="gray">{ch.id.slice(0, 24)}</Text>
              {!enabled && <Text color="yellow" dimColor> (disabled)</Text>}
              {terms && <Text color="gray" dimColor> filter: {terms.slice(0, 20)}</Text>}
            </Box>
          );
        })
      )}
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
