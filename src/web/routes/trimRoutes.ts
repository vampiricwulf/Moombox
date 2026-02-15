/**
 * Trim routes - Create and delete trimmed versions of videos
 */

import type { Router } from "express";
import { Database } from "../../core/database.js";
import { TrimService } from "../../core/worker/trimService.js";
import { asyncHandler } from "./errorHandler.js";

export function registerTrimRoutes(router: Router): void {
  // POST /api/jobs/:id/trims - Create trim
  router.post(
    "/jobs/:id/trims",
    asyncHandler(async (req, res) => {
      const jobId = req.params.id as string;
      const { startTime, endTime } = req.body;

      // Validate inputs (comprehensive checks)
      if (
        typeof startTime !== "number" ||
        typeof endTime !== "number" ||
        !Number.isFinite(startTime) ||
        !Number.isFinite(endTime) ||
        startTime < 0 ||
        endTime <= startTime
      ) {
        return res.status(400).json({ error: "Invalid time range" });
      }

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
      } catch (error: any) {
        res.status(400).json({ error: error.message });
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
