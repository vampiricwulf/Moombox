/**
 * Trim routes - Create and delete trimmed versions of videos
 */

import type { Router} from "express";
import { Database } from "../../core/database.js";
import { TrimService } from "../../core/worker/trimService.js";
import { MuxingError } from "../../types/errors.js";
import { asyncHandler } from "./errorHandler.js";
import { createTrimSchema, formatValidationErrors } from "../../types/schemas.js";
import { Logger } from "../../core/logger.js";

const logger = Logger.getInstance();

export function registerTrimRoutes(router: Router): void {
  // POST /api/jobs/:id/trims - Create trim
  router.post(
    "/jobs/:id/trims",
    asyncHandler(async (req, res) => {
      const jobId = req.params.id as string;

      // Validate request body with Zod schema
      const validation = createTrimSchema.safeParse(req.body);
      if (!validation.success) {
        const errors = formatValidationErrors(validation.error);
        return res.status(400).json({
          error: "Validation failed",
          details: errors
        });
      }

      const { startTime, endTime } = validation.data;

      // Load job
      const db = await Database.getInstance();
      const job = await db.getJob(jobId);
      if (!job) {
        return res.status(404).json({ error: "Job not found" });
      }

      // Create AbortController to cancel on client disconnect
      const abortController = new AbortController();
      req.on("close", () => {
        abortController.abort();
      });

      // Create trim with abort signal
      try {
        const trim = await TrimService.createTrim(
          job,
          startTime,
          endTime,
          abortController.signal,
        );
        res.json({ trim });
      } catch (error: unknown) {
        const message = error instanceof Error ? error.message : String(error);
        logger.error(`[TrimRoutes] Failed to create trim: ${message}`);
        const status = error instanceof MuxingError ? 500 : 400;
        res.status(status).json({ error: "Failed to create trim" });
      }
    }),
  );

  // DELETE /api/jobs/:id/trims/:trimId - Delete trim
  router.delete(
    "/jobs/:id/trims/:trimId",
    asyncHandler(async (req, res) => {
      const { id: jobId, trimId } = req.params;
      await TrimService.deleteTrim(jobId as string, trimId as string);
      res.json({ success: true });
    }),
  );

  // GET /api/jobs/:id/trims - List trims
  router.get(
    "/jobs/:id/trims",
    asyncHandler(async (req, res) => {
      const jobId = req.params.id as string;
      const db = await Database.getInstance();
      const job = await db.getJob(jobId);
      if (!job) {
        return res.status(404).json({ error: "Job not found" });
      }
      res.json({ trims: job.trims || [] });
    }),
  );
}
