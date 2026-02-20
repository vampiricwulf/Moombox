/**
 * Shared async utilities.
 */

/**
 * Simple async delay.
 */
export const sleep = (millis: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, millis));

/**
 * Abort-signal-aware delay. Resolves immediately if the signal is already
 * aborted or when abort fires before the timeout expires.
 */
export function cancellableSleep(millis: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (!signal) { setTimeout(resolve, millis); return; }
    if (signal.aborted) { resolve(); return; }
    const onAbort = () => { clearTimeout(timer); resolve(); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", onAbort); resolve(); }, millis);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}
