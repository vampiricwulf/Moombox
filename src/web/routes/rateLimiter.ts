/**
 * Simple in-memory rate limiter middleware
 */

import type { Request, Response, NextFunction } from "express";

interface RateLimitEntry {
  count: number;
  resetTime: number;
}

const rateLimitMap = new Map<string, RateLimitEntry>();

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

    // Clean up expired entries periodically
    if (Math.random() < 0.01) { // 1% chance
      for (const [key, entry] of rateLimitMap.entries()) {
        if (now > entry.resetTime) {
          rateLimitMap.delete(key);
        }
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
