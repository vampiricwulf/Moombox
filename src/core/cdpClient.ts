/**
 * Minimal Chrome DevTools Protocol Client
 *
 * Uses the `ws` package (already a project dependency) to communicate
 * with Chromium-based browsers (Edge, Chrome) via CDP WebSocket.
 */

import ms from "ms";
import WebSocket from "ws";
import { fetchWithTimeout } from "./http.js";
import type { CdpCookie } from "./cookies.js";

interface PendingCommand {
  resolve: (value: any) => void;
  reject: (reason: any) => void;
}

export class CdpClient {
  private ws: WebSocket;
  private nextId = 1;
  private pending = new Map<number, PendingCommand>();
  private eventHandlers = new Map<string, ((params: any) => void)[]>();

  private constructor(ws: WebSocket) {
    this.ws = ws;

    ws.on("message", (data) => {
      try {
        const msg = JSON.parse(data.toString());
        if (msg.id !== undefined) {
          const cmd = this.pending.get(msg.id);
          if (cmd) {
            this.pending.delete(msg.id);
            if (msg.error) {
              cmd.reject(new Error(`CDP error: ${msg.error.message}`));
            } else {
              cmd.resolve(msg.result);
            }
          }
        } else if (msg.method) {
          const handlers = this.eventHandlers.get(msg.method);
          if (handlers) {
            for (const h of handlers) h(msg.params);
          }
        }
      } catch {
        // Ignore parse errors
      }
    });

    ws.on("close", () => {
      for (const cmd of this.pending.values()) {
        cmd.reject(new Error("CDP WebSocket closed"));
      }
      this.pending.clear();
    });
  }

  /**
   * Connect to the browser-level CDP endpoint.
   * Storage.getCookies works on this endpoint.
   */
  static async connect(port: number): Promise<CdpClient> {
    const resp = await fetchWithTimeout(`http://127.0.0.1:${port}/json/version`, undefined, ms("5s"));
    const info = (await resp.json()) as { webSocketDebuggerUrl: string };
    const wsUrl = info.webSocketDebuggerUrl;

    return new Promise<CdpClient>((resolve, reject) => {
      const ws = new WebSocket(wsUrl);
      ws.once("open", () => resolve(new CdpClient(ws)));
      ws.once("error", (err) => reject(err));
    });
  }

  /**
   * Connect to a page-level CDP target.
   * Network.getCookies and Network.getAllCookies work on page targets.
   * Falls back to browser-level if no page targets exist.
   */
  static async connectToPage(port: number): Promise<CdpClient> {
    const resp = await fetchWithTimeout(`http://127.0.0.1:${port}/json`, undefined, ms("5s"));
    const targets = (await resp.json()) as { webSocketDebuggerUrl?: string; type: string }[];
    const page = targets.find((t) => t.type === "page" && t.webSocketDebuggerUrl);

    if (page?.webSocketDebuggerUrl) {
      return new Promise<CdpClient>((resolve, reject) => {
        const ws = new WebSocket(page.webSocketDebuggerUrl!);
        ws.once("open", () => resolve(new CdpClient(ws)));
        ws.once("error", (err) => reject(err));
      });
    }

    // No page target found, fall back to browser-level
    return CdpClient.connect(port);
  }

  /**
   * Send a CDP command and wait for the response
   */
  async send(method: string, params?: Record<string, unknown>): Promise<any> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }), (err) => {
        if (err) {
          this.pending.delete(id);
          reject(err);
        }
      });
    });
  }

  /**
   * Listen for a CDP event
   */
  on(method: string, handler: (params: any) => void): void {
    const handlers = this.eventHandlers.get(method) || [];
    handlers.push(handler);
    this.eventHandlers.set(method, handlers);
  }

  /**
   * Wait for a specific CDP event (one-shot)
   */
  waitForEvent(method: string, timeoutMs: number = ms("30s")): Promise<any> {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        reject(new Error(`Timeout waiting for CDP event: ${method}`));
      }, timeoutMs);

      this.on(method, (params) => {
        clearTimeout(timer);
        resolve(params);
      });
    });
  }

  /**
   * Get all browser cookies (decrypted).
   * Tries multiple CDP methods since availability varies by browser version:
   * 1. Storage.getCookies — modern replacement (browser-level endpoint)
   * 2. Network.getAllCookies — deprecated but still in many versions
   * 3. Network.getCookies with explicit URLs — stable v1.3, always available
   */
  async getAllCookies(): Promise<CdpCookie[]> {
    // Method 1: Storage.getCookies (modern, all domains)
    try {
      const result = await this.send("Storage.getCookies");
      if (result.cookies?.length > 0) return result.cookies as CdpCookie[];
    } catch { /* not supported in this version */ }

    // Method 2: Network.getAllCookies (deprecated but widely available)
    try {
      const result = await this.send("Network.getAllCookies");
      if (result.cookies?.length > 0) return result.cookies as CdpCookie[];
    } catch { /* not supported in this version */ }

    // Method 3: Network.getCookies with explicit URLs (stable, always works)
    try {
      await this.send("Network.enable");
    } catch { /* may already be enabled or not needed */ }
    const result = await this.send("Network.getCookies", {
      urls: [
        "https://www.youtube.com",
        "https://youtube.com",
        "https://accounts.google.com",
        "https://www.google.com",
        "https://google.com",
        "https://www.twitch.tv",
        "https://twitch.tv",
      ],
    });
    return result.cookies as CdpCookie[];
  }

  /**
   * Navigate to a URL and wait for load
   */
  async navigate(url: string): Promise<void> {
    await this.send("Page.enable");
    const loadPromise = this.waitForEvent("Page.loadEventFired", ms("30s"));
    await this.send("Page.navigate", { url });
    await loadPromise;
  }

  /**
   * Close the browser via CDP
   */
  async closeBrowser(): Promise<void> {
    try {
      await this.send("Browser.close");
    } catch {
      // Browser may already be closing
    }
  }

  /**
   * Disconnect the WebSocket
   */
  disconnect(): void {
    this.eventHandlers.clear();
    try {
      this.ws.close();
    } catch {
      // Ignore
    }
  }
}
