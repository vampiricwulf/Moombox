import React, { useState, useCallback } from "react";
import { Box, Text, useInput } from "ink";
import { ConfigManager } from "../../core/config.js";
import { AuthService, hashPassword } from "../../core/auth.js";
import { Logger } from "../../core/logger.js";

type SecurityMode = "status" | "set" | "remove";

interface SecurityEditorProps {
  focused: boolean;
  onModeChange?: (mode: "list" | "edit") => void;
}

export function SecurityEditor({ focused, onModeChange }: SecurityEditorProps): React.ReactElement {
  const [mode, setMode] = useState<SecurityMode>("status");
  const [message, setMessage] = useState("");
  const [messageColor, setMessageColor] = useState<string>("green");

  // Set mode fields
  const [currentPw, setCurrentPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const [fieldIndex, setFieldIndex] = useState(0);

  // Remove mode field
  const [removePw, setRemovePw] = useState("");

  const hasPassword = !!ConfigManager.getInstance().get().password_hash;

  const enterMode = useCallback((m: SecurityMode) => {
    setMode(m);
    setCurrentPw("");
    setNewPw("");
    setConfirmPw("");
    setRemovePw("");
    setFieldIndex(0);
    setMessage("");
    onModeChange?.(m === "status" ? "list" : "edit");
  }, [onModeChange]);

  const showMessage = useCallback((msg: string, color: string) => {
    setMessage(msg);
    setMessageColor(color);
  }, []);

  const handleSet = useCallback(() => {
    const auth = AuthService.getInstance();
    const config = ConfigManager.getInstance().get();

    // If password exists, verify current password
    if (config.password_hash) {
      if (!currentPw) {
        showMessage("Current password required", "red");
        return;
      }
      if (!auth.verifyPassword(currentPw, config.password_hash)) {
        showMessage("Current password is incorrect", "red");
        return;
      }
    }

    if (!newPw) {
      showMessage("New password required", "red");
      return;
    }
    if (newPw !== confirmPw) {
      showMessage("Passwords do not match", "red");
      return;
    }

    // Hash and save
    const hashed = hashPassword(newPw);
    const configManager = ConfigManager.getInstance();
    const updated = { ...configManager.get(), password_hash: hashed };
    configManager.save(updated).then(() => {
      auth.invalidateAllSessions();
      Logger.getInstance().info("[Auth] Password set/changed from TUI");
      showMessage("Password set successfully", "green");
      enterMode("status");
    }).catch(() => {
      showMessage("Failed to save config", "red");
    });
  }, [currentPw, newPw, confirmPw, enterMode, showMessage]);

  const handleRemove = useCallback(() => {
    const auth = AuthService.getInstance();
    const configManager = ConfigManager.getInstance();
    const config = configManager.get();

    if (!config.password_hash) {
      showMessage("No password is set", "red");
      return;
    }

    if (!auth.verifyPassword(removePw, config.password_hash)) {
      showMessage("Current password is incorrect", "red");
      return;
    }

    // Remove password and reset to localhost if external
    const { password_hash: _, ...configWithoutHash } = config;
    const networkReset = config.network_access === "external";
    const updated = {
      ...configWithoutHash,
      ...(networkReset ? { network_access: "localhost" as const } : {}),
    };

    configManager.save(updated).then(() => {
      auth.invalidateAllSessions();
      const msg = networkReset
        ? "Password removed, network access reset to localhost"
        : "Password removed";
      Logger.getInstance().info(`[Auth] ${msg} from TUI`);
      showMessage(msg, "green");
      enterMode("status");
    }).catch(() => {
      showMessage("Failed to save config", "red");
    });
  }, [removePw, enterMode, showMessage]);

  useInput((input, key) => {
    if (!focused) return;

    // Clear messages on any key when in status mode
    if (mode === "status" && message) {
      // Don't clear immediately on the mode-trigger keys
      if (input !== "s" && input !== "S" && input !== "r" && input !== "R") {
        setMessage("");
      }
    }

    if (mode === "status") {
      if (input === "s" || input === "S") {
        enterMode("set");
        return;
      }
      if ((input === "r" || input === "R") && hasPassword) {
        enterMode("remove");
        return;
      }
      return;
    }

    if (mode === "set") {
      if (key.escape) { enterMode("status"); return; }
      if (key.return) { handleSet(); return; }

      // Determine which fields are visible
      const setFields = hasPassword
        ? ["current", "new", "confirm"] as const
        : ["new", "confirm"] as const;
      const fieldCount = setFields.length;

      if (key.upArrow || (key.tab && key.shift)) {
        setFieldIndex(prev => Math.max(0, prev - 1));
        return;
      }
      if (key.downArrow || key.tab) {
        setFieldIndex(prev => Math.min(fieldCount - 1, prev + 1));
        return;
      }

      // Text input for focused password field
      const activeField = setFields[fieldIndex];
      const setter = activeField === "current" ? setCurrentPw
        : activeField === "new" ? setNewPw
        : setConfirmPw;
      const value = activeField === "current" ? currentPw
        : activeField === "new" ? newPw
        : confirmPw;

      if (key.backspace || key.delete) {
        setter(value.slice(0, -1));
        return;
      }
      if (input && !key.ctrl && !key.meta) {
        if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
        if (input.length > 1 && input[0] === "[") return;
        setter(value + input);
      }
      return;
    }

    if (mode === "remove") {
      if (key.escape) { enterMode("status"); return; }
      if (key.return) { handleRemove(); return; }

      if (key.backspace || key.delete) {
        setRemovePw(prev => prev.slice(0, -1));
        return;
      }
      if (input && !key.ctrl && !key.meta) {
        if (input.charCodeAt(0) === 0x1b || input.charCodeAt(0) === 0x9b) return;
        if (input.length > 1 && input[0] === "[") return;
        setRemovePw(prev => prev + input);
      }
    }
  });

  // Masked display helper
  const masked = (val: string, isFocused: boolean) => (
    <>
      <Text>{"*".repeat(val.length)}</Text>
      {isFocused && <Text color="cyan">_</Text>}
    </>
  );

  if (mode === "set") {
    const setFields = hasPassword
      ? [
          { label: "Current password", value: currentPw, setter: setCurrentPw },
          { label: "New password", value: newPw, setter: setNewPw },
          { label: "Confirm password", value: confirmPw, setter: setConfirmPw },
        ]
      : [
          { label: "New password", value: newPw, setter: setNewPw },
          { label: "Confirm password", value: confirmPw, setter: setConfirmPw },
        ];

    const maxLabel = Math.max(...setFields.map(f => f.label.length));
    const padWidth = maxLabel + 2;

    return (
      <Box flexDirection="column">
        <Box height={1}>
          <Text color="cyan" bold>
            {hasPassword ? "Change Password" : "Set Password"}
          </Text>
          <Text color="gray"> (Enter: save, Esc: cancel)</Text>
        </Box>
        <Text> </Text>
        {setFields.map((field, idx) => {
          const isFocused = idx === fieldIndex;
          return (
            <Box key={field.label} height={1}>
              <Text color={isFocused ? "cyan" : "white"}>
                {isFocused ? "> " : "  "}
              </Text>
              <Text color={isFocused ? "cyan" : "white"} bold={isFocused}>
                {field.label.padEnd(padWidth)}
              </Text>
              {masked(field.value, isFocused)}
            </Box>
          );
        })}
        {message && (
          <>
            <Text> </Text>
            <Text color={messageColor}>  {message}</Text>
          </>
        )}
      </Box>
    );
  }

  if (mode === "remove") {
    const isExternal = ConfigManager.getInstance().get().network_access === "external";

    return (
      <Box flexDirection="column">
        <Box height={1}>
          <Text color="red" bold>Remove Password</Text>
          <Text color="gray"> (Enter: confirm, Esc: cancel)</Text>
        </Box>
        <Text> </Text>
        {isExternal && (
          <Box paddingLeft={2} height={1}>
            <Text color="yellow">Warning: Network access will be reset to localhost</Text>
          </Box>
        )}
        <Box height={1}>
          <Text color="cyan">{"> "}</Text>
          <Text color="cyan" bold>{"Current password".padEnd(20)}</Text>
          {masked(removePw, true)}
        </Box>
        {message && (
          <>
            <Text> </Text>
            <Text color={messageColor}>  {message}</Text>
          </>
        )}
      </Box>
    );
  }

  // Status mode
  return (
    <Box flexDirection="column">
      <Box height={1}>
        <Text>  Password: </Text>
        {hasPassword ? (
          <Text color="green" bold>Set</Text>
        ) : (
          <Text color="gray" dimColor>Not set</Text>
        )}
      </Box>
      <Text> </Text>
      <Box height={1} paddingLeft={2}>
        <Text color="gray">S: {hasPassword ? "Change" : "Set"} password</Text>
        {hasPassword && <Text color="gray">  R: Remove password</Text>}
      </Box>
      {message && (
        <>
          <Text> </Text>
          <Text color={messageColor}>  {message}</Text>
        </>
      )}
    </Box>
  );
}
