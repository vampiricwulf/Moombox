import { describe, it, expect } from "vitest";
import { formatBytes, formatElapsed, formatSpeed } from "../../utils/timeFormat.js";

describe("formatBytes", () => {
  it("should format bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(500)).toBe("500 B");
    expect(formatBytes(1023)).toBe("1023 B");
  });

  it("should format kilobytes", () => {
    expect(formatBytes(1024)).toBe("1.0 KB");
    expect(formatBytes(5120)).toBe("5.0 KB");
    expect(formatBytes(1536)).toBe("1.5 KB");
  });

  it("should format megabytes", () => {
    expect(formatBytes(1048576)).toBe("1.0 MB"); // 1024^2
    expect(formatBytes(5242880)).toBe("5.0 MB");
    expect(formatBytes(1572864)).toBe("1.5 MB");
  });

  it("should format gigabytes", () => {
    expect(formatBytes(1073741824)).toBe("1.00 GB"); // 1024^3
    expect(formatBytes(5368709120)).toBe("5.00 GB");
    expect(formatBytes(1610612736)).toBe("1.50 GB");
  });

  it("should handle decimal precision", () => {
    expect(formatBytes(1536)).toMatch(/1\.[45] KB/); // Allow rounding variance
    expect(formatBytes(1073741824 + 536870912)).toMatch(/1\.5[0-9] GB/);
  });
});

describe("formatElapsed", () => {
  it("should format milliseconds to seconds", () => {
    expect(formatElapsed(500)).toBe("0s");
    expect(formatElapsed(1000)).toBe("1s");
    expect(formatElapsed(5500)).toBe("5s");
  });

  it("should format seconds to minutes", () => {
    expect(formatElapsed(60000)).toBe("1m 0s");
    expect(formatElapsed(90000)).toBe("1m 30s");
    expect(formatElapsed(125000)).toBe("2m 5s");
  });

  it("should format minutes to hours", () => {
    expect(formatElapsed(3600000)).toBe("1h 0m");
    expect(formatElapsed(5400000)).toBe("1h 30m");
    expect(formatElapsed(7200000)).toBe("2h 0m");
  });

  it("should handle zero", () => {
    expect(formatElapsed(0)).toBe("0s");
  });

  it("should handle large values", () => {
    expect(formatElapsed(86400000)).toBe("24h 0m"); // 24 hours
    expect(formatElapsed(90061000)).toBe("25h 1m"); // 25 hours, 1 minute
  });
});

describe("formatSpeed", () => {
  it("should format bytes per second to B/s for < 1KB", () => {
    expect(formatSpeed(512)).toBe("512 B/s");
    expect(formatSpeed(100)).toBe("100 B/s");
    expect(formatSpeed(1)).toBe("1 B/s");
  });

  it("should format to KB/s", () => {
    expect(formatSpeed(1024)).toBe("1.0 KB/s");
    expect(formatSpeed(5120)).toBe("5.0 KB/s");
  });

  it("should format to MB/s", () => {
    expect(formatSpeed(1048576)).toBe("1.0 MB/s"); // 1024^2
    expect(formatSpeed(5242880)).toBe("5.0 MB/s");
  });

  it("should handle zero", () => {
    expect(formatSpeed(0)).toBe("0 B/s");
  });

  it("should not format to GB/s (maxes at MB/s)", () => {
    expect(formatSpeed(1073741824)).toMatch(/\d+\.\d MB\/s/);
  });
});
