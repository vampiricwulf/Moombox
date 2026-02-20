/**
 * Simple in-memory rate limiter middleware
 */

import type { Request, Response, NextFunction } from "express";

interface RateLimitEntry {
  count: number;
  resetTime: number;
}

const rateLimitMap = new Map<string, RateLimitEntry>();
const MAX_ENTRIES = 10_000; // Prevent unbounded growth

// Deterministic cleanup: runs every 60 seconds
const cleanupInterval = setInterval(() => {
  const now = Date.now();
  for (const [key, entry] of rateLimitMap.entries()) {
    if (now > entry.resetTime) {
      rateLimitMap.delete(key);
    }
  }
}, 60_000).unref(); // unref() allows process to exit even if timer is pending

/**
 * Creates a rate limiter middleware that limits requests per IP address
 * @param maxRequests Maximum number of requests allowed in the window
 * @param windowMs Time window in milliseconds
 * @returns Express middleware function
 */
export function createRateLimiter(maxRequests: number, windowMs: number) {
  return (req: Request, res: Response, next: NextFunction): void => {
    const ip = req.socket.remoteAddress || "unknown";
    const now = Date.now();

    // Prevent unbounded map growth (evict oldest if limit exceeded)
    if (rateLimitMap.size > MAX_ENTRIES) {
      const oldest = Array.from(rateLimitMap.entries())
        .sort((a, b) => a[1].resetTime - b[1].resetTime)[0];
      if (oldest) {
        rateLimitMap.delete(oldest[0]);
      }
    }

    const entry = rateLimitMap.get(ip);

    if (!entry || now > entry.resetTime) {
      // New window or expired entry
      rateLimitMap.set(ip, { count: 1, resetTime: now + windowMs });
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
