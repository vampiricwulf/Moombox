/**
 * Retry Manager
 *
 * Provides standardized retry logic with configurable backoff strategies.
 * Based on patterns from yt-dlp for reliable network operations.
 */

/**
 * Sleep function that returns delay in milliseconds based on attempt number
 */
export type SleepFunction = (attempt: number) => number;

/**
 * Retry configuration options
 */
export interface RetryOptions {
  /** Maximum number of retry attempts */
  maxRetries: number;

  /** Optional sleep function to determine delay between retries */
  sleepFunc?: SleepFunction;

  /** Optional callback invoked before each retry */
  onRetry?: (attempt: number, error: Error) => void;
}

/**
 * Retry Manager - handles retry logic with backoff strategies
 */
export class RetryManager {
  private attempt = 0;

  constructor(private options: RetryOptions) {}

  /**
   * Execute a function with retry logic
   *
   * @param fn - Async function to execute
   * @returns Result of the function
   * @throws Last error if all retries exhausted
   */
  async execute<T>(fn: () => Promise<T>): Promise<T> {
    while (true) {
      try {
        return await fn();
      } catch (error) {
        this.attempt++;

        if (this.attempt > this.options.maxRetries) {
          throw error;
        }

        // Notify retry callback
        this.options.onRetry?.(this.attempt, error as Error);

        // Calculate sleep duration
        const sleepMs = this.options.sleepFunc?.(this.attempt) ?? 1000;
        await new Promise(resolve => setTimeout(resolve, sleepMs));
      }
    }
  }

  /**
   * Exponential backoff strategy
   *
   * @param base - Base delay in milliseconds (default: 1000ms)
   * @param max - Maximum delay in milliseconds (default: 30000ms)
   * @returns Sleep function with exponential backoff
   */
  static exponentialBackoff(base = 1000, max = 30000): SleepFunction {
    return (attempt) => Math.min(base * Math.pow(2, attempt - 1), max);
  }

  /**
   * Linear backoff strategy
   *
   * @param increment - Delay increment per attempt in milliseconds (default: 1000ms)
   * @param max - Maximum delay in milliseconds (default: 10000ms)
   * @returns Sleep function with linear backoff
   */
  static linearBackoff(increment = 1000, max = 10000): SleepFunction {
    return (attempt) => Math.min(increment * attempt, max);
  }

  /**
   * Fixed delay strategy
   *
   * @param delay - Fixed delay in milliseconds
   * @returns Sleep function with fixed delay
   */
  static fixedDelay(delay: number): SleepFunction {
    return () => delay;
  }
}
