/**
 * Async route handler wrapper and error middleware.
 *
 * Wraps async route handlers so rejected promises are forwarded to Express
 * error handling, removing the need for try-catch in every route.
 */

import type { Request, Response, NextFunction } from "express";
import { Logger } from "../../core/logger.js";

/**
 * Wraps an async route handler to forward errors to Express error middleware.
 * Generic parameter P preserves route parameter types.
 */
export function asyncHandler<P = Record<string, string>>(
  fn: (req: Request<P>, res: Response, next: NextFunction) => Promise<unknown>,
): (req: Request<P>, res: Response, next: NextFunction) => void {
  return (req, res, next) => {
    fn(req, res, next).catch(next);
  };
}

/**
 * Express error-handling middleware. Must be registered after all routes.
 * Logs the full error server-side and returns a generic message to the client.
 */
export function errorMiddleware(
  err: unknown,
  req: Request,
  res: Response,
  _next: NextFunction,
): void {
  const logger = Logger.getInstance();
  const msg = err instanceof Error ? (err.stack || err.message) : String(err);
  logger.error(`[WebServer] ${req.method} ${req.path} failed: ${msg}`);
  if (!res.headersSent) {
    res.status(500).json({ error: "Internal server error" });
  }
}
