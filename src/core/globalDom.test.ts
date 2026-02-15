import { describe, it, expect, beforeAll } from "vitest";
import { setupGlobalDom } from "./globalDom.js";

describe("globalDom (jsdom)", () => {
  beforeAll(() => {
    setupGlobalDom();
  });

  it("should set up globalThis.window", () => {
    expect(globalThis.window).toBeDefined();
    expect(typeof globalThis.window).toBe("object");
  });

  it("should set up globalThis.document", () => {
    expect(globalThis.document).toBeDefined();
    expect(globalThis.document.createElement).toBeTypeOf("function");
  });

  it("should set up navigator (via window)", () => {
    // BotGuard accesses navigator through window, not globalThis directly.
    // Node.js 22+ defines globalThis.navigator natively, so the guard in
    // setupGlobalDom skips it — but window.navigator is always set by JSDOM.
    expect(globalThis.navigator).toBeDefined();
    const win = globalThis.window as any;
    expect(win.navigator.userAgent).toContain("Chrome");
  });

  it("should set up globalThis.location with YouTube origin", () => {
    expect(globalThis.location).toBeDefined();
    expect(globalThis.location.href).toBe("https://www.youtube.com/");
  });

  it("should support document.createElement", () => {
    const div = globalThis.document.createElement("div");
    expect(div).toBeDefined();
    expect(div.tagName).toBe("DIV");
  });

  it("should support new Function() execution (BotGuard VM)", () => {
    // BotGuard creates VMs via new Function(interpreterJS)
    const fn = new Function("return 42");
    expect(fn()).toBe(42);
  });

  it("should support TextEncoder/TextDecoder (used by BotGuard)", () => {
    const encoder = new TextEncoder();
    const decoder = new TextDecoder();
    const encoded = encoder.encode("test");
    expect(encoded).toBeInstanceOf(Uint8Array);
    expect(decoder.decode(encoded)).toBe("test");
  });

  it("should support setTimeout/clearTimeout in DOM context", () => {
    // BotGuard uses timeouts for VM callback handling
    expect(typeof setTimeout).toBe("function");
    expect(typeof clearTimeout).toBe("function");
  });

  it("should support fetch (used by BotGuard challenge fetcher)", () => {
    expect(typeof fetch).toBe("function");
  });

  it("should be idempotent (safe to call multiple times)", () => {
    // Should not throw on second call
    setupGlobalDom();
    expect(globalThis.window).toBeDefined();
  });
});
