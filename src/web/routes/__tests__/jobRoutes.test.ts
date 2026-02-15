/**
 * Job routes integration tests
 */

import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from "vitest";
import request from "supertest";
import express from "express";
import { registerJobRoutes } from "../jobRoutes.js";
import type { Job } from "../../../types/jobs.js";

// Mock dependencies
vi.mock("../../../core/database.js");
vi.mock("../../../core/logger.js");
vi.mock("../../../core/config.js");
vi.mock("../../../engine/youtube/index.js");

describe("Job Routes", () => {
  let app: express.Application;
  const mockJobs: Job[] = [
    {
      id: "test123",
      videoId: "test123",
      url: "https://www.youtube.com/watch?v=test123",
      title: "Test Video",
      channelName: "Test Channel",
      status: "Downloading",
      percent: 50,
      progress: "50/100 segments",
      eta: "2m 30s",
      speed: "5 MB/s",
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      thumbnailUrl: "https://i.ytimg.com/vi/test123/maxresdefault.jpg",
      manuallyAdded: true,
    },
    {
      id: "finished456",
      videoId: "finished456",
      url: "https://www.youtube.com/watch?v=finished456",
      title: "Finished Video",
      channelName: "Test Channel",
      status: "Finished",
      percent: 100,
      progress: "Complete",
      eta: "",
      speed: "",
      createdAt: new Date(Date.now() - 86400000).toISOString(), // 1 day ago
      updatedAt: new Date().toISOString(),
      thumbnailUrl: "https://i.ytimg.com/vi/finished456/maxresdefault.jpg",
      manuallyAdded: false,
      outputFile: "/path/to/output.mp4",
      filename: "output.mp4",
      fileSize: 1073741824, // 1GB
    },
  ];

  beforeAll(async () => {
    // Set up mock implementations
    const { Database } = await import("../../../core/database.js");
    const { Logger } = await import("../../../core/logger.js");
    const { ConfigManager } = await import("../../../core/config.js");

    vi.mocked(Database.getInstance).mockResolvedValue({
      getJobs: vi.fn().mockResolvedValue(mockJobs),
      getJob: vi.fn((id: string) => {
        return Promise.resolve(mockJobs.find((j) => j.id === id));
      }),
      addJob: vi.fn((job: any) => {
        const newJob: Job = {
          ...job,
          id: job.videoId,
          status: "Upcoming",
          percent: 0,
          progress: "",
          eta: "",
          speed: "",
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
        };
        return Promise.resolve(newJob);
      }),
      jobExists: vi.fn((videoId: string) => {
        return Promise.resolve(mockJobs.some((j) => j.videoId === videoId));
      }),
      updateJob: vi.fn().mockResolvedValue(undefined),
      deleteJob: vi.fn().mockResolvedValue(true),
    } as any);

    vi.mocked(Logger.getInstance).mockReturnValue({
      debug: vi.fn(),
      info: vi.fn(),
      warn: vi.fn(),
      error: vi.fn(),
    } as any);

    vi.mocked(ConfigManager.getInstance).mockReturnValue({
      get: vi.fn().mockReturnValue({
        tasklist: { hide_finished_age_days: 30 },
        downloader: { output_directory: "/output" },
      }),
    } as any);

    // Create Express app
    app = express();
    app.use(express.json());

    const createRouter = () => {
      const router = express.Router();
      registerJobRoutes(router, {
        jobLogs: new Map(),
        filterJobsByAge: (jobs: Job[], archived: boolean) => {
          const now = Date.now();
          const hideAgeMs = 30 * 86400000;
          return jobs.filter((job) => {
            if (job.status !== "Finished") return !archived;
            const age = now - new Date(job.updatedAt).getTime();
            return archived ? age > hideAgeMs : age <= hideAgeMs;
          });
        },
        broadcast: vi.fn(),
        isLoopback: vi.fn(() => true),
      });
      return router;
    };

    // Mount at both /api and /api/v1 to match server behavior
    app.use("/api/v1", createRouter());
    app.use("/api", createRouter());
  });

  afterAll(() => {
    vi.restoreAllMocks();
  });

  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe("GET /api/jobs", () => {
    it("should return active jobs (non-archived)", async () => {
      const res = await request(app).get("/api/jobs").expect(200);

      expect(res.body).toBeInstanceOf(Array);
      expect(res.body.length).toBeGreaterThan(0);
      expect(res.body[0]).toHaveProperty("id");
      expect(res.body[0]).toHaveProperty("status");
    });

    it("should not include archived jobs", async () => {
      const res = await request(app).get("/api/jobs").expect(200);

      // Archived jobs are finished and older than hide_finished_age_days
      const archivedJobs = res.body.filter((j: Job) => {
        if (j.status !== "Finished") return false;
        const age = Date.now() - new Date(j.updatedAt).getTime();
        return age > 30 * 86400000;
      });

      expect(archivedJobs.length).toBe(0);
    });

    it("should support pagination with limit parameter", async () => {
      const res = await request(app).get("/api/jobs?limit=1").expect(200);

      expect(res.body).toHaveProperty("data");
      expect(res.body).toHaveProperty("pagination");
      expect(res.body.data).toBeInstanceOf(Array);
      expect(res.body.data.length).toBeLessThanOrEqual(1);
      expect(res.body.pagination).toHaveProperty("total");
      expect(res.body.pagination).toHaveProperty("offset", 0);
      expect(res.body.pagination).toHaveProperty("limit", 1);
      expect(res.body.pagination).toHaveProperty("hasMore");
    });

    it("should support pagination with offset parameter", async () => {
      const res = await request(app).get("/api/jobs?offset=1").expect(200);

      expect(res.body).toHaveProperty("data");
      expect(res.body).toHaveProperty("pagination");
      expect(res.body.pagination).toHaveProperty("offset", 1);
    });

    it("should reject invalid limit parameter", async () => {
      const res = await request(app).get("/api/jobs?limit=2000").expect(400);

      expect(res.body).toHaveProperty("error");
      expect(res.body.error).toMatch(/Invalid limit parameter/);
    });

    it("should reject invalid offset parameter", async () => {
      const res = await request(app).get("/api/jobs?offset=-1").expect(400);

      expect(res.body).toHaveProperty("error");
      expect(res.body.error).toMatch(/Invalid offset parameter/);
    });

    it("should work with /api/v1 prefix", async () => {
      const res = await request(app).get("/api/v1/jobs").expect(200);

      expect(res.body).toBeInstanceOf(Array);
      expect(res.body.length).toBeGreaterThan(0);
    });
  });

  describe("GET /api/jobs/archived", () => {
    it("should return only archived jobs", async () => {
      const res = await request(app).get("/api/jobs/archived").expect(200);

      expect(res.body).toBeInstanceOf(Array);
      // All returned jobs should be finished and old
      for (const job of res.body) {
        expect(job.status).toBe("Finished");
      }
    });
  });

  describe("GET /api/jobs/:id", () => {
    it("should return a single job by ID", async () => {
      const res = await request(app).get("/api/jobs/test123").expect(200);

      expect(res.body).toHaveProperty("id", "test123");
      expect(res.body).toHaveProperty("title", "Test Video");
    });

    it("should return 404 for non-existent job", async () => {
      const res = await request(app).get("/api/jobs/nonexistent").expect(404);

      expect(res.body).toHaveProperty("error", "Job not found");
    });

    it("should set Cache-Control for finished jobs", async () => {
      const res = await request(app).get("/api/jobs/finished456").expect(200);

      expect(res.headers["cache-control"]).toMatch(/immutable/);
    });

    it("should set no-cache for active jobs", async () => {
      const res = await request(app).get("/api/jobs/test123").expect(200);

      expect(res.headers["cache-control"]).toMatch(/no-cache/);
    });
  });

  describe("POST /api/jobs", () => {
    it("should create a new job with valid videoId", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({ videoId: "newvideo123" })
        .expect(201);

      expect(res.body).toHaveProperty("id", "newvideo123");
      expect(res.body).toHaveProperty("status", "Upcoming");
    });

    it("should reject invalid videoId format", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({ videoId: "short" })
        .expect(400);

      expect(res.body).toHaveProperty("error");
      expect(res.body.error).toMatch(/Validation failed/);
    });

    it("should reject missing videoId", async () => {
      const res = await request(app).post("/api/jobs").send({}).expect(400);

      expect(res.body).toHaveProperty("error");
    });

    it("should reject duplicate videoId with 409", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({ videoId: "test123" })
        .expect(409);

      expect(res.body).toHaveProperty("error", "Job already exists");
    });

    it("should accept advanced options (selectedVideoItag)", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({
          videoId: "newvideo456",
          selectedVideoItag: 137,
          selectedAudioItag: 140,
        })
        .expect(201);

      expect(res.body).toHaveProperty("id", "newvideo456");
    });

    it("should reject both formats as -1 (none)", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({
          videoId: "newvideo789",
          selectedVideoItag: -1,
          selectedAudioItag: -1,
        })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
      expect(res.body.details).toBeDefined();
    });

    it("should accept timestamp selection", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({
          videoId: "newvideoabc",
          startTime: 100,
          endTime: 500,
        })
        .expect(201);

      expect(res.body).toHaveProperty("id");
    });

    it("should reject endTime <= startTime", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({
          videoId: "newvideodef",
          startTime: 500,
          endTime: 100,
        })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
    });

    it("should reject invalid itag values", async () => {
      const res = await request(app)
        .post("/api/jobs")
        .send({
          videoId: "newvideoghi",
          selectedVideoItag: 9999, // Out of range
        })
        .expect(400);

      expect(res.body.error).toMatch(/Validation failed/);
    });
  });

  describe("POST /api/jobs/:id/cancel", () => {
    it("should cancel a downloading job", async () => {
      const res = await request(app)
        .post("/api/jobs/test123/cancel")
        .expect(200);

      expect(res.body).toHaveProperty("success", true);
    });

    it("should return 404 for non-existent job", async () => {
      const res = await request(app)
        .post("/api/jobs/nonexistent/cancel")
        .expect(404);

      expect(res.body).toHaveProperty("error");
    });
  });

  describe("DELETE /api/jobs/:id", () => {
    it("should delete a job", async () => {
      const res = await request(app).delete("/api/jobs/test123").expect(200);

      expect(res.body).toHaveProperty("success", true);
    });

    it("should return 404 for non-existent job", async () => {
      const res = await request(app)
        .delete("/api/jobs/nonexistent")
        .expect(404);

      expect(res.body).toHaveProperty("error");
    });
  });

  describe("GET /api/formats/:videoId", () => {
    it("should reject invalid videoId format", async () => {
      const res = await request(app).get("/api/formats/invalid").expect(400);

      expect(res.body).toHaveProperty("error");
      expect(res.body.error).toMatch(/Invalid videoId format/);
    });

    it("should accept valid 11-character videoId", async () => {
      // This will fail at YouTube fetch, but should pass validation
      const res = await request(app).get("/api/formats/dQw4w9WgXcQ");

      // Either 500 (fetch failed) or 200 (success) - both mean validation passed
      expect([200, 500]).toContain(res.status);
    });
  });
});
