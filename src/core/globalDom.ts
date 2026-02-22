/**
 * Shared Global DOM Setup for BotGuard
 *
 * BotGuard requires a DOM environment. This module sets up JSDOM globals
 * once and guards against double-initialization.
 */

import { JSDOM } from "jsdom";
import { USER_AGENTS } from "../constants.js";

let domInitialized = false;
let domInstance: JSDOM | null = null;

/**
 * Setup global DOM for BotGuard using JSDOM.
 * Safe to call multiple times — only initializes once.
 */
export function setupGlobalDom(): void {
  if (domInitialized) return;

  const dom = new JSDOM(
    '<!DOCTYPE html><html lang="en"><head><title></title></head><body></body></html>',
    {
      url: "https://www.youtube.com/",
      referrer: "https://www.youtube.com/",
      // jsdom v28 moved userAgent into resources object, but @types/jsdom
      // v27 hasn't updated the type signature yet — cast to bypass.
      resources: { userAgent: USER_AGENTS.WEB } as any,
    },
  );

  domInstance = dom;

  Object.assign(globalThis, {
    window: dom.window,
    document: dom.window.document,
    location: dom.window.location,
    origin: dom.window.origin,
  });

  if (!Reflect.has(globalThis, "navigator")) {
    Object.defineProperty(globalThis, "navigator", {
      value: dom.window.navigator,
    });
  }

  domInitialized = true;
}

/**
 * Tear down the global DOM, releasing JSDOM resources.
 * Called during shutdown to free internal timers and DOM tree.
 */
export function teardownGlobalDom(): void {
  if (!domInitialized || !domInstance) return;

  domInstance.window.close();
  domInstance = null;
  domInitialized = false;
}

/**
 * Intercept `setInterval` calls to track persistent timers created by BotGuard.
 *
 * BotGuard's interpreter (`new Function(js)()`) creates persistent `setInterval`
 * timers inside the JSDOM/Node.js environment for monitoring and telemetry.
 * In a browser these are harmless (page eventually closes), but in a long-running
 * Node.js server they leak ~1MB/30s by continuously allocating DOM state.
 *
 * Call this BEFORE loading the BotGuard interpreter. Call the returned cleanup
 * function AFTER the minter/token is fully created to clear all tracked intervals.
 */
export function interceptTimers(): () => void {
  const origGlobalSetInterval = globalThis.setInterval;
  const tracked: ReturnType<typeof setInterval>[] = [];

  // Intercept globalThis.setInterval (what `new Function(js)()` resolves to)
  globalThis.setInterval = ((...args: Parameters<typeof setInterval>) => {
    const id = origGlobalSetInterval(...args);
    tracked.push(id);
    return id;
  }) as typeof setInterval;

  // Also intercept window.setInterval (JSDOM's implementation) in case
  // BotGuard code explicitly calls window.setInterval(...)
  let origWindowSetInterval: (typeof setInterval) | null = null;
  if (domInstance) {
    const win = domInstance.window as Record<string, any>;
    origWindowSetInterval = win.setInterval as typeof setInterval;
    win.setInterval = (...args: any[]) => {
      const id = (origWindowSetInterval as Function).apply(win, args);
      tracked.push(id);
      return id;
    };
  }

  return () => {
    globalThis.setInterval = origGlobalSetInterval;
    if (domInstance && origWindowSetInterval) {
      (domInstance.window as Record<string, any>).setInterval = origWindowSetInterval;
    }
    for (const id of tracked) clearInterval(id);
  };
}
