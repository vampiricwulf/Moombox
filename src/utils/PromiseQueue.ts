/**
 * Promise queue for serializing async operations.
 * Consolidates the queue pattern used in ConfigManager, Database, and Logger.
 *
 * Ensures only one operation runs at a time, preventing race conditions
 * in read-modify-write cycles.
 */
export class PromiseQueue {
  private queue: Promise<void> = Promise.resolve();

  /**
   * Enqueue an async operation to run after all previous operations complete.
   * @param fn - Async function to execute
   * @param onError - Optional error handler (called if fn rejects)
   * @returns Promise that resolves with fn's result
   */
  enqueue<T>(
    fn: () => Promise<T>,
    onError?: (error: unknown) => void,
  ): Promise<T> {
    const result = this.queue.then(fn);

    // Keep the chain going regardless of success/failure
    this.queue = result.then(
      () => {},
      (error) => {
        if (onError) {
          onError(error);
        }
      },
    );

    return result;
  }

  /**
   * Wait for all queued operations to complete.
   * Useful for cleanup/shutdown scenarios.
   */
  async drain(): Promise<void> {
    await this.queue;
  }
}
