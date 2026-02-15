/**
 * POT Provider endpoints (bgutil-ytdlp-pot-provider compatible).
 * These endpoints allow yt-dlp to use Moombox as a POT provider.
 */

import type { Application, Request, Response, NextFunction } from "express";
import { Logger } from "../../core/logger.js";
import { PotProvider } from "../../core/potProvider.js";
import { asyncHandler } from "./errorHandler.js";

// POT Provider version for compatibility
const POT_PROVIDER_VERSION = "1.0.0";

/** Restrict access to loopback IPs only */
function requireLoopback(req: Request, res: Response, next: NextFunction): void {
  const ip = req.ip || req.socket.remoteAddress || "";
  if (ip === "127.0.0.1" || ip === "::1" || ip === "::ffff:127.0.0.1") {
    next();
  } else {
    res.status(403).json({ error: "Forbidden: loopback only" });
  }
}

/** Simple per-IP rate limiter */
function createRateLimiter(maxRequests: number, windowMs: number) {
  const hits = new Map<string, { count: number; resetAt: number }>();
  // Clean up stale entries periodically
  setInterval(() => {
    const now = Date.now();
    for (const [ip, entry] of hits) {
      if (now > entry.resetAt) hits.delete(ip);
    }
  }, windowMs).unref();

  return (req: Request, res: Response, next: NextFunction): void => {
    const ip = req.ip || req.socket.remoteAddress || "unknown";
    const now = Date.now();
    let entry = hits.get(ip);
    if (!entry || now > entry.resetAt) {
      entry = { count: 0, resetAt: now + windowMs };
      hits.set(ip, entry);
    }
    entry.count++;
    if (entry.count > maxRequests) {
      res.status(429).json({ error: "Too many requests" });
      return;
    }
    next();
  };
}

const potRateLimiter = createRateLimiter(10, 60 * 1000); // 10 req/min

export function registerPotRoutes(app: Application): void {
  const logger = Logger.getInstance();

  // Generate PO Token - main endpoint for yt-dlp (loopback only, rate limited)
  app.post("/get_pot", requireLoopback, potRateLimiter, asyncHandler(async (req, res) => {
    const body = req.body || {};

    // Handle deprecated parameters
    if (body.data_sync_id) {
      logger.warn(
        "[PotProvider] Rejected request using deprecated data_sync_id",
      );
      return res.status(400).json({
        error: "data_sync_id is deprecated, use content_binding instead",
      });
    }
    if (body.visitor_data) {
      logger.warn(
        "[PotProvider] Rejected request using deprecated visitor_data",
      );
      return res.status(400).json({
        error: "visitor_data is deprecated, use content_binding instead",
      });
    }

    const contentBinding: string | undefined = body.content_binding;
    const bypassCache: boolean = body.bypass_cache || false;

    logger.info(
      `[PotProvider] POT requested${contentBinding ? ` for ${contentBinding.substring(0, 20)}...` : " (no binding)"}${bypassCache ? " (bypass cache)" : ""}`,
    );

    const potProvider = PotProvider.getInstance();
    const sessionData = await potProvider.generatePoToken(
      contentBinding,
      bypassCache,
    );

    logger.info(
      `[PotProvider] POT generated: ${sessionData.poToken.substring(0, 30)}...`,
    );
    res.json(sessionData);
  }));

  // Invalidate all caches (loopback only — state-changing)
  app.post("/invalidate_caches", requireLoopback, (req, res) => {
    logger.info("[PotProvider] Cache invalidation requested");
    const potProvider = PotProvider.getInstance();
    potProvider.invalidateCaches();
    res.status(204).send();
  });

  // Invalidate integrity tokens (loopback only — state-changing)
  app.post("/invalidate_it", requireLoopback, (req, res) => {
    logger.info("[PotProvider] Integrity token invalidation requested");
    const potProvider = PotProvider.getInstance();
    potProvider.invalidateIntegrityTokens();
    res.status(204).send();
  });

  // Ping endpoint for health checks
  app.get("/ping", (req, res) => {
    logger.debug("[PotProvider] Ping received");
    res.json({
      server_uptime: process.uptime(),
      version: POT_PROVIDER_VERSION,
    });
  });

  // Debug endpoint to view minter cache
  app.get("/minter_cache", (req, res) => {
    logger.debug("[PotProvider] Minter cache requested");
    const potProvider = PotProvider.getInstance();
    const cache = potProvider.getMinterCache();
    res.json(Array.from(cache.keys()));
  });
}
