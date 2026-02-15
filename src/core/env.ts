import { cleanEnv, str, num, bool } from 'envalid';

/**
 * Validated environment variables with type-safe defaults.
 *
 * This module uses envalid to validate environment variables at startup,
 * preventing runtime errors from invalid values and providing helpful
 * error messages when validation fails.
 */
export const env = cleanEnv(process.env, {
  // Cipher cache sizes (number of entries)
  PREPROCESSED_CACHE_SIZE: num({ default: 150, desc: 'Preprocessed JavaScript cache size' }),
  SOLVER_CACHE_SIZE: num({ default: 50, desc: 'Signature solver cache size' }),
  STS_CACHE_SIZE: num({ default: 150, desc: 'Signature timestamp cache size' }),

  // Feature flags
  IGNORE_SCRIPT_REGION: bool({ default: false, desc: 'Ignore script region in player cache' }),

  // SEA (Single Executable Application) mode
  MOOMBOX_ASSETS_DIR: str({ default: '', desc: 'Path to extracted assets directory (SEA mode)' }),

  // System paths (optional, platform-specific)
  PROGRAMFILES: str({ default: '', desc: 'Windows Program Files directory' }),
  LOCALAPPDATA: str({ default: '', desc: 'Windows Local AppData directory' }),
  APPDATA: str({ default: '', desc: 'Windows/Linux AppData directory' }),
  HOME: str({ default: '', desc: 'User home directory' }),
});
