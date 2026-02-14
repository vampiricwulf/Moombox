/**
 * Clipboard reading for TUI paste support.
 * Uses platform-native commands to read clipboard text.
 */

import { execFile } from "child_process";

export function readClipboard(): Promise<string> {
  return new Promise((resolve) => {
    const opts = { encoding: "utf-8" as const, timeout: 3000 };

    const cb = (err: Error | null, stdout: string) => {
      resolve(err ? "" : (stdout || "").trim());
    };

    if (process.platform === "win32") {
      execFile("powershell", ["-NoProfile", "-Command", "Get-Clipboard"], opts, cb);
    } else if (process.platform === "darwin") {
      execFile("pbpaste", [], opts, cb);
    } else {
      // Linux: try xclip, then xsel
      execFile("xclip", ["-selection", "clipboard", "-o"], opts, (err, stdout) => {
        if (err) {
          execFile("xsel", ["--clipboard", "--output"], opts, cb);
        } else {
          resolve((stdout || "").trim());
        }
      });
    }
  });
}
