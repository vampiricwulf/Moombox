/**
 * Shared types for route handlers
 */

import type { Database } from "../../core/database.js";
import type { Logger } from "../../core/logger.js";
import type { ConfigManager } from "../../core/config.js";
import type { DownloadWorker } from "../../core/worker/index.js";

/**
 * Context object passed to route registration functions
 */
export interface RouteContext {
  db: Database;
  logger: Logger;
  config: ConfigManager;
  worker: DownloadWorker;
}
