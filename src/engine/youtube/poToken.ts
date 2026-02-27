/**
 * YouTube PO Token Generator
 *
 * Generates Proof of Origin tokens using BotGuard.
 */

import { BG } from "../../bgutils/index.js";
import { Logger } from "../../core/logger.js";
import { setupGlobalDom, teardownGlobalDom, interceptTimers, cleanupBotGuardState } from "../../core/globalDom.js";
import { USER_AGENTS, BOTGUARD_REQUEST_KEY } from "../../constants.js";
import { createRetryFetch } from "../../core/http.js";
import { getErrorMessage } from "../../types/errors.js";

/**
 * Apply a GVS PO token to a URL.
 *
 * @param url - The URL to modify
 * @param token - The PO token value
 * @param mode - "path" appends `/pot/{token}` (for manifests); "query" sets `?pot={token}` (for format/segment URLs)
 */
export function applyGvsToken(url: string, token: string, mode: "query" | "path"): string {
  if (!token) return url;

  if (mode === "path") {
    // Remove trailing slash if present, then append /pot/{token}
    return url.replace(/\/?$/, `/pot/${token}`);
  }

  // Query mode: set pot= query parameter
  const parsed = new URL(url);
  parsed.searchParams.set("pot", token);
  return parsed.toString();
}

/**
 * PO Token Generator
 */
export class PoTokenGenerator {
  private logger: Logger;
  private poToken: string = "";
  private visitorData: string = "";

  constructor() {
    this.logger = Logger.getInstance();
  }

  /**
   * Set visitor data for token generation
   */
  setVisitorData(visitorData: string): void {
    this.visitorData = visitorData;
  }

  /**
   * Get the current visitor data
   */
  getVisitorData(): string {
    return this.visitorData;
  }

  /**
   * Get the generated PO token
   */
  getPoToken(): string {
    return this.poToken;
  }

  /**
   * Generate a new PO token
   */
  async generate(): Promise<string> {
    setupGlobalDom();

    const identifier = this.visitorData || "default_identifier";

    const bgConfig = {
      fetch: createRetryFetch({ headers: { "User-Agent": USER_AGENTS.WEB } }),
      globalObj: globalThis,
      identifier,
      requestKey: BOTGUARD_REQUEST_KEY,
    };

    // Intercept setInterval calls from BotGuard's interpreter — it creates
    // persistent telemetry timers that leak ~1MB/30s in long-running Node.js.
    const cleanupTimers = interceptTimers();

    try {
      const bgChallenge = await BG.Challenge.create(bgConfig);
      if (!bgChallenge) {
        this.logger.debug("[PoToken] No BotGuard challenge returned");
        return "";
      }

      if (
        bgChallenge.interpreterJavascript
          ?.privateDoNotAccessOrElseSafeScriptWrappedValue
      ) {
        new Function(
          bgChallenge.interpreterJavascript
            .privateDoNotAccessOrElseSafeScriptWrappedValue,
        )();
      }

      const poTokenResult = await BG.PoToken.generate({
        program: bgChallenge.program,
        globalName: bgChallenge.globalName,
        bgConfig,
      });

      // Clean up BotGuard state — no minter is cached in this path,
      // so we can fully tear down JSDOM to free ~30-50MB of DOM state.
      cleanupBotGuardState(bgChallenge.globalName);
      teardownGlobalDom();

      if (poTokenResult?.poToken) {
        this.poToken = poTokenResult.poToken;
        this.logger.debug(
          `[PoToken] Generated: ${this.poToken.substring(0, 30)}...`,
        );
        return this.poToken;
      }
    } catch (e) {
      const msg = getErrorMessage(e);
      this.logger.debug(
        `[PoToken] Generation failed (may not be needed): ${msg}`,
      );
    } finally {
      cleanupTimers();
      // Ensure JSDOM is torn down even on error
      teardownGlobalDom();
    }

    return "";
  }
}
