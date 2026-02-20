/**
 * HTTP Client Utilities
 *
 * Provides HTTP request helpers with:
 * - Automatic retry logic with exponential backoff (via p-retry)
 * - Timeout support
 * - fetch-compatible retry wrapper for BotGuard integration
 */

import pRetry from "p-retry";
import { Logger } from "./logger.js";

/**
 * Fetch with timeout and automatic retry on transient failures.
 * Automatically retries on 5xx errors and 429 rate limits with exponential backoff.
 *
 * @param url - URL to fetch
 * @param init - Fetch options
 * @param timeout - Timeout in milliseconds (default: 30000)
 * @param retryOptions - Optional retry configuration
 * @returns Response object
 */
export async function fetchWithTimeout(
  url: string | URL | Request,
  init?: RequestInit,
  timeout = 30_000,
  retryOptions?: { retries?: number },
): Promise<Response> {
  return pRetry(
    async () => {
      const timeoutSignal = AbortSignal.timeout(timeout);
      const combined = init?.signal
        ? AbortSignal.any([init.signal, timeoutSignal])
        : timeoutSignal;

      const response = await fetch(url, { ...init, signal: combined });

      // Retry on 5xx errors and 429 rate limit
      if (!response.ok && (response.status >= 500 || response.status === 429)) {
        throw new Error(`HTTP ${response.status}: ${response.statusText}`);
      }

      return response;
    },
    {
      retries: retryOptions?.retries ?? 3,
      onFailedAttempt: (error) => {
        try {
          Logger.getInstance().debug(
            `[HTTP] Attempt ${error.attemptNumber} failed. Retries left: ${error.retriesLeft}`,
          );
        } catch {
          // Logger may not be initialized yet
        }
      },
    },
  );
}

/**
 * Create a fetch-compatible function with retry logic and timeout.
 * Compatible with BotGuard's fetch parameter signature (typeof fetch).
 * Uses p-retry for standardized exponential backoff.
 */
export function createRetryFetch(options?: {
  maxRetries?: number;
  timeout?: number;
  headers?: Record<string, string>;
}): typeof fetch {
  const maxRetries = options?.maxRetries ?? 3;
  const timeout = options?.timeout ?? 30_000;
  const defaultHeaders = options?.headers ?? {};

  return async (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.toString()
          : input.url;

    return pRetry(
      async () => {
        const response = await fetch(url, {
          ...init,
          signal: init?.signal || AbortSignal.timeout(timeout),
          headers: { ...defaultHeaders, ...init?.headers },
        });

        // Retry on 5xx errors and 429 rate limit (same as fetchWithTimeout)
        if (!response.ok && (response.status >= 500 || response.status === 429)) {
          throw new Error(`HTTP ${response.status}: ${response.statusText}`);
        }

        return response;
      },
      {
        retries: maxRetries,
        onFailedAttempt: (error) => {
          try {
            Logger.getInstance().debug(
              `[HTTP] Retry fetch attempt ${error.attemptNumber} failed for ${url}. Retries left: ${error.retriesLeft}`,
            );
          } catch {
            // Logger may not be initialized yet
          }
        },
      },
    );
  };
}
