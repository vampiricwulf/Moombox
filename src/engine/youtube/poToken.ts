/**
 * YouTube PO Token Generator
 *
 * Generates Proof of Origin tokens using BotGuard.
 */

import { BG } from "../../bgutils/index.js";
import { Logger } from "../../core/logger.js";
import { setupGlobalDom } from "../../core/globalDom.js";
import { USER_AGENTS, BOTGUARD_REQUEST_KEY } from "../../constants.js";
import { createRetryFetch } from "../../core/http.js";
import { getErrorMessage } from "../../types/errors.js";

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
    }

    return "";
  }
}
