/**
 * Shared async utilities.
 */

/**
 * Simple async delay.
 * For abort-aware delays, use cancellableDelay() in the respective module.
 */
export const sleep = (millis: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, millis));
