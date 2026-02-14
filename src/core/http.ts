/**
 * HTTP Client Abstraction
 *
 * Provides centralized HTTP request handling with:
 * - Retry logic with exponential backoff
 * - Consistent headers
 * - Request/response logging
 * - Error handling
 */

import { Logger } from "./logger.js";
import {
  NetworkError,
  DownloadError,
  getErrorMessage,
} from "../types/errors.js";
import {
  USER_AGENTS,
  MAX_DOWNLOAD_RETRIES,
  RETRY_DELAY_MS,
} from "../constants.js";

/**
 * HTTP request options
 */
export interface HttpRequestOptions {
  method?: "GET" | "POST" | "HEAD";
  headers?: Record<string, string>;
  body?: BodyInit;
  timeout?: number;
  retries?: number;
  retryDelay?: number;
  throwOnError?: boolean;
}

/**
 * HTTP response wrapper
 */
export interface HttpResponse<T = unknown> {
  ok: boolean;
  status: number;
  statusText: string;
  headers: Headers;
  data: T;
}

/**
 * Default request options
 */
const DEFAULT_OPTIONS: Required<
  Pick<HttpRequestOptions, "method" | "retries" | "retryDelay" | "throwOnError">
> = {
  method: "GET",
  retries: MAX_DOWNLOAD_RETRIES,
  retryDelay: RETRY_DELAY_MS,
  throwOnError: true,
};

/**
 * HTTP Client class
 */
export class HttpClient {
  private logger: Logger;
  private defaultHeaders: Record<string, string>;

  constructor(defaultHeaders?: Record<string, string>) {
    this.logger = Logger.getInstance();
    this.defaultHeaders = {
      "User-Agent": USER_AGENTS.WEB,
      ...defaultHeaders,
    };
  }

  /**
   * Set default headers for all requests
   */
  setDefaultHeaders(headers: Record<string, string>): void {
    this.defaultHeaders = { ...this.defaultHeaders, ...headers };
  }

  /**
   * Make an HTTP request with retry logic
   */
  async request<T = unknown>(
    url: string,
    options: HttpRequestOptions = {},
  ): Promise<HttpResponse<T>> {
    const opts = { ...DEFAULT_OPTIONS, ...options };
    const headers = { ...this.defaultHeaders, ...options.headers };

    let lastError: Error | null = null;

    for (let attempt = 1; attempt <= opts.retries; attempt++) {
      try {
        const controller = new AbortController();
        const timeoutId = opts.timeout
          ? setTimeout(() => controller.abort(), opts.timeout)
          : undefined;

        const response = await fetch(url, {
          method: opts.method,
          headers,
          body: opts.body,
          signal: controller.signal,
        });

        if (timeoutId) clearTimeout(timeoutId);

        // Handle response based on content type
        let data: T;
        const contentType = response.headers.get("content-type") || "";

        if (contentType.includes("application/json")) {
          data = (await response.json()) as T;
        } else if (
          contentType.includes("text/") ||
          contentType.includes("application/xml")
        ) {
          data = (await response.text()) as T;
        } else {
          data = (await response.arrayBuffer()) as T;
        }

        if (!response.ok && opts.throwOnError) {
          throw new NetworkError(
            `HTTP ${response.status}: ${response.statusText}`,
            response.status,
            url,
          );
        }

        return {
          ok: response.ok,
          status: response.status,
          statusText: response.statusText,
          headers: response.headers,
          data,
        };
      } catch (error) {
        lastError =
          error instanceof Error ? error : new Error(getErrorMessage(error));

        // Don't retry on certain errors
        if (error instanceof NetworkError) {
          const status = error.httpStatus;
          // Don't retry on client errors (except 429 rate limit)
          if (status && status >= 400 && status < 500 && status !== 429) {
            throw error;
          }
        }

        // Log retry attempt
        if (attempt < opts.retries) {
          this.logger.debug(
            `[HTTP] Request failed (attempt ${attempt}/${opts.retries}): ${lastError.message}`,
          );
          await this.delay(opts.retryDelay * attempt); // Exponential backoff
        }
      }
    }

    throw new NetworkError(
      `Request failed after ${opts.retries} attempts: ${lastError?.message}`,
      undefined,
      url,
      lastError || undefined,
    );
  }

  /**
   * Convenience method for GET requests
   */
  async get<T = unknown>(
    url: string,
    options?: Omit<HttpRequestOptions, "method" | "body">,
  ): Promise<HttpResponse<T>> {
    return this.request<T>(url, { ...options, method: "GET" });
  }

  /**
   * Convenience method for POST requests
   */
  async post<T = unknown>(
    url: string,
    body?: unknown,
    options?: Omit<HttpRequestOptions, "method">,
  ): Promise<HttpResponse<T>> {
    const bodyStr =
      typeof body === "string" ? body : body ? JSON.stringify(body) : undefined;
    const headers = {
      ...options?.headers,
      ...(body && typeof body !== "string"
        ? { "Content-Type": "application/json" }
        : {}),
    };
    return this.request<T>(url, { ...options, method: "POST", body: bodyStr, headers });
  }

