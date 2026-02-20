/**
 * POT Provider Service
 *
 * Provides PO Token generation compatible with bgutil-ytdlp-pot-provider.
 * This allows yt-dlp to use Moombox as a POT provider server.
 *
 * Uses JSDOM directly for BotGuard VM execution.
 */

import { BG, buildURL, getHeaders } from "../bgutils/index.js";
import { Logger } from "./logger.js";
import { setupGlobalDom } from "./globalDom.js";
import { USER_AGENTS, BOTGUARD_REQUEST_KEY } from "../constants.js";
import { createRetryFetch } from "./http.js";
import type {
  IntegrityTokenData,
  WebPoSignalOutput,
} from "../bgutils/utils/types.js";

const VERSION = "1.0.0";

interface TokenMinter {
  expiry: Date;
  integrityToken: string;
  minter: InstanceType<typeof BG.WebPoMinter>;
}

interface SessionData {
  poToken: string;
  visitedData?: string;
  contentBinding: string;
}

interface SessionDataCache {
  [contentBinding: string]: {
    poToken: string;
    contentBinding: string;
    expiresAt: Date;
  };
}

/**
 * POT Provider - compatible with bgutil-ytdlp-pot-provider
 */
export class PotProvider {
  private static instance: PotProvider;
  private logger: Logger;
  private minterCache: Map<string, TokenMinter> = new Map();
  private sessionCache: SessionDataCache = {};
  private tokenTtlHours = 6;
  private inflight: Map<string, Promise<SessionData>> = new Map();

  private constructor() {
    this.logger = Logger.getInstance();
  }

  static getInstance(): PotProvider {
    if (!PotProvider.instance) {
      PotProvider.instance = new PotProvider();
    }
    return PotProvider.instance;
  }

  static getVersion(): string {
    return VERSION;
  }

  /**
   * Get the minter cache (for debugging)
   */
  getMinterCache(): Map<string, TokenMinter> {
    return this.minterCache;
  }

  /**
   * Invalidate all caches
   */
  invalidateCaches(): void {
    this.sessionCache = {};
    this.minterCache.clear();
    this.logger.debug("[PotProvider] All caches invalidated");
  }

  /**
   * Invalidate integrity tokens (force refresh on next request)
   */
  invalidateIntegrityTokens(): void {
    this.minterCache.forEach((minter) => {
      minter.expiry = new Date(0);
    });
    this.logger.debug("[PotProvider] Integrity tokens invalidated");
  }

  /**
   * Clean up expired session cache entries
   */
  private cleanupCaches(): void {
    const now = new Date();
    for (const contentBinding in this.sessionCache) {
      const session = this.sessionCache[contentBinding];
      if (session && now > session.expiresAt) {
        delete this.sessionCache[contentBinding];
      }
    }
  }

  /**
   * Generate visitor data if none provided
   */
  async generateVisitorData(): Promise<string> {
    const timestamp = Date.now();
    const random = Math.random().toString(36).substring(2, 15);
    return Buffer.from(`${timestamp}-${random}`).toString("base64");
  }

