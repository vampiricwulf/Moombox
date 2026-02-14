import type { BotGuardClientOptions, SnapshotArgs, VMFunctions } from '../utils/types.js';
import { BGError, DeferredPromise } from '../utils/index.js';

export default class BotGuardClient {
  public vm: Record<string, any>;
  public program: string;
  public userInteractionElement?: any;
  public syncSnapshotFunction?: (args: any[]) => Promise<string>;
  public deferredVmFunctions = new DeferredPromise<VMFunctions>();
  public defaultTimeout = 3000;

  constructor(options: BotGuardClientOptions) {
    this.userInteractionElement = options.userInteractionElement;
    this.vm = options.globalObj[options.globalName];
    this.program = options.program;
  }

  /**
   * Factory method to create and load a BotGuardClient instance.
   * @param options - Configuration options for the BotGuardClient.
   * @returns A promise that resolves to a loaded BotGuardClient instance.
   */
  public static async create(options: BotGuardClientOptions): Promise<BotGuardClient> {
    return await new BotGuardClient(options).load();
  }

  private async load() {
    if (!this.vm)
      throw new BGError('VM_INIT', 'VM not found');

    if (!this.vm.a)
      throw new BGError('VM_INIT', 'VM init function not found');

    const vmFunctionsCallback = (
      asyncSnapshotFunction: VMFunctions['asyncSnapshotFunction'],
      shutdownFunction: VMFunctions['shutdownFunction'],
      passEventFunction: VMFunctions['passEventFunction'],
      checkCameraFunction: VMFunctions['checkCameraFunction']
    ) => {
      this.deferredVmFunctions.resolve({
        asyncSnapshotFunction,
        shutdownFunction,
        passEventFunction,
        checkCameraFunction
      });
    };

    try {
      this.syncSnapshotFunction = await this.vm.a(this.program, vmFunctionsCallback, true, this.userInteractionElement, () => {/** no-op */ }, [ [], [] ])[0];
    } catch (error) {
      throw new BGError('VM_ERROR', 'Could not load program', { error });
    }

    // Wait for VM callback with a timeout to prevent hanging
    const timeout = new Promise<never>((_, reject) =>
      setTimeout(() => reject(new BGError('TIMEOUT', 'VM load callback timed out')), 10000)
    );
    await Promise.race([this.deferredVmFunctions.promise, timeout]);

    return this;
  }

  /**
   * Takes a snapshot asynchronously.
   * @returns The snapshot result.
   * @example
   * ```ts
   * const result = await botguard.snapshot({
   *   contentBinding: {
   *     c: "a=6&a2=10&b=SZWDwKVIuixOp7Y4euGTgwckbJA&c=1729143849&d=1&t=7200&c1a=1&c6a=1&c6b=1&hh=HrMb5mRWTyxGJphDr0nW2Oxonh0_wl2BDqWuLHyeKLo",
   *     e: "ENGAGEMENT_TYPE_VIDEO_LIKE",
   *     encryptedVideoId: "P-vC09ZJcnM"
   *    }
   * });
   *
   * console.log(result);
   * ```
   */
  public async snapshot(args: SnapshotArgs, timeout = 3000): Promise<string> {
    const vmFunctions = await this.deferredVmFunctions.promise;
    if (!vmFunctions.asyncSnapshotFunction)
      throw new BGError('ASYNC_SNAPSHOT', 'Asynchronous snapshot function not found');

    return new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => reject(new BGError('TIMEOUT', 'VM operation timed out')), timeout);
      vmFunctions.asyncSnapshotFunction!((response: string) => {
        clearTimeout(timer);
        resolve(response);
      }, [
        args.contentBinding,
        args.signedTimestamp,
        args.webPoSignalOutput,
        args.skipPrivacyBuffer
      ]).catch((err: unknown) => {
        clearTimeout(timer);
        reject(err);
      });
    });
  }

  /**
   * Passes an event to the VM.
   * @throws Error Throws an error if the pass event function is not found.
   */
  public async passEvent(args: unknown, timeout = this.defaultTimeout): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new BGError('TIMEOUT', 'VM operation timed out')), timeout);
      this.deferredVmFunctions.promise.then((vmFunctions) => {
        clearTimeout(timer);
        if (!vmFunctions.passEventFunction)
          return reject(new BGError('PASS_EVENT', 'Pass event function not found'));
        vmFunctions.passEventFunction(args);
        resolve();
      }).catch((err) => { clearTimeout(timer); reject(err); });
    });
  }

  /**
   * Checks the "camera".
   * @throws Error Throws an error if the check camera function is not found.
   */
  public async checkCamera(args: unknown, timeout = this.defaultTimeout): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new BGError('TIMEOUT', 'VM operation timed out')), timeout);
      this.deferredVmFunctions.promise.then((vmFunctions) => {
        clearTimeout(timer);
        if (!vmFunctions.checkCameraFunction)
          return reject(new BGError('CHECK_CAMERA', 'Check camera function not found'));
        vmFunctions.checkCameraFunction(args);
        resolve();
      }).catch((err) => { clearTimeout(timer); reject(err); });
    });
  }

  /**
   * Shuts down the VM. Taking a snapshot after this will throw an error.
   * @throws Error Throws an error if the shutdown function is not found.
   */
  public async shutdown(timeout = this.defaultTimeout): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new BGError('TIMEOUT', 'VM operation timed out')), timeout);
      this.deferredVmFunctions.promise.then((vmFunctions) => {
        clearTimeout(timer);
        if (!vmFunctions.shutdownFunction)
          return reject(new BGError('SHUTDOWN', 'Shutdown function not found'));
        vmFunctions.shutdownFunction();
        resolve();
      }).catch((err) => { clearTimeout(timer); reject(err); });
    });
  }

  /**
   * Takes a snapshot synchronously.
   * @returns The snapshot result.
   * @throws Error Throws an error if the synchronous snapshot function is not found.
   */
  public async snapshotSynchronous(args: SnapshotArgs): Promise<string> {
    if (!this.syncSnapshotFunction)
      throw new BGError('SYNC_SNAPSHOT', 'Synchronous snapshot function not found');

    return this.syncSnapshotFunction([
      args.contentBinding,
      args.signedTimestamp,
      args.webPoSignalOutput,
      args.skipPrivacyBuffer
    ]);
  }
}