  /**
   * Convenience method for HEAD requests
   */
  async head(
    url: string,
    options?: Omit<HttpRequestOptions, "method" | "body">,
  ): Promise<HttpResponse<null>> {
    return this.request<null>(url, { ...options, method: "HEAD" });
  }

  /**
   * Download a file with progress tracking
   */
  async downloadFile(
    url: string,
    options?: {
      headers?: Record<string, string>;
      onProgress?: (downloaded: number, total: number | null) => void;
      chunkSize?: number;
    },
  ): Promise<Buffer> {
    const chunkSize = options?.chunkSize || 5 * 1024 * 1024; // 5MB default
    const headers = {
      ...this.defaultHeaders,
      "User-Agent": USER_AGENTS.ANDROID, // Use Android UA for downloads
      ...options?.headers,
    };

    // First, try to get file size with Range request
    let totalBytes: number | null = null;
    try {
      const probeResponse = await fetch(url, {
        method: "GET",
        headers: { ...headers, Range: "bytes=0-0" },
      });

      if (probeResponse.status === 206) {
        const contentRange = probeResponse.headers.get("content-range");
        if (contentRange) {
          const match = contentRange.match(/bytes \d+-\d+\/(\d+)/);
          if (match) {
            totalBytes = parseInt(match[1], 10);
          }
        }
      }
      // Drain probe response
      await probeResponse.arrayBuffer();
    } catch {
      // Ignore probe errors
    }

    // Download in chunks
    const chunks: Buffer[] = [];
    let downloadedBytes = 0;

    while (true) {
      const start = downloadedBytes;
      const end =
        totalBytes !== null
          ? Math.min(start + chunkSize - 1, totalBytes - 1)
          : start + chunkSize - 1;

      const response = await fetch(url, {
        headers: { ...headers, Range: `bytes=${start}-${end}` },
      });

      if (response.status === 416) {
        // Range not satisfiable - done
        break;
      }

      if (!response.ok && response.status !== 206) {
        throw new DownloadError(
          `Download failed: HTTP ${response.status}`,
          "DOWNLOAD_HTTP_ERROR",
          response.status,
        );
      }

      const data = await response.arrayBuffer();
      const buffer = Buffer.from(data);

      if (buffer.length === 0) break;

      chunks.push(buffer);
      downloadedBytes += buffer.length;

      if (options?.onProgress) {
        options.onProgress(downloadedBytes, totalBytes);
      }

      // If server returned 200 instead of 206, we got everything
      if (response.status === 200) break;

      // If we've downloaded everything
      if (totalBytes !== null && downloadedBytes >= totalBytes) break;
    }

    return Buffer.concat(chunks);
  }

  /**
   * Helper to delay execution
   */
  private delay(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}

/**
 * Singleton HTTP client instance
 */
let httpClientInstance: HttpClient | null = null;

/**
 * Get the singleton HTTP client
 */
export function getHttpClient(): HttpClient {
  if (!httpClientInstance) {
    httpClientInstance = new HttpClient();
  }
  return httpClientInstance;
}

/**
 * Create a new HTTP client with custom headers
 */
export function createHttpClient(
  defaultHeaders?: Record<string, string>,
): HttpClient {
  return new HttpClient(defaultHeaders);
}

/**
 * Simple fetch with timeout. Returns raw Response.
 * If the caller provides a signal in init, it is combined with the timeout
 * signal using AbortSignal.any() so that either can cancel the request.
 */
export async function fetchWithTimeout(
  url: string | URL | Request,
  init?: RequestInit,
  timeout = 30000,
): Promise<Response> {
  const timeoutSignal = AbortSignal.timeout(timeout);
  if (init?.signal) {
    const combined = AbortSignal.any([init.signal, timeoutSignal]);
    return fetch(url, { ...init, signal: combined });
  }
  return fetch(url, { ...init, signal: timeoutSignal });
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
 */
export async function retryWithBackoff<T>(
  fn: () => Promise<T>,
  options?: {
    maxRetries?: number;
    baseDelay?: number;
    maxDelay?: number;
    signal?: AbortSignal;
  },
): Promise<T> {
  const maxRetries = options?.maxRetries ?? 3;
  const baseDelay = options?.baseDelay ?? 2000;
  const maxDelay = options?.maxDelay ?? 30000;

  let lastError: Error | null = null;

  for (let attempt = 1; attempt <= maxRetries; attempt++) {
    try {
      if (options?.signal?.aborted) throw new Error("Aborted");
      return await fn();
    } catch (e) {
      lastError = e instanceof Error ? e : new Error(String(e));
      if (attempt === maxRetries) throw lastError;
      const delay = Math.min(baseDelay * attempt, maxDelay);
      await new Promise((r) => setTimeout(r, delay));
    }
  }
  throw lastError || new Error("All retry attempts failed");
}
