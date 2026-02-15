/**
 * Trim routes integration tests
 */

import { describe, it, expect, beforeAll, vi } from "vitest";
import request from "supertest";
import express from "express";
import { registerTrimRoutes } from "../trimRoutes.js";

vi.mock("../../../core/database.js");
vi.mock("../../../core/logger.js");
vi.mock("../../../core/worker/trimService.js");

describe("Trim Routes", () => {
  let app: express.Application;

  beforeAll(async () => {
    const { Database } = await import("../../../core/database.js");
    const { Logger } = await import("../../../core/logger.js");
    const { TrimService } = await import("../../../core/worker/trimService.js");

    vi.mocked(Database.getInstance).mockResolvedValue({
      getJob: vi.fn((id: string) => {
        if (id === "testjob") {
          return Promise.resolve({
            id: "testjob",
            videoId: "testjob",
            status: "Finished",
            outputFile: "/path/to/video.mp4",
            lengthSeconds: 3600,
          });
        }
        return Promise.resolve(undefined);
      }),
    } as any);

    vi.mocked(Logger.getInstance).mockReturnValue({
      debug: vi.fn(),
      info: vi.fn(),
      warn: vi.fn(),
      error: vi.fn(),
    } as any);

    vi.mocked(TrimService.createTrim).mockResolvedValue({
      id: "trim123",
      jobId: "testjob",
      startTime: 100,
      endTime: 500,
      duration: 400,
      filename: "trim.mp4",
      status: "completed",
    } as any);

    app = express();
    app.use(express.json());

    const router = express.Router();
    registerTrimRoutes(router);
    app.use("/api", router);
  });

  describe("POST /api/jobs/:id/trims", () => {
    it("should create a trim with valid time range", async () => {
      const res = await request(app)
        .post("/api/jobs/testjob/trims")
        .send({ startTime: 100, endTime: 500 })
        .expect(200);

      expect(res.body).toHaveProperty("trim");
      expect(res.body.trim).toHaveProperty("id");
    });

    it("should reject invalid time range (endTime <= startTime)", async () => {
      const res = await request(app)
        .post("/api/jobs/testjob/trims")
        .send({ startTime: 500, endTime: 100 })
        .expect(400);

      expect(res.body).toHaveProperty("error");
      expect(res.body.error).toMatch(/Validation failed/);
    });

    it("should reject negative startTime", async () => {
      const res = await request(app)
        .post("/api/jobs/testjob/trims")
        .send({ startTime: -10, endTime: 100 })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
    });

    it("should reject non-number values", async () => {
      const res = await request(app)
        .post("/api/jobs/testjob/trims")
        .send({ startTime: "invalid", endTime: 100 })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
    });

    it("should return 404 for non-existent job", async () => {
      const res = await request(app)
        .post("/api/jobs/nonexistent/trims")
        .send({ startTime: 0, endTime: 100 })
        .expect(404);

      expect(res.body).toHaveProperty("error", "Job not found");
    });

    it("should reject trim < 1 second", async () => {
      const res = await request(app)
        .post("/api/jobs/testjob/trims")
        .send({ startTime: 100, endTime: 100.5 })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
    });
  });
});