  /**
   * Generate a token minter
   */
  private async generateTokenMinter(cacheKey: string): Promise<TokenMinter> {
    setupGlobalDom();

    const fetchWithRetry = createRetryFetch({ headers: { "User-Agent": USER_AGENTS.WEB } });

    const bgConfig = {
      fetch: fetchWithRetry,
      globalObj: globalThis,
      identifier: cacheKey,
      requestKey: BOTGUARD_REQUEST_KEY,
    };

    // Get challenge
    this.logger.debug("[PotProvider] Fetching BotGuard challenge...");
    const bgChallenge = await BG.Challenge.create(bgConfig);
    if (!bgChallenge) {
      throw new Error("Could not get BotGuard challenge");
    }
    this.logger.debug(
      `[PotProvider] Challenge received, globalName: ${bgChallenge.globalName}`,
    );

    // Load interpreter
    if (
      bgChallenge.interpreterJavascript
        ?.privateDoNotAccessOrElseSafeScriptWrappedValue
    ) {
      this.logger.debug("[PotProvider] Loading BotGuard interpreter...");
      new Function(
        bgChallenge.interpreterJavascript
          .privateDoNotAccessOrElseSafeScriptWrappedValue,
      )();
      this.logger.debug("[PotProvider] Interpreter loaded");
    } else {
      throw new Error("Could not load BotGuard VM");
    }

    // Create BotGuard client
    this.logger.debug("[PotProvider] Creating BotGuard client...");
    const bgClient = await BG.BotGuardClient.create({
      program: bgChallenge.program,
      globalName: bgChallenge.globalName,
      globalObj: globalThis,
    });
    this.logger.debug("[PotProvider] BotGuard client created");

    // Generate integrity token
    const webPoSignalOutput: WebPoSignalOutput = [];
    this.logger.debug("[PotProvider] Generating BotGuard snapshot...");
    const botguardResponse = await bgClient.snapshot(
      { webPoSignalOutput },
      30_000,
    );
    this.logger.debug(
      `[PotProvider] BotGuard response length: ${botguardResponse?.length || 0}`,
    );

    const generateItUrl = buildURL("GenerateIT");
    this.logger.debug(`[PotProvider] Calling ${generateItUrl}`);

    const integrityTokenResp = await fetchWithRetry(generateItUrl, {
      method: "POST",
      headers: getHeaders(),
      body: JSON.stringify([BOTGUARD_REQUEST_KEY, botguardResponse]),
    });

    this.logger.debug(
      `[PotProvider] GenerateIT response status: ${integrityTokenResp.status}`,
    );

    if (!integrityTokenResp.ok) {
      const errorText = await integrityTokenResp
        .text()
        .catch(() => "Unknown error");
      throw new Error(
        `GenerateIT failed: ${integrityTokenResp.status} - ${errorText}`,
      );
    }

    const integrityTokenData = (await integrityTokenResp.json()) as [
      string,
      number,
      number,
      string,
    ];

    this.logger.debug(
      `[PotProvider] IntegrityTokenData received: ${JSON.stringify(integrityTokenData).substring(0, 100)}...`,
    );

    const [integrityToken, estimatedTtlSecs] = integrityTokenData;

    if (!integrityToken) {
      throw new Error(
        `Failed to generate integrity token: ${JSON.stringify(integrityTokenData)}`,
      );
    }

    this.logger.debug(
      `[PotProvider] Generated integrity token (TTL: ${estimatedTtlSecs}s)`,
    );

    const tokenMinter: TokenMinter = {
      expiry: new Date(Date.now() + estimatedTtlSecs * 1000),
      integrityToken,
      minter: await BG.WebPoMinter.create(
        {
          integrityToken,
          estimatedTtlSecs,
          mintRefreshThreshold: integrityTokenData[2],
          websafeFallbackToken: integrityTokenData[3],
        } as IntegrityTokenData,
        webPoSignalOutput,
      ),
    };

    this.minterCache.set(cacheKey, tokenMinter);
    return tokenMinter;
  }

  /**
   * Mint a PO token for a content binding
   */
  private async mintPoToken(
    contentBinding: string,
    tokenMinter: TokenMinter,
  ): Promise<SessionData> {
    this.logger.debug(`[PotProvider] Minting POT for ${contentBinding}`);

    const poToken =
      await tokenMinter.minter.mintAsWebsafeString(contentBinding);

    if (!poToken) {
      throw new Error("Failed to mint PO token");
    }

    this.logger.debug(
      `[PotProvider] POT generated: ${poToken.substring(0, 30)}...`,
    );

    const sessionData = {
      poToken,
      contentBinding,
      expiresAt: new Date(Date.now() + this.tokenTtlHours * 3_600_000),
    };

    this.sessionCache[contentBinding] = sessionData;

    return {
      poToken,
      contentBinding,
    };
  }

  /**
   * Generate a PO token - main entry point compatible with bgutil-ytdlp-pot-provider
   */
  async generatePoToken(
    contentBinding?: string,
    bypassCache = false,
  ): Promise<SessionData> {
    // Generate visitor data if not provided
    if (!contentBinding) {
      this.logger.debug(
        "[PotProvider] No content binding provided, generating visitor data",
      );
      contentBinding = await this.generateVisitorData();
    }

    this.cleanupCaches();

    const cacheKey = contentBinding;

    // Check session cache first
    if (!bypassCache && this.sessionCache[contentBinding]) {
      const cached = this.sessionCache[contentBinding];
      if (new Date() < cached.expiresAt) {
        this.logger.debug(
          `[PotProvider] Returning cached POT for ${contentBinding}`,
        );
        return {
          poToken: cached.poToken,
          contentBinding: cached.contentBinding,
        };
      }
    }

    // Deduplicate concurrent requests for the same key
    const existing = this.inflight.get(cacheKey);
    if (existing) return existing;

    const promise = this.doGeneratePoToken(contentBinding, cacheKey, bypassCache)
      .finally(() => this.inflight.delete(cacheKey));
    this.inflight.set(cacheKey, promise);
    return promise;
  }

  private async doGeneratePoToken(
    contentBinding: string,
    cacheKey: string,
    bypassCache: boolean,
  ): Promise<SessionData> {
    // Check minter cache
    if (!bypassCache) {
      let tokenMinter = this.minterCache.get(cacheKey);
      if (tokenMinter) {
        // Refresh if expired
        if (new Date() >= tokenMinter.expiry) {
          this.logger.debug("[PotProvider] Minter expired, regenerating");
          tokenMinter = await this.generateTokenMinter(cacheKey);
        }
        return await this.mintPoToken(contentBinding, tokenMinter);
      }
    }

    // Generate new minter
    const tokenMinter = await this.generateTokenMinter(cacheKey);
    return await this.mintPoToken(contentBinding, tokenMinter);
  }

  /**
   * Shutdown the POT provider
   */
  shutdown(): void {
    // No-op - no subprocess to stop
  }
}
