/**
 * Config, setup, cookies, yt-dlp, logs, and status endpoints.
 */

import type { Router } from "express";
import { Logger } from "../../core/logger.js";
import { ConfigManager } from "../../core/config.js";
import { CookieRefreshService } from "../../core/cookieRefresh.js";
import { AutoCookieService } from "../../core/autoCookies.js";
import { getYtdlpPluginStatus, installYtdlpPlugin } from "../../core/ytdlpPlugin.js";
import { asyncHandler } from "./errorHandler.js";
import { updateConfigSchema, addChannelSchema, formatValidationErrors } from "../../types/schemas.js";
import { getErrorMessage } from "../../types/errors.js";
import { AuthService } from "../../core/auth.js";
import { DownloadWorker } from "../../core/worker/index.js";
import { isLoopback } from "../../utils/ipValidation.js";

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
    const validation = CookieRefreshService.getInstance().validationState;
    const autoCookieReloginRequired = AutoCookieService.getInstance().needsManualRelogin;

    res.json({
      status: "running",
      uptime: process.uptime(),
      timestamp: new Date().toISOString(),
      cookieStatus: {
        found: validation.youtube.hasCookies,
        authenticated: validation.youtube.authenticated,
      },
      twitchAuthStatus: {
        authenticated: validation.twitch.authenticated,
      },
      autoCookieReloginRequired,
    });
  }));

  // Get config (strip password_hash, add hasPassword boolean)
  router.get("/config", (req, res) => {
    const { password_hash, ...safeConfig } = ConfigManager.getInstance().get();
    res.json({ ...safeConfig, hasPassword: !!password_hash });
  });

  // Update config
  router.put("/config", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();

    // Validate with Zod schema
    const validation = updateConfigSchema.safeParse(req.body);
    if (!validation.success) {
      const details = formatValidationErrors(validation.error);
      logger.warn(`[Config] PUT /config validation failed: ${JSON.stringify(details)}`);
      return res.status(400).json({
        error: "Validation failed",
        details,
      });
    }

    // Prevent enabling external access without a password
    if (validation.data.network_access === "external") {
      const existing = configManager.get();
      if (!existing.password_hash) {
        return res.status(400).json({
          error: "A password must be set before enabling external access. Go to Settings \u2192 Security.",
        });
      }
    }

    // Never allow password_hash via config update (use /auth/set-password)
    delete (validation.data as Record<string, unknown>).password_hash;

    // Preserve existing password_hash (managed via /auth endpoints, not config UI)
    const existing = configManager.get();
    const configToSave = { ...validation.data } as Record<string, unknown>;
    if (existing.password_hash) {
      configToSave.password_hash = existing.password_hash;
    }

    await configManager.save(configToSave as any);
    // Apply runtime-reloadable settings immediately
    logger.refreshLogLevel();
    // Hot-reload num_parallel_downloads
    const newParallel = configManager.get().downloader?.num_parallel_downloads;
    if (newParallel != null) {
      DownloadWorker.getInstance().setMaxDownloadSlots(newParallel);
    }
    res.json({ success: true });
  }));

  // Restart process (loopback-only)
  router.post("/restart", asyncHandler(async (req, res) => {
    const ip = (req.ip || req.socket.remoteAddress || "").replace(/^::ffff:/, "");
    if (!isLoopback(ip)) {
      return res.status(403).json({ error: "Restart is only available from localhost" });
    }
    res.json({ success: true, message: "Restarting..." });
    // Delay slightly so the response gets sent before we exit
    setTimeout(async () => {
      try {
        const { restart } = await import("../../index.js");
        await restart();
      } catch (e: unknown) {
        logger.error(`[Config] Restart failed: ${getErrorMessage(e)}`);
      }
    }, 500);
  }));

  // Add channel
  router.post("/config/channels", asyncHandler(async (req, res) => {
    const configManager = ConfigManager.getInstance();

    // Validate with Zod schema
    const validation = addChannelSchema.safeParse(req.body);
    if (!validation.success) {
      return res.status(400).json({
        error: "Validation failed",
        details: formatValidationErrors(validation.error),
      });
    }

    const channel = validation.data;
    const config = { ...configManager.get() };
    const channels = [...(config.channels || [])];

    // Check if channel already exists
    const existingIndex = channels.findIndex((c) => c.id === channel.id);
    if (existingIndex >= 0) {
      channels[existingIndex] = channel;
    } else {
      channels.push(channel);
    }

    config.channels = channels;
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

    // Extract password before Zod validation (not part of config schema)
    const password = typeof req.body?.password === "string" ? req.body.password : undefined;

    // Validate with Zod schema (same rules as config update)
    const validation = updateConfigSchema.safeParse(req.body);
    if (!validation.success) {
      return res.status(400).json({
        error: "Validation failed",
        details: formatValidationErrors(validation.error),
      });
    }

    // If external access is selected, require a password
    if (validation.data.network_access === "external") {
      if (!password || password.length < 8) {
        return res.status(400).json({
          error: "A password (min 8 characters) is required for external access.",
        });
      }
    }

    // Hash password if provided and include in config
    const configData: Record<string, unknown> = { ...validation.data };
    if (password && password.length >= 8) {
      const auth = AuthService.getInstance();
      configData.password_hash = auth.hashPassword(password);
    }

    // Never pass raw password to config save
    delete configData.password;

    // Save the configuration
    await configManager.save(configData as any);

    logger.info("[Setup] First-run setup completed");
    res.json({ success: true });
  }));

  // Recheck cookies (triggers real validation via API calls)
  router.post("/cookies/recheck", asyncHandler(async (req, res) => {
    const cookieService = CookieRefreshService.getInstance();
    const validation = await cookieService.recheck();
    const autoCookieReloginRequired = AutoCookieService.getInstance().needsManualRelogin;

    res.json({
      success: validation.youtube.authenticated || validation.twitch.authenticated,
      cookieStatus: {
        found: validation.youtube.hasCookies,
        authenticated: validation.youtube.authenticated,
      },
      twitchAuthStatus: {
        authenticated: validation.twitch.authenticated,
      },
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
