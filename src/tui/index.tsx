import React from "react";
import { execaSync } from "execa";
import fs from "fs";
import os from "os";
import path from "path";
import { Database } from "../core/database.js";
import { Logger } from "../core/logger.js";
import { ErrorBoundary } from "./components/ErrorBoundary.js";

let cleanup: (() => void) | null = null;

/**
 * On Windows, add ENABLE_VIRTUAL_TERMINAL_INPUT (0x0200) to the console input mode.
 *
 * Without this flag, ConPTY (Windows Terminal) translates incoming VT mouse
 * sequences into MOUSE_EVENT_RECORDs. Node.js/libuv only processes KEY_EVENT
 * records and silently discards MOUSE_EVENT records, so mouse clicks and scrolls
 * never reach the application.
 *
 * With this flag, ConPTY passes VT sequences through as raw characters in
 * KEY_EVENT records, which libuv reads normally and our useMouse hook can parse.
 *
 * Must be called AFTER Ink's render() because render() calls setRawMode(true),
 * which resets the console mode to just ENABLE_WINDOW_INPUT.
 */
function enableVTInput(): void {
  if (process.platform !== "win32" || !process.stdin.isTTY) return;

  try {
    const helperDir = path.join(os.homedir(), ".moombox");
    if (!fs.existsSync(helperDir)) fs.mkdirSync(helperDir, { recursive: true });
    const helperExe = path.join(helperDir, "moombox-vtinput.exe");

    if (!fs.existsSync(helperExe)) {
      // Compile a tiny native helper using .NET Framework's csc.exe (always present on Win10/11)
      const cscExe = path.join(
        process.env.WINDIR || "C:\\Windows",
        "Microsoft.NET", "Framework64", "v4.0.30319", "csc.exe",
      );
      if (!fs.existsSync(cscExe)) return;

      const csFile = helperExe.replace(".exe", ".cs");
      fs.writeFileSync(csFile, [
        "using System;using System.Runtime.InteropServices;",
        "class P{",
        '[DllImport("kernel32")]static extern bool SetConsoleMode(IntPtr h,uint m);',
        '[DllImport("kernel32")]static extern IntPtr GetStdHandle(int n);',
        '[DllImport("kernel32")]static extern bool GetConsoleMode(IntPtr h,out uint m);',
        "static void Main(){var h=GetStdHandle(-10);uint m;GetConsoleMode(h,out m);SetConsoleMode(h,m|0x200);}}",
      ].join(""));
      execaSync(cscExe, ["/nologo", "/optimize", `/out:${helperExe}`, csFile], {
        stdio: "ignore", timeout: 10_000,
      });
      try { fs.unlinkSync(csFile); } catch {}
    }

    if (fs.existsSync(helperExe)) {
      execaSync(helperExe, [], { stdio: ["inherit", "ignore", "ignore"], timeout: 5_000 });
    }
  } catch {
    // Non-fatal — mouse won't work but keyboard and TUI still function
  }
}

export async function startTUI(): Promise<void> {
  // Initialize yoga-layout before ink loads.
  // In bundled build: yoga-layout resolves to our Proxy shim, __initYoga exists.
  // In dev mode: yoga-layout self-initializes via top-level await, __initYoga doesn't exist.
  const yogaModule = (await import("yoga-layout")) as any;
  if (typeof yogaModule.__initYoga === "function") {
    const { loadYoga } = await import("yoga-layout/load");
    const Yoga = await loadYoga();
    yogaModule.__initYoga(Yoga);
  }

  // Dynamic imports — must happen AFTER yoga init
  const React = (await import("react")).default;
  const { render } = await import("ink");
  const { App } = await import("./App.js");

  const db = await Database.getInstance();
  const logger = Logger.getInstance();

  // Enable raw mode and mouse tracking BEFORE React renders.
  // Raw mode disables QuickEdit/Mark mode on Windows.
  if (process.stdin.isTTY) {
    process.stdin.setRawMode(true);
    process.stdout.write("\x1b[?1000h\x1b[?1002h\x1b[?1006h");
  }

  // Install stdin filter to strip mouse escape sequences before Ink sees them.
  // Must be called before render() so the patched emit is in place when Ink starts listening.
  const { installStdinMouseFilter } = await import("./stdinFilter.js");
  installStdinMouseFilter();

  const { unmount, waitUntilExit } = render(
    React.createElement(
      ErrorBoundary,
      null,
      React.createElement(App, { db, logger }),
    ),
  );

  // Enable VT input AFTER render() — Ink's render calls setRawMode(true) which
  // resets console mode to ENABLE_WINDOW_INPUT only, so we must add our flag after.
  enableVTInput();

  cleanup = () => {
    // Disable mouse tracking and restore terminal state
    if (process.stdout.isTTY) {
      process.stdout.write("\x1b[?1000l\x1b[?1002l\x1b[?1006l");
    }
    unmount();
  };

  process.on("SIGINT", () => {
    if (cleanup) cleanup();
    process.exit(0);
  });

  await waitUntilExit();
}

export function stopTUI(): void {
  if (cleanup) {
    cleanup();
    cleanup = null;
  }
}
