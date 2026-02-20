import React, { useState, useCallback } from "react";
import { Box, Text, useInput } from "ink";
import type { NotificationConfig } from "../../types/config.js";
import { readClipboard } from "../clipboard.js";

const ALL_EVENTS = ["found", "live", "downloading", "finished", "error", "cancelled", "auth", "added"];

function notifToValues(n: NotificationConfig): { url: string; events: Set<string> } {
  return {
    url: n.url || "",
    events: new Set(n.events || ALL_EVENTS),
  };
}

function valuesToNotif(url: string, events: Set<string>): NotificationConfig {
  const eventsArray = ALL_EVENTS.filter(e => events.has(e));
  return {
    url,
    events: eventsArray.length === ALL_EVENTS.length ? undefined : eventsArray,
  };
}

interface NotificationEditorProps {
  notifications: NotificationConfig[];
  onChange: (notifications: NotificationConfig[]) => void;
  focused: boolean;
  onModeChange?: (mode: "list" | "edit") => void;
}

export function NotificationEditor({ notifications, onChange, focused, onModeChange }: NotificationEditorProps): React.ReactElement {
  const [mode, _setMode] = useState<"list" | "edit">("list");
  const setMode = useCallback((m: "list" | "edit") => {
    _setMode(m);
    onModeChange?.(m);
  }, [onModeChange]);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const [editUrl, setEditUrl] = useState("");
  const [editEvents, setEditEvents] = useState<Set<string>>(new Set());
  const [editFocus, setEditFocus] = useState<"url" | number>("url"); // "url" or event index
  const [deleteConfirm, setDeleteConfirm] = useState(false);

  const startEdit = useCallback((index: number) => {
    const n = notifications[index];
    if (!n) return;
    const vals = notifToValues(n);
    setEditUrl(vals.url);
    setEditEvents(vals.events);
    setEditFocus("url");
    setMode("edit");
  }, [notifications]);

  const startAdd = useCallback(() => {
    setEditUrl("");
    setEditEvents(new Set(ALL_EVENTS));
    setEditFocus("url");
    setSelectedIndex(notifications.length);
    setMode("edit");
  }, [notifications.length]);

  const saveEdit = useCallback(() => {
    if (!editUrl.trim()) return;
    const notif = valuesToNotif(editUrl, editEvents);
    const newNotifs = [...notifications];
    if (selectedIndex < notifications.length) {
      newNotifs[selectedIndex] = notif;
    } else {
      newNotifs.push(notif);
    }
    onChange(newNotifs);
    setMode("list");
  }, [editUrl, editEvents, notifications, selectedIndex, onChange]);

  const deleteNotif = useCallback(() => {
    if (selectedIndex >= notifications.length) return;
    const newNotifs = notifications.filter((_, i) => i !== selectedIndex);
    onChange(newNotifs);
    setSelectedIndex(prev => Math.min(prev, Math.max(0, newNotifs.length - 1)));
    setDeleteConfirm(false);
  }, [notifications, selectedIndex, onChange]);

  useInput((input, key) => {
    if (!focused) return;

    if (mode === "list") {
      if (deleteConfirm) {
        if (input === "d" || input === "D") {
          deleteNotif();
        } else {
          setDeleteConfirm(false);
        }
        return;
      }

      if (key.escape) return; // Handled by parent
      if (key.upArrow) {
        setSelectedIndex(prev => Math.max(0, prev - 1));
      } else if (key.downArrow) {
        setSelectedIndex(prev => Math.min(notifications.length - 1, prev + 1));
      } else if (key.return && notifications.length > 0) {
        startEdit(selectedIndex);
      } else if (input === "a" || input === "A") {
        startAdd();
      } else if ((input === "d" || input === "D") && notifications.length > 0) {
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

    // Navigate between URL field and event checkboxes
    const totalEditItems = 1 + ALL_EVENTS.length; // URL + events
    const currentEditIdx = editFocus === "url" ? 0 : (editFocus as number) + 1;

    if (key.upArrow) {
      const newIdx = Math.max(0, currentEditIdx - 1);
      setEditFocus(newIdx === 0 ? "url" : newIdx - 1);
      return;
    }
    if (key.downArrow) {
      const newIdx = Math.min(totalEditItems - 1, currentEditIdx + 1);
      setEditFocus(newIdx === 0 ? "url" : newIdx - 1);
      return;
    }

    // URL field text editing
    if (editFocus === "url") {
      if (key.backspace || key.delete) {
        setEditUrl(prev => prev.slice(0, -1));
        return;
      }
      if (key.ctrl && input === "v") {
        readClipboard().then(text => {
          if (text) {
            const firstLine = text.split(/[\r\n]/)[0].trim();
            if (firstLine) setEditUrl(prev => prev + firstLine);
          }
        });
        return;
      }
      if (input && !key.ctrl && !key.meta) {
        if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
        if (input.length > 1 && input[0] === "[") return;
        setEditUrl(prev => prev + input);
      }
      return;
    }

    // Event toggle (Space or Enter toggles)
    if (typeof editFocus === "number") {
      if (input === " ") {
        const eventName = ALL_EVENTS[editFocus];
        setEditEvents(prev => {
          const next = new Set(prev);
          if (next.has(eventName)) next.delete(eventName);
          else next.add(eventName);
          return next;
        });
      }
    }
  });

  if (mode === "edit") {
    return (
      <Box flexDirection="column">
        <Box height={1}>
          <Text color="cyan" bold>
            {selectedIndex < notifications.length ? "Edit Notification" : "Add Notification"}
          </Text>
          <Text color="gray"> (Enter: save, Esc: cancel)</Text>
        </Box>
        {/* URL field */}
        <Box height={1}>
          <Text color={editFocus === "url" ? "cyan" : "white"}>
            {editFocus === "url" ? "> " : "  "}
          </Text>
          <Text color={editFocus === "url" ? "cyan" : "white"} bold={editFocus === "url"}>
            {"Webhook URL".padEnd(16)}
          </Text>
          <Text>{editUrl}</Text>
          {editFocus === "url" && <Text color="cyan">_</Text>}
        </Box>
        {/* Events */}
        <Box height={1} marginTop={1}>
          <Text color="gray">  Events (Space to toggle):</Text>
        </Box>
        {ALL_EVENTS.map((event, idx) => {
          const isFocused = editFocus === idx;
          const isChecked = editEvents.has(event);
          return (
            <Box key={event} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={isChecked ? "green" : "gray"}>
                [{isChecked ? "x" : " "}]
              </Text>
              <Text color={isFocused ? "cyan" : "white"}> {event}</Text>
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
      {notifications.length === 0 ? (
        <Box paddingLeft={2} height={1}>
          <Text color="gray" dimColor>No webhooks configured. Press A to add one.</Text>
        </Box>
      ) : (
        notifications.map((n, idx) => {
          const isFocused = idx === selectedIndex;
          const eventCount = n.events ? n.events.length : ALL_EVENTS.length;
          const urlDisplay = (n.url || "").slice(0, 50);

          return (
            <Box key={idx} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={isFocused ? "cyan" : "white"}>
                {urlDisplay}
              </Text>
              <Text color="gray" dimColor>
                {" "}({eventCount}/{ALL_EVENTS.length} events)
              </Text>
            </Box>
          );
        })
      )}
    </Box>
  );
}
