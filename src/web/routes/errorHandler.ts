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
 */
export function asyncHandler(
  fn: (req: Request, res: Response, next: NextFunction) => Promise<any>,
): (req: any, res: any, next: any) => void {
  return (req, res, next) => {
    fn(req, res, next).catch(next);
  };
}

/**
 * Express error-handling middleware. Must be registered after all routes.
 * Logs the full error server-side and returns a generic message to the client.
 */
export function errorMiddleware(
  err: any,
  req: Request,
  res: Response,
  _next: NextFunction,
): void {
  const logger = Logger.getInstance();
  logger.error(
    `[WebServer] ${req.method} ${req.path} failed: ${err.stack || err.message || err}`,
  );
  if (!res.headersSent) {
    res.status(500).json({ error: "Internal server error" });
  }
}
