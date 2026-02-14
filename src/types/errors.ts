/**
 * Custom error types for better error handling
 */

/**
 * Base error class for all Moombox errors
 */
export class MoomboxError extends Error {
  constructor(
    message: string,
    public readonly code: string,
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
}

/**
 * YouTube API related errors
 */
export class YouTubeError extends MoomboxError {
  constructor(
    message: string,
    code: string = "YOUTUBE_ERROR",
    cause?: Error,
  ) {
    super(message, code, cause);
    this.name = "YouTubeError";
  }
}

/**
 * Video playability errors (member-only, unavailable, etc.)
 */
export class VideoPlayabilityError extends MoomboxError {
  constructor(
    message: string,
    public readonly playabilityStatus: string,
    public readonly reason?: string,
  ) {
    super(message, `PLAYABILITY_${playabilityStatus.toUpperCase()}`);
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
    super(message, code, cause);
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
    super(message, `HTTP_${httpStatus || "UNKNOWN"}`, cause);
    this.name = "NetworkError";
  }
}

/**
 * Configuration errors
 */
export class ConfigError extends MoomboxError {
  constructor(message: string, cause?: Error) {
    super(message, "CONFIG_ERROR", cause);
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
    super(message, "MUXING_ERROR", cause);
    this.name = "MuxingError";
  }
}

/**
 * Cookie/Authentication errors
 */
export class AuthenticationError extends MoomboxError {
  constructor(message: string, cause?: Error) {
    super(message, "AUTH_ERROR", cause);
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
    return new MoomboxError(error.message, "UNKNOWN_ERROR", error);
  }
  return new MoomboxError(
    typeof error === "string" ? error : defaultMessage,
    "UNKNOWN_ERROR",
  );
}
