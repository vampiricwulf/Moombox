import { describe, it, expect } from "vitest";
import { extractVideoId } from "./youtube.js";

describe("extractVideoId", () => {
  describe("valid inputs", () => {
    it("should extract from youtube.com/watch?v=ID", () => {
      expect(extractVideoId("https://www.youtube.com/watch?v=dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("http://youtube.com/watch?v=dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("youtube.com/watch?v=dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
    });

    it("should extract from youtu.be/ID", () => {
      expect(extractVideoId("https://youtu.be/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("http://youtu.be/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("youtu.be/dQw4w9WgXcQ")).toBe("dQw4w9WgXcQ");
    });

    it("should extract from youtube.com/live/ID", () => {
      expect(extractVideoId("https://www.youtube.com/live/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("youtube.com/live/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
    });

    it("should extract from youtube.com/shorts/ID", () => {
      expect(extractVideoId("https://www.youtube.com/shorts/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
      expect(extractVideoId("youtube.com/shorts/dQw4w9WgXcQ")).toBe(
        "dQw4w9WgXcQ",
      );
    });

    it("should accept bare 11-character IDs", () => {
      expect(extractVideoId("dQw4w9WgXcQ")).toBe("dQw4w9WgXcQ");
      expect(extractVideoId("0123456789A")).toBe("0123456789A");
      expect(extractVideoId("___________")).toBe("___________");
      expect(extractVideoId("-----------")).toBe("-----------");
    });

    it("should handle URLs with query parameters", () => {
      expect(
        extractVideoId(
          "https://www.youtube.com/watch?v=dQw4w9WgXcQ&feature=share",
        ),
      ).toBe("dQw4w9WgXcQ");
      expect(
        extractVideoId("https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10s"),
      ).toBe("dQw4w9WgXcQ");
    });

    it("should handle URLs with timestamps", () => {
      expect(extractVideoId("https://youtu.be/dQw4w9WgXcQ?t=123")).toBe(
        "dQw4w9WgXcQ",
      );
    });
  });

  describe("invalid inputs", () => {
    it("should return null for empty string", () => {
      expect(extractVideoId("")).toBeNull();
    });

    it("should return null for whitespace-only", () => {
      expect(extractVideoId("   ")).toBeNull();
      expect(extractVideoId("\n\t")).toBeNull();
    });

    it("should return null for invalid ID length", () => {
      expect(extractVideoId("short")).toBeNull();
      expect(extractVideoId("toolongvideoid123")).toBeNull();
      expect(extractVideoId("exactly12chr")).toBeNull();
    });

    it("should return null for invalid characters in ID", () => {
      expect(extractVideoId("has spaces!")).toBeNull();
      expect(extractVideoId("has@symbols")).toBeNull();
      expect(extractVideoId("hasспецсимв")).toBeNull();
    });

    it("should return null for non-YouTube URLs", () => {
      expect(extractVideoId("https://vimeo.com/123456789")).toBeNull();
      expect(extractVideoId("https://example.com/watch?v=dQw4w9WgXcQ")).toBeNull();
    });

    it("should return null for malformed YouTube URLs", () => {
      expect(extractVideoId("https://youtube.com/notwatch")).toBeNull();
      expect(extractVideoId("https://youtube.com/watch?noV=param")).toBeNull();
    });
  });

  describe("edge cases", () => {
    it("should preserve case in video ID", () => {
      expect(extractVideoId("AbCdEfGhIjK")).toBe("AbCdEfGhIjK");
      expect(extractVideoId("https://youtu.be/AbCdEfGhIjK")).toBe(
        "AbCdEfGhIjK",
      );
    });

    it("should handle trailing slashes", () => {
      expect(extractVideoId("https://youtu.be/dQw4w9WgXcQ/")).toBe("dQw4w9WgXcQ");
    });

    it("should handle underscores and hyphens in IDs", () => {
      expect(extractVideoId("ABC_DEF-123")).toBe("ABC_DEF-123");
      expect(extractVideoId("https://youtu.be/ABC_DEF-123")).toBe("ABC_DEF-123");
    });
  });
});
