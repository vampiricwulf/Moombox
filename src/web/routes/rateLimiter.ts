/**
 * Simple in-memory rate limiter middleware.
 * Each createRateLimiter() call gets its own Map to prevent cross-contamination.
 */

import type { Request, Response, NextFunction } from "express";

interface RateLimitEntry {
  count: number;
  resetTime: number;
}

const MAX_ENTRIES = 10_000; // Prevent unbounded growth per limiter

/** All active limiter maps, for shared cleanup */
const allMaps: Map<string, RateLimitEntry>[] = [];

// Deterministic cleanup: runs every 60 seconds across all limiters
setInterval(() => {
  const now = Date.now();
  for (const map of allMaps) {
    for (const [key, entry] of map.entries()) {
      if (now > entry.resetTime) {
        map.delete(key);
      }
    }
  }
}, 60_000).unref(); // unref() allows process to exit even if timer is pending

/**
 * Creates a rate limiter middleware that limits requests per IP address.
 * Each call creates an independent limiter with its own counter map.
 * @param maxRequests Maximum number of requests allowed in the window
 * @param windowMs Time window in milliseconds
 * @returns Express middleware function
 */
export function createRateLimiter(maxRequests: number, windowMs: number) {
  const map = new Map<string, RateLimitEntry>();
  allMaps.push(map);

  return (req: Request, res: Response, next: NextFunction): void => {
    const ip = req.socket.remoteAddress || "unknown";
    const now = Date.now();

    // Prevent unbounded map growth (evict oldest entry via O(1) iterator)
    if (map.size > MAX_ENTRIES) {
      const firstKey = map.keys().next().value;
      if (firstKey !== undefined) map.delete(firstKey);
    }

    const entry = map.get(ip);

    if (!entry || now > entry.resetTime) {
      // New window or expired entry
      map.set(ip, { count: 1, resetTime: now + windowMs });
      next();
    } else if (entry.count < maxRequests) {
      // Within limit
      entry.count++;
      next();
    } else {
      // Rate limit exceeded
      const retryAfter = Math.ceil((entry.resetTime - now) / 1000);
      res.set("Retry-After", String(retryAfter));
      res.status(429).json({
        error: "Too many requests, please try again later",
        retryAfter,
      });
    }
  };
}
