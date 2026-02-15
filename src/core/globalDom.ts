/**
 * Shared Global DOM Setup for BotGuard
 *
 * BotGuard requires a DOM environment. This module sets up JSDOM globals
 * once and guards against double-initialization.
 */

import { JSDOM } from "jsdom";
import { USER_AGENTS } from "../constants.js";

let domInitialized = false;

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
