/**
 * yt-dlp Plugin Management
 *
 * Shared logic for checking status and installing the Moombox yt-dlp
 * PO token provider plugin. Used by both the web API and TUI setup.
 */

import path from "path";
import fs from "fs-extra";
import { env } from "./env.js";

export interface YtdlpPluginStatus {
  installed: boolean;
  pluginDir: string;
  currentPort: number;
  installedPort: number | null;
  portMismatch: boolean;
  extractedPath: string;
}

export interface YtdlpPluginInstallResult {
  success: boolean;
  alreadyInstalled?: boolean;
  path?: string;
  error?: string;
}

/** Determine yt-dlp's default plugin directory for the current platform. */
function getPluginDir(): string {
  if (process.platform === "win32") {
    return path.join(env.APPDATA || "", "yt-dlp", "plugins");
  }
  return path.join(env.HOME || "~", ".config", "yt-dlp", "plugins");
}

/** Path where the installed plugin file lives inside the yt-dlp plugin dir. */
function getInstalledPluginPath(pluginDir: string): string {
  return path.join(
    pluginDir,
    "moombox",
    "yt_dlp_plugins",
    "extractor",
    "getpot_moombox.py",
  );
}

/** Determine where the plugin source file can be read from. */
function getPluginSourcePath(): string | null {
  // SEA mode: extracted assets
  const assetsDir = env.MOOMBOX_ASSETS_DIR;
  if (assetsDir) {
    const p = path.join(
      assetsDir,
      "yt-dlp-plugin",
      "moombox",
      "yt_dlp_plugins",
      "extractor",
      "getpot_moombox.py",
    );
    if (fs.existsSync(p)) return p;
  }

  // Development mode: source tree
  const srcPath = path.join(
    process.cwd(),
    "src",
    "pot-plugin",
    "yt_dlp_plugins",
    "extractor",
    "getpot_moombox.py",
  );
  if (fs.existsSync(srcPath)) return srcPath;

  return null;
}

/** Parse the DEFAULT_BASE_URL port out of a plugin file's contents. */
function parseInstalledPort(content: string): number | null {
  const match = content.match(
    /DEFAULT_BASE_URL\s*=\s*['"]https?:\/\/[^:]+:(\d+)['"]/,
  );
  return match ? parseInt(match[1], 10) : null;
}

/** Get the extracted plugin path for display / --plugin-dirs usage. */
function getExtractedPath(): string {
  const assetsDir = env.MOOMBOX_ASSETS_DIR;
  return assetsDir
    ? path.join(assetsDir, "yt-dlp-plugin")
    : path.join(process.cwd(), "src", "pot-plugin");
}

/**
 * Check the current install status of the yt-dlp plugin.
 */
export function getYtdlpPluginStatus(currentPort: number): YtdlpPluginStatus {
  const pluginDir = getPluginDir();
  const installedPath = getInstalledPluginPath(pluginDir);

  let installed = false;
  let installedPort: number | null = null;
  let portMismatch = false;

  if (fs.existsSync(installedPath)) {
    installed = true;
    try {
      const content = fs.readFileSync(installedPath, "utf-8");
      installedPort = parseInstalledPort(content);
      if (installedPort !== null) {
        portMismatch = installedPort !== currentPort;
      }
    } catch {
      // Ignore read errors
    }
  }

  return {
    installed,
    pluginDir,
    currentPort,
    installedPort,
    portMismatch,
    extractedPath: getExtractedPath(),
  };
}

/**
 * Install (or update) the yt-dlp plugin to the platform's plugin directory.
 *
 * Replaces the DEFAULT_BASE_URL port with `currentPort`.
 * If `force` is false and the plugin is already installed with the right port,
 * returns early with `alreadyInstalled: true`.
 */
export async function installYtdlpPlugin(
  currentPort: number,
  force = false,
): Promise<YtdlpPluginInstallResult> {
  const pluginDir = getPluginDir();
  const destPath = getInstalledPluginPath(pluginDir);

  // Check if already installed with correct port
  if (!force && fs.existsSync(destPath)) {
    try {
      const existing = fs.readFileSync(destPath, "utf-8");
      const existingPort = parseInstalledPort(existing);
      if (existingPort === currentPort) {
        return { success: true, alreadyInstalled: true, path: destPath };
      }
    } catch {
      // Continue to install
    }
  }

  // Find the plugin source
  const sourcePath = getPluginSourcePath();
  if (!sourcePath) {
    return {
      success: false,
      error:
        "Plugin source not found. Are you running from the packaged executable?",
    };
  }

  let pluginSource = fs.readFileSync(sourcePath, "utf-8");

  // Replace DEFAULT_BASE_URL with current port
  pluginSource = pluginSource.replace(
    /DEFAULT_BASE_URL\s*=\s*['"]http:\/\/127\.0\.0\.1:\d+['"]/,
    `DEFAULT_BASE_URL = 'http://127.0.0.1:${currentPort}'`,
  );

  // Write to yt-dlp plugin directory
  const destDir = path.dirname(destPath);
  await fs.ensureDir(destDir);
  fs.writeFileSync(destPath, pluginSource, "utf-8");

  return { success: true, path: destPath };
}
