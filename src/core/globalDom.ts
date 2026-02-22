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
 * Intercept `setInterval` and `setTimeout` calls to track timers created by BotGuard.
 *
 * BotGuard's interpreter (`new Function(js)()`) creates persistent `setInterval`
 * timers and one-shot `setTimeout` callbacks inside the JSDOM/Node.js environment
 * for monitoring and telemetry. In a browser these are harmless (page eventually
 * closes), but in a long-running Node.js server they leak memory by continuously
 * allocating DOM state.
 *
 * Call this BEFORE loading the BotGuard interpreter. Call the returned cleanup
 * function AFTER the minter/token is fully created to clear all tracked timers.
 */
export function interceptTimers(): () => void {
  const origGlobalSetInterval = globalThis.setInterval;
  const origGlobalSetTimeout = globalThis.setTimeout;
  const trackedIntervals: ReturnType<typeof setInterval>[] = [];
  const trackedTimeouts: ReturnType<typeof setTimeout>[] = [];

  // Intercept globalThis.setInterval (what `new Function(js)()` resolves to)
  globalThis.setInterval = ((...args: Parameters<typeof setInterval>) => {
    const id = origGlobalSetInterval(...args);
    trackedIntervals.push(id);
    return id;
  }) as typeof setInterval;

  // Intercept globalThis.setTimeout
  globalThis.setTimeout = ((...args: Parameters<typeof setTimeout>) => {
    const id = origGlobalSetTimeout(...args);
    trackedTimeouts.push(id);
    return id;
  }) as typeof setTimeout;

  // Also intercept window.setInterval/setTimeout (JSDOM's implementation) in case
  // BotGuard code explicitly calls window.setInterval(...) / window.setTimeout(...)
  let origWindowSetInterval: (typeof setInterval) | null = null;
  let origWindowSetTimeout: (typeof setTimeout) | null = null;
  if (domInstance) {
    const win = domInstance.window as Record<string, any>;

    origWindowSetInterval = win.setInterval as typeof setInterval;
    win.setInterval = (...args: any[]) => {
      const id = (origWindowSetInterval as Function).apply(win, args);
      trackedIntervals.push(id);
      return id;
    };

    origWindowSetTimeout = win.setTimeout as typeof setTimeout;
    win.setTimeout = (...args: any[]) => {
      const id = (origWindowSetTimeout as Function).apply(win, args);
      trackedTimeouts.push(id);
      return id;
    };
  }

  return () => {
    globalThis.setInterval = origGlobalSetInterval;
    globalThis.setTimeout = origGlobalSetTimeout;
    if (domInstance && origWindowSetInterval) {
      (domInstance.window as Record<string, any>).setInterval = origWindowSetInterval;
    }
    if (domInstance && origWindowSetTimeout) {
      (domInstance.window as Record<string, any>).setTimeout = origWindowSetTimeout;
    }
    for (const id of trackedIntervals) clearInterval(id);
    for (const id of trackedTimeouts) clearTimeout(id);
  };
}

/**
 * Clean up DOM state left by BotGuard's interpreter.
 *
 * BotGuard's `new Function(js)()` may inject script tags, add event listeners,
 * and create globals on the JSDOM window. This function removes that accumulated
 * state without tearing down the entire JSDOM instance (which would break any
 * cached minter callbacks that reference the window).
 *
 * @param globalName - The BotGuard VM global name to remove from globalThis
 */
export function cleanupBotGuardState(globalName?: string): void {
  // Remove BotGuard's VM global variable (large closure tree)
  if (globalName && globalName in globalThis) {
    delete (globalThis as Record<string, unknown>)[globalName];
  }

  // Clean JSDOM document — BotGuard may have injected script tags or other elements
  if (domInstance) {
    try {
      const doc = domInstance.window.document;
      // Clear body and head to remove any injected elements
      while (doc.body.firstChild) doc.body.removeChild(doc.body.firstChild);
      while (doc.head.childNodes.length > 1) {
        // Keep the <title> element (first child)
        const last = doc.head.lastChild;
        if (last) doc.head.removeChild(last);
      }
    } catch {
      // JSDOM may already be closed
    }
  }
}
