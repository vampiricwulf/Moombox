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
 * Create a managed AbortController with timeout and optional caller signal propagation.
 *
 * Unlike `AbortSignal.any()` + `AbortSignal.timeout()`, the timer is immediately
 * cleared when `cleanup()` is called. This prevents 30-second ghost timers from
 * accumulating in memory during high-frequency polling (e.g., chat downloader
 * every ~5s creates ~6 overlapping timeout signals at any given time). It also
 * avoids dead `WeakRef` accumulation on long-lived abort signals.
 */
function createTimeoutController(
  timeout: number,
  callerSignal?: AbortSignal | null,
): { signal: AbortSignal; cleanup: () => void } {
  const controller = new AbortController();
  const timer = setTimeout(
    () => controller.abort(new DOMException("The operation was aborted due to timeout", "TimeoutError")),
    timeout,
  );

  let onAbort: (() => void) | null = null;

  if (callerSignal) {
    if (callerSignal.aborted) {
      clearTimeout(timer);
      controller.abort(callerSignal.reason);
    } else {
      onAbort = () => {
        clearTimeout(timer);
        controller.abort(callerSignal.reason);
      };
      callerSignal.addEventListener("abort", onAbort, { once: true });
    }
  }

  return {
    signal: controller.signal,
    cleanup() {
      clearTimeout(timer);
      if (onAbort && callerSignal) {
        callerSignal.removeEventListener("abort", onAbort);
      }
    },
  };
}

/**
 * Fetch with timeout and automatic retry on transient failures.
 * Automatically retries on 5xx errors and 429 rate limits with exponential backoff.
 *
 * @param url - URL to fetch
 * @param init - Fetch options
 * @param timeout - Timeout in milliseconds (default: 30s)
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
      const { signal, cleanup } = createTimeoutController(timeout, init?.signal);
      try {
        const response = await fetch(url, { ...init, signal });

        // Retry on 5xx errors and 429 rate limit
        if (!response.ok && (response.status >= 500 || response.status === 429)) {
          // Drain body to prevent memory/socket leak from unconsumed responses
          await response.body?.cancel();
          throw new Error(`HTTP ${response.status}: ${response.statusText}`);
        }

        return response;
      } finally {
        cleanup();
      }
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
        const { signal, cleanup } = createTimeoutController(timeout, init?.signal);
        try {
          const response = await fetch(url, {
            ...init,
            signal,
            headers: { ...defaultHeaders, ...init?.headers },
          });

          // Retry on 5xx errors and 429 rate limit (same as fetchWithTimeout)
          if (!response.ok && (response.status >= 500 || response.status === 429)) {
            // Drain body to prevent memory/socket leak from unconsumed responses
            await response.body?.cancel();
            throw new Error(`HTTP ${response.status}: ${response.statusText}`);
          }

          return response;
        } finally {
          cleanup();
        }
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
