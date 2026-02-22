/**
 * Authentication Service
 *
 * Password hashing (scrypt), session management, cookie parsing.
 * Only required when network_access === "external" and password_hash is set.
 */

import crypto from "crypto";
import { ConfigManager } from "./config.js";

const SCRYPT_KEYLEN = 64;
const SCRYPT_OPTIONS: crypto.ScryptOptions = { N: 16384, r: 8, p: 1 };
const SESSION_TTL_MS = 24 * 60 * 60_000; // 24 hours
const SALT_BYTES = 16;
const TOKEN_BYTES = 32;

/** Check if a value is already an scrypt hash (scrypt:<salt>:<hash>) */
export function isScryptHash(value: string): boolean {
  const parts = value.split(":");
  return parts.length === 3 && parts[0] === "scrypt";
}

/** Hash a plaintext password using scrypt. Returns "scrypt:<salt_hex>:<hash_hex>" */
export function hashPassword(plain: string): string {
  const salt = crypto.randomBytes(SALT_BYTES);
  const hash = crypto.scryptSync(plain, salt, SCRYPT_KEYLEN, SCRYPT_OPTIONS);
  return `scrypt:${salt.toString("hex")}:${hash.toString("hex")}`;
}

interface Session {
  createdAt: number;
}

export class AuthService {
  private static instance: AuthService;
  private sessions = new Map<string, Session>();
  private cleanupTimer: NodeJS.Timeout | null = null;

  private constructor() {
    // Periodically evict expired sessions (every hour) so stale tokens
    // don't accumulate when validateSession() is never called for them.
    this.cleanupTimer = setInterval(() => this.evictExpired(), 60 * 60_000);
    this.cleanupTimer.unref(); // Don't prevent process exit
  }

  /** Remove all expired sessions. */
  private evictExpired(): void {
    const now = Date.now();
    for (const [token, session] of this.sessions) {
      if (now - session.createdAt > SESSION_TTL_MS) {
        this.sessions.delete(token);
      }
    }
  }

  static getInstance(): AuthService {
    if (!AuthService.instance) {
      AuthService.instance = new AuthService();
    }
    return AuthService.instance;
  }

  /**
   * Hash a plaintext password using scrypt.
   * Returns "scrypt:<salt_hex>:<hash_hex>"
   */
  hashPassword(plain: string): string {
    return hashPassword(plain);
  }

  /**
   * Verify a plaintext password against a stored hash.
   * Uses timing-safe comparison to prevent timing attacks.
   */
  verifyPassword(plain: string, stored: string): boolean {
    const parts = stored.split(":");
    if (parts.length !== 3 || parts[0] !== "scrypt") return false;

    const salt = Buffer.from(parts[1], "hex");
    const expectedHash = Buffer.from(parts[2], "hex");

    if (salt.length !== SALT_BYTES || expectedHash.length !== SCRYPT_KEYLEN) {
      return false;
    }

    const derivedHash = crypto.scryptSync(plain, salt, SCRYPT_KEYLEN, SCRYPT_OPTIONS);
    return crypto.timingSafeEqual(derivedHash, expectedHash);
  }

  /**
   * Create a new session token and store it.
   */
  createSession(): string {
    const token = crypto.randomBytes(TOKEN_BYTES).toString("hex");
    this.sessions.set(token, { createdAt: Date.now() });
    return token;
  }

  /**
   * Validate a session token. Returns false if missing or expired.
   */
  validateSession(token: string): boolean {
    const session = this.sessions.get(token);
    if (!session) return false;

    if (Date.now() - session.createdAt > SESSION_TTL_MS) {
      this.sessions.delete(token);
      return false;
    }

    return true;
  }

  /**
   * Invalidate a single session by token.
   */
  invalidateSession(token: string): void {
    this.sessions.delete(token);
  }

  /**
   * Invalidate all sessions (called on password change/removal).
   */
  invalidateAllSessions(): void {
    this.sessions.clear();
  }

  /**
   * Invalidate all sessions except the given token.
   */
  invalidateOtherSessions(keepToken: string): void {
    for (const key of this.sessions.keys()) {
      if (key !== keepToken) {
        this.sessions.delete(key);
      }
    }
  }

  /**
   * Check whether authentication is required for external connections.
   * True when network_access === "external" AND password_hash is set.
   */
  isAuthRequired(): boolean {
    try {
      const config = ConfigManager.getInstance().get();
      return config.network_access === "external" && !!config.password_hash;
    } catch {
      return false;
    }
  }

  /**
   * Parse cookies from a Cookie header string.
   * Simple implementation — no dependency needed.
   */
  parseCookies(header: string): Record<string, string> {
    const cookies: Record<string, string> = {};
    if (!header) return cookies;

    for (const pair of header.split(";")) {
      const idx = pair.indexOf("=");
      if (idx === -1) continue;
      const key = pair.substring(0, idx).trim();
      const value = pair.substring(idx + 1).trim();
      if (key) cookies[key] = value;
    }

    return cookies;
  }
}
