/**
 * Smooth Value
 *
 * Provides exponential moving average for smoothing jittery values.
 * Based on yt-dlp's progress smoothing pattern.
 */

/**
 * Smooth Value - smooths fluctuating values using exponential moving average
 */
export class SmoothValue {
  private smoothedValue: number | null = null;

  /**
   * @param alpha - Smoothing factor (0-1). Higher = more responsive to changes
   */
  constructor(private alpha: number = 0.7) {
    if (alpha < 0 || alpha > 1) {
      throw new Error("Alpha must be between 0 and 1");
    }
  }

  /**
   * Update with a new value and return the smoothed result
   *
   * @param newValue - New value to incorporate
   * @returns Smoothed value
   */
  update(newValue: number): number {
    if (this.smoothedValue === null) {
      this.smoothedValue = newValue;
    } else {
      // Exponential moving average: smoothed = α * new + (1-α) * smoothed
      this.smoothedValue = this.alpha * newValue + (1 - this.alpha) * this.smoothedValue;
    }
    return this.smoothedValue;
  }

  /**
   * Get current smoothed value (null if no values added yet)
   */
  get value(): number | null {
    return this.smoothedValue;
  }

  /**
   * Reset the smoother
   */
  reset(): void {
    this.smoothedValue = null;
  }
}
