/**
 * Singleton base class using TypeScript generics.
 * Eliminates boilerplate getInstance() code across 9+ files.
 *
 * Usage:
 * ```typescript
 * class MyService extends Singleton<MyService> {
 *   protected constructor() {
 *     super();
 *     // initialization
 *   }
 *
 *   public doSomething() { ... }
 * }
 *
 * // Access:
 * const service = MyService.getInstance();
 * ```
 */
export abstract class Singleton<T> {
  private static instances = new Map<any, any>();

  /**
   * Protected constructor prevents direct instantiation.
   * Subclasses must define their own protected/private constructor.
   */
  protected constructor() {}

  /**
   * Get singleton instance. Creates one if it doesn't exist.
   * Uses 'this' context to store per-class instances.
   */
  public static getInstance<T>(this: new () => T): T {
    if (!Singleton.instances.has(this)) {
      Singleton.instances.set(this, new this());
    }
    return Singleton.instances.get(this);
  }

  /**
   * Clear the singleton instance (useful for testing).
   * Only clears the instance for the specific class.
   */
  public static clearInstance<T>(this: new () => T): void {
    Singleton.instances.delete(this);
  }

  /**
   * Clear all singleton instances (useful for test cleanup).
   */
  public static clearAllInstances(): void {
    Singleton.instances.clear();
  }
}
