/**
 * Custom error types for better error handling
 */

/**
 * Error context - additional structured information about the error
 */
export interface ErrorContext {
  [key: string]: unknown;
}

/**
 * Base error class for all Moombox errors
 */
export class MoomboxError extends Error {
  constructor(
    message: string,
    public readonly code: string,
    public readonly expected: boolean = false,  // User-facing vs developer error
    public readonly context: ErrorContext = {},  // Contextual information
    public readonly cause?: Error,
  ) {
    super(message);
    this.name = "MoomboxError";
    // Maintains proper stack trace for where error was thrown
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, this.constructor);
    }
  }

  /**
   * Get error message including cause if present
   */
  get fullMessage(): string {
    if (this.cause) {
      return `${this.message}: ${this.cause.message}`;
    }
    return this.message;
  }

  /**
   * Convert error to JSON for logging/transmission
   */
  toJSON(): Record<string, unknown> {
    return {
      name: this.name,
      message: this.message,
      code: this.code,
      expected: this.expected,
      context: this.context,
      cause: this.cause?.message,
      stack: this.stack,
    };
  }
}

/**
 * YouTube API related errors
 */
export class YouTubeError extends MoomboxError {
  constructor(
    message: string,
    code: string = "YOUTUBE_ERROR",
    context: ErrorContext = {},
    cause?: Error,
  ) {
    super(message, code, false, context, cause);
    this.name = "YouTubeError";
  }
}

/**
 * Video playability errors (member-only, unavailable, etc.)
 * Marked as expected (user-facing) errors
 */
export class VideoPlayabilityError extends MoomboxError {
  constructor(
    message: string,
    public readonly playabilityStatus: string,
    public readonly reason?: string,
  ) {
    super(
      message,
      `PLAYABILITY_${playabilityStatus.toUpperCase()}`,
      true,  // expected = true (user-facing error)
      { playabilityStatus, reason }
    );
    this.name = "VideoPlayabilityError";
  }
}

/**
 * Download related errors
 */
export class DownloadError extends MoomboxError {
  constructor(
    message: string,
    code: string = "DOWNLOAD_ERROR",
    public readonly httpStatus?: number,
    cause?: Error,
  ) {
    super(message, code, false, { httpStatus }, cause);
    this.name = "DownloadError";
  }
}

/**
 * Network/HTTP errors
 */
export class NetworkError extends MoomboxError {
  constructor(
    message: string,
    public readonly httpStatus?: number,
    public readonly url?: string,
    cause?: Error,
  ) {
    super(message, `HTTP_${httpStatus || "UNKNOWN"}`, false, { httpStatus, url }, cause);
    this.name = "NetworkError";
  }
}

/**
 * Configuration errors
 */
export class ConfigError extends MoomboxError {
  constructor(message: string, cause?: Error) {
    super(message, "CONFIG_ERROR", false, {}, cause);
    this.name = "ConfigError";
  }
}

/**
 * FFmpeg/Muxing errors
 */
export class MuxingError extends MoomboxError {
  constructor(
    message: string,
    public readonly exitCode?: number,
    cause?: Error,
  ) {
    super(message, "MUXING_ERROR", false, { exitCode }, cause);
    this.name = "MuxingError";
  }
}

/**
 * Cookie/Authentication errors
 */
export class AuthenticationError extends MoomboxError {
  constructor(message: string, cause?: Error) {
    super(message, "AUTH_ERROR", false, {}, cause);
    this.name = "AuthenticationError";
  }
}

/**
 * Helper to extract error message from unknown error type
 */
export function getErrorMessage(error: unknown): string {
  if (error instanceof Error) {
    return error.message;
  }
  if (typeof error === "string") {
    return error;
  }
  return String(error);
}

/**
 * Helper to wrap unknown errors in MoomboxError
 */
export function wrapError(error: unknown, defaultMessage: string): MoomboxError {
  if (error instanceof MoomboxError) {
    return error;
  }
  if (error instanceof Error) {
    return new MoomboxError(error.message, "UNKNOWN_ERROR", false, {}, error);
  }
  return new MoomboxError(
    typeof error === "string" ? error : defaultMessage,
    "UNKNOWN_ERROR",
    false,
    {},
  );
}
