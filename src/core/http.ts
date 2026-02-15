/**
 * HTTP Client Utilities
 *
 * Provides HTTP request helpers with:
 * - Automatic retry logic with exponential backoff (via p-retry)
 * - Timeout support
 * - Modern ky HTTP client for advanced use cases
 */

import pRetry, { AbortError } from "p-retry";
import ky from "ky";

/**
 * Modern HTTP client with built-in retry and timeout
 * Use this for advanced HTTP requests with automatic retries
 */
export const httpClient = ky.create({
  timeout: 30000,
  retry: {
    limit: 3,
    statusCodes: [408, 413, 429, 500, 502, 503, 504],
    backoffLimit: 30000,
  },
  hooks: {
    beforeRetry: [
      ({ request, options, error, retryCount }) => {
        console.log(`Retrying ${request.url} (attempt ${retryCount + 1})`);
      },
    ],
  },
});

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
  timeout = 30000,
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
        console.log(
          `[HTTP] Attempt ${error.attemptNumber} failed. Retries left: ${error.retriesLeft}`,
        );
      },
    },
  );
}

/**
 * Create a fetch-compatible function with retry logic and timeout.
 * Compatible with BotGuard's fetch parameter signature (typeof fetch).
 */
export function createRetryFetch(options?: {
  maxRetries?: number;
  retryDelay?: number;
  timeout?: number;
  headers?: Record<string, string>;
}): typeof fetch {
  const maxRetries = options?.maxRetries ?? 3;
  const retryDelay = options?.retryDelay ?? 2000;
  const timeout = options?.timeout ?? 30000;
  const defaultHeaders = options?.headers ?? {};

  return async (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.toString()
          : input.url;

    for (let attempt = 1; attempt <= maxRetries; attempt++) {
      try {
        return await fetch(url, {
          ...init,
          signal: init?.signal || AbortSignal.timeout(timeout),
          headers: { ...defaultHeaders, ...init?.headers },
        });
      } catch (e) {
        if (attempt === maxRetries) throw e;
        await new Promise((r) => setTimeout(r, retryDelay));
      }
    }
    throw new Error("All fetch attempts failed");
  };
}

/**
 * Generic retry with exponential backoff for any async operation.
 * Uses p-retry for standardized retry logic.
 */
export async function retryWithBackoff<T>(
  fn: () => Promise<T>,
  options?: {
    maxRetries?: number;
    signal?: AbortSignal;
  },
): Promise<T> {
  return pRetry(
    async () => {
      if (options?.signal?.aborted) {
        throw new AbortError("Aborted");
      }
      return await fn();
    },
    {
      retries: options?.maxRetries ?? 3,
    },
  );
}
