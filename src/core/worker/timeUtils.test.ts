import { describe, it, expect } from "vitest";
import { parseTimeToSeconds, formatSecondsToTimestamp } from "./timeUtils.js";

describe("parseTimeToSeconds", () => {
  describe("HH:MM:SS format", () => {
    it("should parse standard timestamps", () => {
      expect(parseTimeToSeconds("01:23:45")).toBe(5025); // 1*3600 + 23*60 + 45
      expect(parseTimeToSeconds("10:00:00")).toBe(36000);
      expect(parseTimeToSeconds("00:00:01")).toBe(1);
    });

    it("should handle hours > 23", () => {
      expect(parseTimeToSeconds("25:30:15")).toBe(91815); // 25*3600 + 30*60 + 15
      expect(parseTimeToSeconds("100:00:00")).toBe(360000);
    });

    it("should handle single-digit components", () => {
      expect(parseTimeToSeconds("1:2:3")).toBe(3723); // 1*3600 + 2*60 + 3
      expect(parseTimeToSeconds("0:0:0")).toBe(0);
    });
  });

  describe("MM:SS format", () => {
    it("should parse minute:second timestamps", () => {
      expect(parseTimeToSeconds("12:34")).toBe(754); // 12*60 + 34
      expect(parseTimeToSeconds("00:30")).toBe(30);
      expect(parseTimeToSeconds("59:59")).toBe(3599);
    });

    it("should handle minutes > 59", () => {
      expect(parseTimeToSeconds("90:30")).toBe(5430); // 90*60 + 30
      expect(parseTimeToSeconds("120:00")).toBe(7200);
    });

    it("should handle single-digit components", () => {
      expect(parseTimeToSeconds("5:7")).toBe(307); // 5*60 + 7
      expect(parseTimeToSeconds("0:0")).toBe(0);
    });
  });

  describe("raw seconds", () => {
    it("should parse integer seconds", () => {
      expect(parseTimeToSeconds("0")).toBe(0);
      expect(parseTimeToSeconds("60")).toBe(60);
      expect(parseTimeToSeconds("3600")).toBe(3600);
      expect(parseTimeToSeconds("123456")).toBe(123456);
    });

    it("should parse decimal seconds", () => {
      expect(parseTimeToSeconds("30.5")).toBe(30.5);
      expect(parseTimeToSeconds("1.25")).toBe(1.25);
    });
  });

  describe("invalid inputs", () => {
    it("should return null for empty string", () => {
      expect(parseTimeToSeconds("")).toBeNull();
    });

    it("should return null for whitespace-only", () => {
      expect(parseTimeToSeconds("   ")).toBeNull();
      expect(parseTimeToSeconds("\n\t")).toBeNull();
    });

    it("should return null for non-numeric input", () => {
      expect(parseTimeToSeconds("abc")).toBeNull();
      expect(parseTimeToSeconds("12:ab")).toBeNull();
      expect(parseTimeToSeconds("not a time")).toBeNull();
    });

    it("should return null for too many colons", () => {
      expect(parseTimeToSeconds("1:2:3:4")).toBeNull();
    });

    it("should return null for negative values", () => {
      expect(parseTimeToSeconds("-10")).toBeNull();
      expect(parseTimeToSeconds("-1:30")).toBeNull();
    });
  });

  describe("edge cases", () => {
    it("should handle leading/trailing whitespace", () => {
      expect(parseTimeToSeconds("  12:34  ")).toBe(754);
      expect(parseTimeToSeconds("\t100\n")).toBe(100);
    });

    it("should handle zero values", () => {
      expect(parseTimeToSeconds("0")).toBe(0);
      expect(parseTimeToSeconds("00:00")).toBe(0);
      expect(parseTimeToSeconds("00:00:00")).toBe(0);
    });

    it("should handle large values", () => {
      expect(parseTimeToSeconds("999:59:59")).toBe(3599999);
      expect(parseTimeToSeconds("9999999")).toBe(9999999);
    });
  });
});

describe("formatSecondsToTimestamp", () => {
  it("should format to H:MM:SS when hours present", () => {
    expect(formatSecondsToTimestamp(5025)).toBe("1:23:45");
    expect(formatSecondsToTimestamp(3600)).toBe("1:00:00");
  });

  it("should format to M:SS when no hours", () => {
    expect(formatSecondsToTimestamp(60)).toBe("1:00");
    expect(formatSecondsToTimestamp(1)).toBe("0:01");
  });

  it("should handle zero", () => {
    expect(formatSecondsToTimestamp(0)).toBe("0:00");
  });

  it("should handle large hours", () => {
    expect(formatSecondsToTimestamp(360000)).toBe("100:00:00");
    expect(formatSecondsToTimestamp(3599999)).toBe("999:59:59");
  });

  it("should pad minutes and seconds", () => {
    expect(formatSecondsToTimestamp(3661)).toBe("1:01:01");
    expect(formatSecondsToTimestamp(3)).toBe("0:03");
  });
});
