/**
 * Config, setup, cookies, yt-dlp, logs, and status endpoints.
 */

import type { Router } from "express";
import fs from "fs-extra";
import { Logger } from "../../core/logger.js";
import { ConfigManager } from "../../core/config.js";
import { CookieRefreshService } from "../../core/cookieRefresh.js";
import { CookieJar } from "../../core/cookies.js";
import { AutoCookieService } from "../../core/autoCookies.js";
import { getYtdlpPluginStatus, installYtdlpPlugin } from "../../core/ytdlpPlugin.js";
import { asyncHandler } from "./errorHandler.js";

export interface ConfigRoutesContext {
  logBuffer: string[];
}

export function registerConfigRoutes(router: Router, ctx: ConfigRoutesContext): void {
  const logger = Logger.getInstance();

  // Get recent logs
  router.get("/logs", (req, res) => {
    res.json(ctx.logBuffer);
  });

  // Get status
  router.get("/status", asyncHandler(async (req, res) => {
    const config = ConfigManager.getInstance().get();

    // Check cookie status by directly loading and parsing the cookie file
    let cookieStatus = { found: false, authenticated: false };
    try {
      const cookieFile = config.downloader?.cookie_file;
      if (cookieFile && fs.existsSync(cookieFile)) {
        cookieStatus.found = true;
        // Load cookies fresh to check authentication
        await CookieJar.load(cookieFile, true);
        cookieStatus.authenticated = CookieJar.hasAuthCookies();
      }
    } catch (e) {
      // Ignore errors
    }

    // Twitch auth status
    const twitchAuthStatus = { authenticated: CookieJar.hasTwitchAuthCookies() };

    // Auto-cookie re-login state
    const autoCookieReloginRequired = AutoCookieService.getInstance().needsManualRelogin;

    res.json({
      status: "running",
      uptime: process.uptime(),
      timestamp: new Date().toISOString(),
      cookieStatus,
      twitchAuthStatus,
      autoCookieReloginRequired,
    });
  }));

  // Get config
  router.get("/config", (req, res) => {
    const config = ConfigManager.getInstance().get();
    res.json(config);
  });

  // Update config
  router.put("/config", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();
    const newConfig = req.body;

    // Validate required fields
    if (!newConfig.downloader || typeof newConfig.downloader !== "object") {
      return res
        .status(400)
        .json({ error: "downloader configuration is required" });
    }

    // Validate known top-level keys only
    const allowedKeys = new Set([
      "port", "network_access",
      "log_level", "log_file_path", "log_max_file_size", "log_max_files",
      "database_path", "max_feed_items", "feed_check_interval",
      "downloader", "tasklist",
      "auto_cookies", "notifications", "channels",
    ]);
    for (const key of Object.keys(newConfig)) {
      if (!allowedKeys.has(key)) {
        return res.status(400).json({ error: `Unknown config key: ${key}` });
      }
    }

    await configManager.save(newConfig);
    // Apply runtime-reloadable settings immediately
    logger.refreshLogLevel();
    res.json({ success: true });
  }));

  // Add channel
  router.post("/config/channels", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();
    const config = configManager.get();
    const channel = req.body;

    if (!channel.id) {
      return res.status(400).json({ error: "Channel ID is required" });
    }

    if (!config.channels) {
      config.channels = [];
    }

    // Check if channel already exists
    const existingIndex = config.channels.findIndex(
      (c) => c.id === channel.id,
    );
    if (existingIndex >= 0) {
      config.channels[existingIndex] = channel;
    } else {
      config.channels.push(channel);
    }

    await configManager.save(config);
    res.json({ success: true, channel });
  }));

  // Delete channel
  router.delete("/config/channels/:id", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();
    const config = configManager.get();
    const channelId = req.params.id as string;

    if (!config.channels) {
      return res.status(404).json({ error: "Channel not found" });
    }

    const index = config.channels.findIndex((c) => c.id === channelId);
    if (index < 0) {
      return res.status(404).json({ error: "Channel not found" });
    }

    config.channels.splice(index, 1);
    await configManager.save(config);
    res.json({ success: true });
  }));

  // Check if first run (no config exists)
  router.get("/setup/status", (req, res) => {
    const configManager = ConfigManager.getInstance();
    const isFirstRun = !configManager.hasConfig();
    res.json({ isFirstRun });
  });

  // Complete first-run setup
  router.post("/setup/complete", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();
    const config = req.body;

    // Validate required fields
    if (!config.downloader) {
      return res
        .status(400)
        .json({ error: "downloader configuration is required" });
    }

    // Save the configuration
    await configManager.save(config);

    logger.info("[Setup] First-run setup completed");
    res.json({ success: true });
  }));

  // Recheck cookies
  router.post("/cookies/recheck", asyncHandler(async (req, res) => {
    const cookieService = CookieRefreshService.getInstance();
    const success = await cookieService.refresh();

    // Get updated status by directly loading and parsing the cookie file
    const config = ConfigManager.getInstance().get();
    let cookieStatus = { found: false, authenticated: false };

    const cookieFile = config.downloader?.cookie_file;
    if (cookieFile && fs.existsSync(cookieFile)) {
      cookieStatus.found = true;
      // Load cookies fresh to check authentication
      await CookieJar.load(cookieFile, true);
      cookieStatus.authenticated = CookieJar.hasAuthCookies();
    }

    const twitchAuthStatus = { authenticated: CookieJar.hasTwitchAuthCookies() };
    const autoCookieReloginRequired = AutoCookieService.getInstance().needsManualRelogin;

    res.json({
      success,
      cookieStatus,
      twitchAuthStatus,
      autoCookieReloginRequired,
    });
  }));

  // ===== Auto Cookie Endpoints =====

  // Get auto-cookie status
  router.get("/cookies/auto-status", (req, res) => {
    res.json(AutoCookieService.getInstance().getStatus());
  });

  // Start auto-cookie setup (launches browser)
  router.post("/cookies/auto-setup/start", asyncHandler(async (req, res) => {
    const platform = req.body?.platform as "youtube" | "twitch" | undefined;
    const result = await AutoCookieService.getInstance().startSetup(platform);
    res.json(result);
  }));

  // Finish auto-cookie setup (extract cookies)
  router.post("/cookies/auto-setup/finish", asyncHandler(async (req, res) => {
    const result = await AutoCookieService.getInstance().finishSetup();
    res.json(result);
  }));

  // Navigate browser to Twitch login during setup
  router.post("/cookies/auto-setup/navigate-twitch", asyncHandler(async (req, res) => {
    const result = await AutoCookieService.getInstance().navigateToTwitch();
    res.json(result);
  }));

  // Cancel auto-cookie setup
  router.post("/cookies/auto-setup/cancel", asyncHandler(async (req, res) => {
    await AutoCookieService.getInstance().cancelSetup();
    res.json({ success: true });
  }));

  // ===== yt-dlp Plugin Endpoints =====

  // Get yt-dlp plugin install status
  router.get("/ytdlp-plugin/status", (req, res) => {
    const config = ConfigManager.getInstance().get();
    const currentPort = config.port || 774;
    res.json(getYtdlpPluginStatus(currentPort));
  });

  // Install yt-dlp plugin
  router.post("/ytdlp-plugin/install", asyncHandler(async (req, res) => {
    const body = req.body || {};
    const force = body.force === true;
    const config = ConfigManager.getInstance().get();
    const currentPort = config.port || 774;

    const result = await installYtdlpPlugin(currentPort, force);

    if (!result.success) {
      return res.status(500).json({ error: result.error });
    }

    if (!result.alreadyInstalled) {
      logger.info(
        `[yt-dlp Plugin] Installed to ${result.path} (port ${currentPort})`,
      );
    }

    res.json(result);
  }));
}
