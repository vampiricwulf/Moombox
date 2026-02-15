import { describe, it, expect } from "vitest";
import {
  MoomboxError,
  YouTubeError,
  VideoPlayabilityError,
  NetworkError,
  DownloadError,
  getErrorMessage,
  wrapError,
} from "./errors.js";

describe("Error Classes", () => {
  describe("MoomboxError", () => {
    it("should create error with message and code", () => {
      const error = new MoomboxError("Test error", "TEST_ERROR");
      expect(error.message).toBe("Test error");
      expect(error.code).toBe("TEST_ERROR");
      expect(error.name).toBe("MoomboxError");
    });

    it("should preserve stack trace", () => {
      const error = new MoomboxError("Test", "TEST");
      expect(error.stack).toBeDefined();
      expect(error.stack).toContain("MoomboxError");
    });

    it("should chain causes", () => {
      const cause = new Error("Original cause");
      const error = new MoomboxError("Wrapped", "WRAP", false, {}, cause);
      expect(error.cause).toBe(cause);
      expect(error.fullMessage).toContain("Original cause");
    });
  });

  describe("YouTubeError", () => {
    it("should inherit from MoomboxError", () => {
      const error = new YouTubeError("Test", "TEST");
      expect(error).toBeInstanceOf(MoomboxError);
      expect(error).toBeInstanceOf(YouTubeError);
      expect(error.name).toBe("YouTubeError");
    });

    it("should accept optional cause", () => {
      const cause = new Error("Original");
      const error = new YouTubeError("Failed", "FAIL", {}, cause);
      expect(error.cause).toBe(cause);
    });
  });

  describe("VideoPlayabilityError", () => {
    it("should store playability status", () => {
      const error = new VideoPlayabilityError(
        "Members only content",
        "members_only",
        "Reason message",
      );
      expect(error.playabilityStatus).toBe("members_only");
      expect(error.reason).toBe("Reason message");
      expect(error.message).toBe("Members only content");
    });

    it("should use status as code", () => {
      const error = new VideoPlayabilityError("Message", "login_required");
      expect(error.code).toBe("PLAYABILITY_LOGIN_REQUIRED");
    });
  });

  describe("NetworkError", () => {
    it("should store HTTP status and URL", () => {
      const error = new NetworkError("Request failed", 404, "https://example.com");
      expect(error.httpStatus).toBe(404);
      expect(error.url).toBe("https://example.com");
      expect(error.code).toBe("HTTP_404");
    });
  });

  describe("DownloadError", () => {
    it("should store HTTP status", () => {
      const error = new DownloadError("Download failed", "SEG_FAIL", 500);
      expect(error.httpStatus).toBe(500);
      expect(error.code).toBe("SEG_FAIL");
    });
  });
});

describe("getErrorMessage", () => {
  it("should extract message from Error objects", () => {
    const error = new Error("Test error");
    expect(getErrorMessage(error)).toBe("Test error");
  });

  it("should extract message from MoomboxError", () => {
    const error = new MoomboxError("Moombox error", "TEST");
    expect(getErrorMessage(error)).toBe("Moombox error");
  });

  it("should handle string errors", () => {
    expect(getErrorMessage("String error")).toBe("String error");
  });

  it("should handle number errors", () => {
    expect(getErrorMessage(404)).toBe("404");
  });

  it("should handle null/undefined", () => {
    expect(getErrorMessage(null)).toBe("null");
    expect(getErrorMessage(undefined)).toBe("undefined");
  });

  it("should handle objects with toString", () => {
    const obj = { toString: () => "Custom object" };
    expect(getErrorMessage(obj)).toBe("Custom object");
  });

  it("should handle objects without message", () => {
    const obj = { code: "ERROR", data: 123 };
    expect(getErrorMessage(obj)).toBe("[object Object]");
  });
});

describe("wrapError", () => {
  it("should wrap Error objects", () => {
    const original = new Error("Original error");
    const wrapped = wrapError(original, "Wrapped");

    expect(wrapped).toBeInstanceOf(MoomboxError);
    expect(wrapped.message).toBe("Original error");
    expect(wrapped.cause).toBe(original);
  });

  it("should return MoomboxError as-is", () => {
    const original = new YouTubeError("YT error", "YT_ERR");
    const wrapped = wrapError(original, "Ignored");

    expect(wrapped).toBe(original); // Same instance
  });

  it("should wrap string errors", () => {
    const wrapped = wrapError("String error", "Default msg");

    expect(wrapped).toBeInstanceOf(MoomboxError);
    expect(wrapped.message).toBe("String error");
  });

  it("should wrap unknown types with default message", () => {
    const wrapped = wrapError({ weird: "object" }, "Wrapped object");

    expect(wrapped).toBeInstanceOf(MoomboxError);
    expect(wrapped.message).toBe("Wrapped object");
  });

  it("should chain Error wrapping", () => {
    const original = new Error("Level 0");
    const wrap1 = wrapError(original, "Level 1");

    expect(wrap1.cause).toBe(original);
    expect(wrap1.fullMessage).toContain("Level 0");
  });

  it("should use default message for non-string unknowns", () => {
    const wrapped = wrapError(123, "Default");
    expect(wrapped.message).toBe("Default");
  });
});
