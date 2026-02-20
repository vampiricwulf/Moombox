/**
 * Authentication endpoints — login, logout, password management.
 */

import type { Router } from "express";
import { AuthService } from "../../core/auth.js";
import { ConfigManager } from "../../core/config.js";
import { Logger } from "../../core/logger.js";
import { asyncHandler } from "./errorHandler.js";
import { createRateLimiter } from "./rateLimiter.js";
import { isLoopback } from "../../utils/ipValidation.js";
import { loginSchema, setPasswordSchema, removePasswordSchema, formatValidationErrors } from "../../types/schemas.js";

export function registerAuthRoutes(router: Router): void {
  const logger = Logger.getInstance();

  // GET /auth/status — public, no auth needed
  router.get("/auth/status", (req, res) => {
    const config = ConfigManager.getInstance().get();
    const auth = AuthService.getInstance();
    const hasPassword = !!config.password_hash;
    const authRequired = auth.isAuthRequired();

    // Check if caller is authenticated
    let authenticated = false;
    if (authRequired) {
      const token = auth.parseCookies(req.headers.cookie || "")["moombox_session"];
      authenticated = !!token && auth.validateSession(token);
    }

    res.json({ authRequired, authenticated, hasPassword });
  });

  // POST /auth/login — rate-limited
  router.post("/auth/login", createRateLimiter(5, 60_000), asyncHandler(async (req, res) => {
    const validation = loginSchema.safeParse(req.body);
    if (!validation.success) {
      return res.status(400).json({
        error: "Validation failed",
        details: formatValidationErrors(validation.error),
      });
    }

    const config = ConfigManager.getInstance().get();
    if (!config.password_hash) {
      return res.status(400).json({ error: "No password is set" });
    }

    const auth = AuthService.getInstance();
    if (!auth.verifyPassword(validation.data.password, config.password_hash)) {
      logger.warn("[Auth] Failed login attempt from " + req.socket.remoteAddress);
      return res.status(401).json({ error: "Invalid password" });
    }

    const token = auth.createSession();
    const secure = req.secure ? "; Secure" : "";
    res.setHeader("Set-Cookie",
      `moombox_session=${token}; HttpOnly; SameSite=Lax; Path=/; Max-Age=86400${secure}`
    );

    logger.info("[Auth] Successful login from " + req.socket.remoteAddress);
    res.json({ success: true });
  }));

  // POST /auth/logout — requires valid session
  router.post("/auth/logout", asyncHandler(async (req, res) => {
    const auth = AuthService.getInstance();
    const token = auth.parseCookies(req.headers.cookie || "")["moombox_session"];
    if (token) {
      auth.invalidateSession(token);
    }

    // Clear cookie
    res.setHeader("Set-Cookie",
      "moombox_session=; HttpOnly; SameSite=Lax; Path=/; Max-Age=0"
    );
    res.json({ success: true });
  }));

  // POST /auth/set-password — rate-limited, requires session OR loopback
  router.post("/auth/set-password", createRateLimiter(3, 60_000), asyncHandler(async (req, res) => {
    const ip = req.socket.remoteAddress || "";
    const auth = AuthService.getInstance();
    const configManager = ConfigManager.getInstance();
    const config = configManager.get();

    // Must be authenticated or from loopback
    const token = auth.parseCookies(req.headers.cookie || "")["moombox_session"];
    const isAuthed = !!token && auth.validateSession(token);
    if (!isAuthed && !isLoopback(ip)) {
      return res.status(401).json({ error: "Authentication required" });
    }

    const validation = setPasswordSchema.safeParse(req.body);
    if (!validation.success) {
      return res.status(400).json({
        error: "Validation failed",
        details: formatValidationErrors(validation.error),
      });
    }

    // If a password already exists, require currentPassword to change it
    if (config.password_hash) {
      if (!validation.data.currentPassword) {
        return res.status(400).json({ error: "Current password is required" });
      }
      if (!auth.verifyPassword(validation.data.currentPassword, config.password_hash)) {
        return res.status(401).json({ error: "Current password is incorrect" });
      }
    }

    // Hash and save
    const newHash = auth.hashPassword(validation.data.newPassword);
    const updatedConfig = { ...config, password_hash: newHash };
    await configManager.save(updatedConfig);

    // Invalidate other sessions (keep caller's session if they have one)
    if (token && isAuthed) {
      auth.invalidateOtherSessions(token);
    } else {
      auth.invalidateAllSessions();
    }

    logger.info("[Auth] Password set/changed from " + ip);
    res.json({ success: true });
  }));

  // POST /auth/remove-password — requires session or loopback
  router.post("/auth/remove-password", createRateLimiter(3, 60_000), asyncHandler(async (req, res) => {
    const ip = req.socket.remoteAddress || "";
    const auth = AuthService.getInstance();
    const configManager = ConfigManager.getInstance();
    const config = configManager.get();

    // Must be authenticated or from loopback
    const token = auth.parseCookies(req.headers.cookie || "")["moombox_session"];
    const isAuthed = !!token && auth.validateSession(token);
    if (!isAuthed && !isLoopback(ip)) {
      return res.status(401).json({ error: "Authentication required" });
    }

    if (!config.password_hash) {
      return res.status(400).json({ error: "No password is set" });
    }

    const validation = removePasswordSchema.safeParse(req.body);
    if (!validation.success) {
      return res.status(400).json({
        error: "Validation failed",
        details: formatValidationErrors(validation.error),
      });
    }

    if (!auth.verifyPassword(validation.data.currentPassword, config.password_hash)) {
      return res.status(401).json({ error: "Current password is incorrect" });
    }

    // Remove password and force network_access back to localhost
    const { password_hash: _, ...configWithoutHash } = config;
    const updatedConfig = { ...configWithoutHash, network_access: "localhost" as const };
    await configManager.save(updatedConfig);

    // Invalidate all sessions
    auth.invalidateAllSessions();

    // Clear cookie
    res.setHeader("Set-Cookie",
      "moombox_session=; HttpOnly; SameSite=Lax; Path=/; Max-Age=0"
    );

    logger.info("[Auth] Password removed, network_access reset to localhost from " + ip);
    res.json({ success: true, networkAccessChanged: true });
  }));
}
