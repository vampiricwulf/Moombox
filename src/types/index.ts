/**
 * Centralized type definitions for Moombox
 *
 * This module re-exports all types used throughout the application.
 * Import types from here rather than from individual modules.
 */

export * from "./youtube.js";
export * from "./jobs.js";
export * from "./errors.js";
export * from "./config.js";

/**
 * SEA (Single Executable Application) embedded assets.
 * Populated by the build process when creating Moombox.exe.
 */
declare global {
  // eslint-disable-next-line no-var
  var __MOOMBOX_ASSETS__: Record<string, Buffer | string> | undefined;
}
