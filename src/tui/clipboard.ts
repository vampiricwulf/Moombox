/**
 * Clipboard reading for TUI paste support.
 * Uses platform-native commands to read clipboard text.
 */

import { execa } from "execa";

export async function readClipboard(): Promise<string> {
  try {
    let result: { stdout: string };

    if (process.platform === "win32") {
      result = await execa("powershell", ["-NoProfile", "-Command", "Get-Clipboard"], {
        timeout: 3_000,
      });
    } else if (process.platform === "darwin") {
      result = await execa("pbpaste", [], {
        timeout: 3_000,
      });
    } else {
      // Linux: try xclip, then xsel
      try {
        result = await execa("xclip", ["-selection", "clipboard", "-o"], {
          timeout: 3_000,
        });
      } catch (err) {
        result = await execa("xsel", ["--clipboard", "--output"], {
          timeout: 3_000,
        });
      }
    }

    return (result.stdout || "").trim();
  } catch (error) {
    return "";
  }
}
