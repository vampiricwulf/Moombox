import fs from "fs-extra";
import path from "path";
import { EventEmitter } from "events";
import { ManifestParser } from "./manifest.js";
import { Logger } from "../core/logger.js";
import { DOWNLOAD_TIMEOUT_MS } from "../constants.js";
import { LIMITS } from "../constants/limits.js";

export interface DownloaderOptions {
  baseUrl: string;
  outputFile: string;
  startSeq: number;
  poToken?: string;
  isHls?: boolean;
  maxRetries?: number;
  initUrl?: string;
  resumeFile?: string;
  onCheckStreamStatus?: () => Promise<boolean>; // Returns true if stream has ended
  retryDelayCap?: number; // Max backoff delay in seconds (default: 60)
  liveCheckRetries?: number; // Retries before first live status check (default: 16)
  endSeq?: number; // Stop downloading after this sequence number (inclusive, for timestamp selection)
}

interface ResumeState {
  lastSeq: number;
  bytesWritten: number;
  timestamp: number;
  baseUrl: string;
}

export class SegmentDownloader extends EventEmitter {
  private options: DownloaderOptions;
  private running: boolean = false;
  private currentSeq: number;
  private fileHandle: number = 0;
  private cancelFlag: boolean = false;
  private streamEnded: boolean = false;
  private bytesWritten: number = 0;
  private logger: Logger;

  // Parallel download settings
  private static readonly PARALLEL_DOWNLOADS = LIMITS.MAX_PARALLEL_SEGMENTS;
  private static readonly CATCHUP_THRESHOLD = LIMITS.SEGMENT_BEHIND_THRESHOLD; // Use parallel mode if this many segments behind

  constructor(options: DownloaderOptions) {
    super();
    this.options = options;
    this.currentSeq = options.startSeq;
    this.logger = Logger.getInstance();
  }

  /**
   * Cancellable delay that checks running/cancel/streamEnded flags every 500ms.
   * Returns early if the downloader is stopped or the stream ends.
   */
  private async cancellableDelay(ms: number): Promise<void> {
    const interval = 500;
    let remaining = ms;
    while (remaining > 0 && this.running && !this.cancelFlag && !this.streamEnded) {
      const sleepTime = Math.min(remaining, interval);
      await new Promise((r) => setTimeout(r, sleepTime));
      remaining -= sleepTime;
    }
  }

  private getResumeFilePath(): string {
    return this.options.resumeFile || `${this.options.outputFile}.resume.json`;
  }

  private async loadResumeState(): Promise<ResumeState | null> {
    const resumePath = this.getResumeFilePath();
    try {
      if (await fs.pathExists(resumePath)) {
        const data = await fs.readJson(resumePath);
        // Resume state is keyed by output file path, so if the file exists
        // and has valid data, we can resume (baseUrl changes on restart due
        // to new signatures/tokens, but the stream is the same)
        if (data.lastSeq !== undefined && data.bytesWritten !== undefined) {
          return data as ResumeState;
        }
      }
    } catch {
      // Ignore errors reading resume state
    }
    return null;
  }

  private async saveResumeState(): Promise<void> {
    // Don't save if no segments have been downloaded yet
    if (this.currentSeq <= 0) return;
    const resumePath = this.getResumeFilePath();
    const state: ResumeState = {
      lastSeq: this.currentSeq - 1, // Last *completed* sequence
      bytesWritten: this.bytesWritten,
      timestamp: Date.now(),
      baseUrl: this.options.baseUrl,
    };
    try {
      // Atomic write: tmp file + rename to prevent corruption on crash
      const tmpPath = resumePath + ".tmp";
      await fs.writeJson(tmpPath, state);
      await fs.move(tmpPath, resumePath, { overwrite: true });
    } catch {
      // Ignore errors saving resume state
    }
  }

  private async clearResumeState(): Promise<void> {
    const resumePath = this.getResumeFilePath();
    try {
      await fs.remove(resumePath);
    } catch {
      // Ignore errors
    }
  }

  async start() {
    if (this.running) return;
    this.running = true;
    this.cancelFlag = false;
    this.streamEnded = false;

    // Check for resume state
    const resumeState = await this.loadResumeState();
    let isResuming = false;

    // Open file for appending/writing
    await fs.ensureDir(path.dirname(this.options.outputFile));

    if (resumeState) {
      // Verify the output file exists and has expected size
      try {
        const stats = await fs.stat(this.options.outputFile);
        if (stats.size >= resumeState.bytesWritten) {
          // Truncate file to exact resume point to prevent duplicate data
          if (stats.size > resumeState.bytesWritten) {
            await fs.truncate(this.options.outputFile, resumeState.bytesWritten);
            this.logger.info(
              `[Downloader] Truncated file from ${stats.size} to ${resumeState.bytesWritten} bytes for resume`,
            );
          }
          // Resume from last sequence
          this.currentSeq = resumeState.lastSeq + 1;
          this.bytesWritten = resumeState.bytesWritten;
          isResuming = true;
          this.logger.info(
            `[Downloader] Resuming from sequence ${this.currentSeq}`,
          );
        }
      } catch {
        // File doesn't exist or can't be read, start fresh
      }
    } else {
      // No resume state — truncate any existing file to prevent duplicate data
      // (e.g., if resume state was lost during a crash or manifest refresh)
      try {
        const stats = await fs.stat(this.options.outputFile);
        if (stats.size > 0) {
          await fs.truncate(this.options.outputFile, 0);
          this.logger.info(
            `[Downloader] No resume state — truncated existing file (${stats.size} bytes)`,
          );
        }
      } catch {
        // File doesn't exist yet, nothing to truncate
      }
    }

    this.fileHandle = await fs.open(this.options.outputFile, "a");

    try {
      // Only download init segment if not resuming
      if (!isResuming && this.options.initUrl) {
        try {
          const resp = await fetch(this.options.initUrl, {
            signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
            headers: {
              "User-Agent":
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
            },
          });

          if (resp.status === 200) {
            const buffer = await resp.arrayBuffer();
            await fs.write(this.fileHandle, Buffer.from(buffer));
            this.bytesWritten += buffer.byteLength;
          } else {
            await resp.arrayBuffer(); // Consume body to free socket
            this.logger.warn(
              `[Downloader] Failed to download init segment: ${resp.status}`,
            );
          }
        } catch (e: any) {
          this.logger.warn(
            `[Downloader] Error downloading init segment: ${e.message}`,
          );
        }
      }

      this.emit("start", { seq: this.currentSeq, resuming: isResuming });

      if (this.options.isHls) {
        await this.runHlsLoop();
      } else {
        await this.runDashLoop();
      }
    } finally {
      if (this.fileHandle) {
        await fs.close(this.fileHandle);
        this.fileHandle = 0;
      }
    }

    // Clear resume state only on clean completion (not cancelled or stream-ended,
    // since a stream-ended downloader may be replaced by a refreshed one that
    // needs the resume state to continue from where this one left off)
    if (!this.cancelFlag && !this.streamEnded) {
      await this.clearResumeState();
    }

    this.emit("finish");
  }

  private async runDashLoop() {
    const HEAD_PROBE_INTERVAL_MS = 5000;
    const CATCHUP_PROGRESS_INTERVAL = 10;

    const state = {
      saveCounter: 0,
      progressCounter: 0,
      consecutiveGoneErrors: 0,
      hasStartedDownloading: false,
      headSeq: null as number | null,
      lastHeadProbeTime: 0,
      sameHeadRetryDelay: 0,
      lastConfirmedHead: null as number | null,
      sameSegRetries: 0,
      lastRetrySeq: -1,
    };

    // Initial head probe and catch-up
    state.headSeq = await this.probeHeadSequence();
    state.lastHeadProbeTime = Date.now();
    if (state.headSeq !== null) {
      const segmentsBehind = state.headSeq - this.currentSeq;
      this.logger.debug(
        `[Downloader] Head sequence: ${state.headSeq}, starting from: ${this.currentSeq} (${segmentsBehind} segments to catch up)`,
      );

      if (segmentsBehind >= SegmentDownloader.CATCHUP_THRESHOLD) {
        this.currentSeq = await this.runParallelCatchUp(state.headSeq);
        state.hasStartedDownloading = true;
        state.headSeq = await this.probeHeadSequence();
        state.lastHeadProbeTime = Date.now();
      }
    }

    while (this.running && !this.cancelFlag && !this.streamEnded) {
      // Check if reached end sequence
      if (this.options.endSeq != null && this.currentSeq > this.options.endSeq) {
        this.logger.info(
          `[Downloader] Reached end sequence ${this.options.endSeq}, stopping`,
        );
        break;
      }

      // Check and handle fall-behind
      const shouldContinue = await this.checkAndHandleFallBehind(state);
      if (shouldContinue) continue;

      try {
        const success = await this.downloadSegment(this.currentSeq);
        if (success) {
          await this.handleSuccessfulSegmentDownload(
            state,
            HEAD_PROBE_INTERVAL_MS,
            CATCHUP_PROGRESS_INTERVAL,
          );
        } else {
          const ended = await this.handleFailedSegmentDownload(state);
          if (ended) break;
        }
      } catch (e: any) {
        if (e.message === "CANCELLED") break;
        if (e.message === "GONE") {
          const ended = await this.handleGoneErrors(state);
          if (ended) break;
          if (state.consecutiveGoneErrors <= 20 && !state.hasStartedDownloading) {
            continue; // Skip to next segment
          }
          if (state.consecutiveGoneErrors > 20 && !state.hasStartedDownloading) {
            break; // Failed to find valid starting segment
          }
          continue; // Retry same segment
        }
        this.logger.error(
          `[Downloader] Error downloading seq ${this.currentSeq}: ${e.message}`,
        );
        await this.cancellableDelay(2000);
      }
    }

    await this.saveResumeState();
  }

  /**
   * Check if we've fallen behind and trigger parallel catch-up if needed.
   * Returns true if the caller should continue (skip rest of loop iteration).
   */
  private async checkAndHandleFallBehind(state: {
    headSeq: number | null;
    lastHeadProbeTime: number;
  }): Promise<boolean> {
    if (state.headSeq !== null) {
      const segmentsBehind = state.headSeq - this.currentSeq;
      if (segmentsBehind >= SegmentDownloader.CATCHUP_THRESHOLD) {
        this.currentSeq = await this.runParallelCatchUp(state.headSeq);
        state.headSeq = await this.probeHeadSequence();
        state.lastHeadProbeTime = Date.now();

        // Only re-enter parallel if catch-up fully completed
        const stillFarBehind = state.headSeq !== null
          && (state.headSeq - this.currentSeq >= SegmentDownloader.CATCHUP_THRESHOLD);
        if (!stillFarBehind) return true; // Continue loop
      }
    }
    return false;
  }

  /**
   * Handle successful segment download: update state, probe head, emit progress, save resume state.
   */
  private async handleSuccessfulSegmentDownload(
    state: {
      hasStartedDownloading: boolean;
      consecutiveGoneErrors: number;
      sameHeadRetryDelay: number;
      sameSegRetries: number;
      headSeq: number | null;
      lastHeadProbeTime: number;
      progressCounter: number;
      saveCounter: number;
    },
    HEAD_PROBE_INTERVAL_MS: number,
    CATCHUP_PROGRESS_INTERVAL: number,
  ): Promise<void> {
    state.hasStartedDownloading = true;
    state.consecutiveGoneErrors = 0;
    state.sameHeadRetryDelay = 0;
    state.sameSegRetries = 0;
    this.currentSeq++;

    const isCatchingUp = state.headSeq !== null && this.currentSeq < state.headSeq;

    // Probe head when not catching up
    const now = Date.now();
    if (!isCatchingUp && now - state.lastHeadProbeTime >= HEAD_PROBE_INTERVAL_MS) {
      const newHead = await this.probeHeadSequence();
      state.lastHeadProbeTime = now;
      if (newHead !== null) {
        state.headSeq = newHead;
      }
    }

    const stillCatchingUp = state.headSeq !== null && this.currentSeq < state.headSeq;

    // Emit progress (less frequently when catching up)
    state.progressCounter++;
    if (!stillCatchingUp || state.progressCounter >= CATCHUP_PROGRESS_INTERVAL) {
      const reportedHead = state.headSeq !== null ? Math.max(state.headSeq, this.currentSeq) : null;
      this.emit("progress", {
        seq: this.currentSeq,
        bytes: this.bytesWritten,
        headSeq: reportedHead,
        total: reportedHead,
        catchingUp: stillCatchingUp,
      });
      state.progressCounter = 0;
    }

    // Save resume state (less frequently when catching up)
    state.saveCounter++;
    const saveInterval = stillCatchingUp ? 50 : 10;
    if (state.saveCounter >= saveInterval) {
      await this.saveResumeState();
      state.saveCounter = 0;
    }
  }

  /**
   * Handle failed segment download with backoff and stream status checks.
   * Returns true if stream has ended.
   */
  private async handleFailedSegmentDownload(state: {
    sameSegRetries: number;
    lastRetrySeq: number;
    headSeq: number | null;
    lastHeadProbeTime: number;
    lastConfirmedHead: number | null;
    sameHeadRetryDelay: number;
  }): Promise<boolean> {
    // Track consecutive failures on same segment
    if (this.currentSeq === state.lastRetrySeq) {
      state.sameSegRetries++;
    } else {
      state.sameSegRetries = 1;
      state.lastRetrySeq = this.currentSeq;
    }

    // Re-probe head
    const newHead = await this.probeHeadSequence();
    state.lastHeadProbeTime = Date.now();
    if (newHead !== null) {
      state.headSeq = newHead;
    }

    const behindHead = state.headSeq !== null && this.currentSeq < state.headSeq;
    const stuckOnSegment = state.sameSegRetries >= 10;

    if (behindHead && !stuckOnSegment) {
      // Transient failure - retry with small delay
      await this.cancellableDelay(1000);
      return false;
    }

    // At/past live edge or stuck - backoff with status checks
    if (stuckOnSegment && behindHead) {
      this.logger.info(
        `[Downloader] Segment ${this.currentSeq} failed ${state.sameSegRetries} times while behind head ${state.headSeq} — entering backoff`,
      );
    }

    // Reset backoff if head moved
    if (state.headSeq !== null && state.headSeq !== state.lastConfirmedHead) {
      state.lastConfirmedHead = state.headSeq;
      state.sameHeadRetryDelay = 0;
    }

    const delayCap = this.options.retryDelayCap ?? 60;
    const liveCheckThreshold = this.options.liveCheckRetries ?? 16;
    state.sameHeadRetryDelay = Math.min(state.sameHeadRetryDelay + 1, delayCap);

    // Check stream status at threshold
    if (state.sameHeadRetryDelay === liveCheckThreshold && this.options.onCheckStreamStatus) {
      this.logger.info(
        `[Downloader] Checking stream status at ${state.sameHeadRetryDelay}s backoff...`,
      );
      const ended = await this.options.onCheckStreamStatus();
      if (ended) {
        this.logger.info(`[Downloader] Stream confirmed ended at ${state.sameHeadRetryDelay}s backoff`);
        return true;
      }
    }

    // Check status on every probe at cap
    if (state.sameHeadRetryDelay >= delayCap && this.options.onCheckStreamStatus) {
      this.logger.info(
        `[Downloader] Checking stream status during ${delayCap}s polling...`,
      );
      const ended = await this.options.onCheckStreamStatus();
      if (ended) {
        this.logger.info(`[Downloader] Stream confirmed ended during ${delayCap}s polling`);
        return true;
      }
    }

    await this.cancellableDelay(state.sameHeadRetryDelay * 1000);
    return false;
  }

  /**
   * Handle 403/410 GONE errors. Returns true if stream has ended.
   */
  private async handleGoneErrors(state: {
    consecutiveGoneErrors: number;
    hasStartedDownloading: boolean;
  }): Promise<boolean> {
    state.consecutiveGoneErrors++;

    // Stream ended after consecutive errors
    if (state.hasStartedDownloading && state.consecutiveGoneErrors > 10) {
      this.logger.debug(
        `[Downloader] Stream ended after ${state.consecutiveGoneErrors} consecutive 403/410 errors`,
      );
      await this.saveResumeState();
      this.streamEnded = true;
      return true;
    }

    // Before started - try next segment (expired segments at start)
    if (!state.hasStartedDownloading && state.consecutiveGoneErrors <= 20) {
      this.logger.debug(
        `[Downloader] Segment ${this.currentSeq} unavailable (403/410), trying next...`,
      );
      this.currentSeq++;
      await this.cancellableDelay(100);
      return false;
    }

    // Too many errors without success
    if (!state.hasStartedDownloading && state.consecutiveGoneErrors > 20) {
      this.logger.error(
        `[Downloader] Failed to find valid starting segment after ${state.consecutiveGoneErrors} attempts`,
      );
      this.streamEnded = true;
      return true;
    }

    // Single error while downloading - retry same segment
    await this.cancellableDelay(500);
    return false;
  }

  /**
   * Probe for the head sequence number using X-Head-Seqnum header
   */
  private async probeHeadSequence(): Promise<number | null> {
    try {
      let url = this.options.baseUrl;
      // Use a high sequence number that's likely ahead of the stream
      const probeSeq = 999999999;

      if (url.includes("$Number$")) {
        url = url.replace("$Number$", probeSeq.toString());
      } else {
        if (!url.endsWith("/")) url += "/";
        url += `sq/${probeSeq}`;
      }

      const resp = await fetch(url, {
        method: "GET",
        signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
        headers: {
          "User-Agent":
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
        },
      });

      // Consume response body to free TCP socket
      await resp.arrayBuffer();

      const headSeqStr = resp.headers.get("X-Head-Seqnum");
      if (headSeqStr) {
        const headSeq = parseInt(headSeqStr, 10);
        this.logger.debug(`[Downloader] Probed head sequence: ${headSeq}`);
        return headSeq;
      }
    } catch (e) {
      this.logger.debug(`[Downloader] Failed to probe head sequence: ${e}`);
    }
    return null;
  }

  private async runHlsLoop() {
    let consecutiveErrors = 0;
    let stalePlaylistCount = 0;

    while (this.running && !this.cancelFlag && !this.streamEnded) {
      try {
        // 1. Fetch Playlist
        const resp = await fetch(this.options.baseUrl, {
          signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
          headers: {
            "User-Agent":
              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
          },
        });
        if (!resp.ok) {
          await resp.arrayBuffer(); // Consume body to free TCP socket
          if (resp.status === 404 || resp.status === 410) {
            // Stream likely ended
            this.streamEnded = true;
            break;
          }
          throw new Error(`Playlist fetch failed: ${resp.status}`);
        }

        const text = await resp.text();
        const playlist = ManifestParser.parseHls(text, this.options.baseUrl);

        if (playlist.type !== "media") {
          throw new Error("Expected Media Playlist, got Master");
        }

        const segments = playlist.segments || [];
        const mediaSequence = playlist.mediaSequence || 0;

        // Initialize startSeq if needed
        if (this.currentSeq < 0) {
          this.currentSeq = mediaSequence;
        }

        // Gap Detection — segments expired from CDN, unavoidable
        if (this.currentSeq < mediaSequence) {
          this.logger.warn(
            `[Downloader] HLS Gap: segments ${this.currentSeq}-${mediaSequence - 1} expired from CDN, advancing to ${mediaSequence}`,
          );
          this.currentSeq = mediaSequence;
        }

        // Identify new segments
        const newSegments = segments.filter(
          (_, idx) => mediaSequence + idx >= this.currentSeq,
        );

        for (const seg of newSegments) {
          if (!this.running || this.cancelFlag) break;

          const segUrl = seg.url;
          const success = await this.downloadHlsSegment(segUrl);
          if (success) {
            this.currentSeq++;
            this.emit("progress", {
              seq: this.currentSeq,
              bytes: this.bytesWritten,
            });
          } else {
            // Don't skip — break to re-fetch playlist and retry this segment.
            // If the CDN has purged it, the playlist's mediaSequence will advance
            // and gap detection will handle it on the next iteration.
            this.logger.debug(
              `[Downloader] HLS segment at seq ${this.currentSeq} failed, will retry after playlist refresh`,
            );
            await this.cancellableDelay(2000);
            break;
          }
        }

        // Detect stale playlists (no new segments for multiple refreshes)
        if (newSegments.length === 0) {
          stalePlaylistCount++;
          if (stalePlaylistCount >= 5 && this.options.onCheckStreamStatus) {
            const ended = await this.options.onCheckStreamStatus();
            if (ended) { this.streamEnded = true; break; }
          }
        } else {
          stalePlaylistCount = 0;
        }

        // Save resume state after processing segments
        await this.saveResumeState();

        consecutiveErrors = 0;

        // Check if stream ended (EXT-X-ENDLIST present)
        if (playlist.endList) {
          this.streamEnded = true;
          break;
        }

        // Wait Target Duration before refreshing playlist
        const sleepTime = (playlist.targetDuration || 2) * 1000;
        await this.cancellableDelay(sleepTime);
      } catch (e: any) {
        consecutiveErrors++;
        this.logger.error(`[Downloader] HLS Loop Error: ${e.message}`);
        if (consecutiveErrors > 5) {
          this.cancelFlag = true; // Preserve resume state for retry
          break;
        }
        await this.cancellableDelay(5000);
      }
    }
  }

  private async downloadHlsSegment(url: string): Promise<boolean> {
    try {
      const resp = await fetch(url, {
        signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
        headers: {
          "User-Agent":
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
        },
      });

      if (resp.status === 200) {
        const buffer = await resp.arrayBuffer();
        await fs.write(this.fileHandle, Buffer.from(buffer));
        this.bytesWritten += buffer.byteLength;
        return true;
      }
      await resp.arrayBuffer(); // Consume body to free TCP socket
      return false;
    } catch {
      return false;
    }
  }

  stop() {
    this.running = false;
    this.cancelFlag = true;
  }

  private async downloadSegment(seq: number): Promise<boolean> {
    let url = this.options.baseUrl;

    // DASH Template Logic
    if (url.includes("$Number$")) {
      url = url.replace("$Number$", seq.toString());
    } else {
      // Fallback to "sq/" appending style if not a template
      if (!url.endsWith("/")) url += "/";
      url += `sq/${seq}`;
    }

    // Debug: Log first segment URL
    if (seq === this.options.startSeq) {
      this.logger.debug(
        `[Downloader] First segment URL: ${url.substring(0, 150)}...`,
      );
    }

    const resp = await fetch(url, {
      signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
      },
    });

    if (resp.status === 200) {
      const buffer = await resp.arrayBuffer();
      await fs.write(this.fileHandle, Buffer.from(buffer));
      this.bytesWritten += buffer.byteLength;
      return true;
    } else if (resp.status === 404) {
      // Likely ahead of the stream (segment not generated yet)
      // Wait and retry is handled by main loop returning false
      await resp.arrayBuffer(); // Consume body to free socket
      return false;
    } else if (resp.status === 403 || resp.status === 410) {
      await resp.arrayBuffer(); // Consume body to free socket
      this.logger.debug(`[Downloader] Segment ${seq} returned ${resp.status}`);
      throw new Error("GONE");
    } else {
      // Other error
      await resp.arrayBuffer(); // Consume body to free socket
      this.logger.debug(`[Downloader] Segment ${seq} returned ${resp.status}`);
      return false;
    }
  }

  /**
   * Fetch segment data without writing to file
   * Returns buffer on success, null on 404, throws on error
   */
  private async fetchSegmentData(
    seq: number,
  ): Promise<{ seq: number; data: Buffer } | null> {
    let url = this.options.baseUrl;

    if (url.includes("$Number$")) {
      url = url.replace("$Number$", seq.toString());
    } else {
      if (!url.endsWith("/")) url += "/";
      url += `sq/${seq}`;
    }

    const resp = await fetch(url, {
      signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
      headers: {
        "User-Agent":
          "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
      },
    });

    if (resp.status === 200) {
      const buffer = await resp.arrayBuffer();
      return { seq, data: Buffer.from(buffer) };
    } else {
      await resp.arrayBuffer(); // Consume body to free socket
      if (resp.status === 404) {
        return null;
      } else if (resp.status === 403 || resp.status === 410) {
        throw new Error("GONE");
      } else {
        throw new Error(`HTTP ${resp.status}`);
      }
    }
  }

  /**
   * Parallel catch-up: download multiple segments concurrently and write in order
   * Returns the next sequence number to download after catch-up
   */
  private async runParallelCatchUp(headSeq: number): Promise<number> {
    const segmentBuffer: Map<number, Buffer> = new Map();
    const retryQueue: number[] = [];
    const retryCounts: Map<number, number> = new Map();
    const MAX_SEGMENT_RETRIES = 10;
    const BUFFER_SIZE_CAP = SegmentDownloader.PARALLEL_DOWNLOADS * 3;
    let nextToWrite = this.currentSeq;
    let nextToFetch = this.currentSeq;
    let targetSeq = headSeq - 30; // Stop well before head to hand off to sequential cleanly
    // Respect endSeq for timestamp selection
    if (this.options.endSeq != null && targetSeq > this.options.endSeq + 1) {
      targetSeq = this.options.endSeq + 1;
    }
    let activeDownloads = 0;
    let totalFetched = 0;

    this.logger.info(
      `[Downloader] Starting parallel catch-up: ${this.currentSeq} -> ${targetSeq} (${targetSeq - this.currentSeq} segments, ${SegmentDownloader.PARALLEL_DOWNLOADS} parallel)`,
    );

    const startTime = Date.now();

    // Process downloads and writes
    while (
      this.running &&
      !this.cancelFlag &&
      !this.streamEnded &&
      (nextToWrite < targetSeq || activeDownloads > 0)
    ) {
      // Start new downloads up to parallel limit
      const downloadPromises: Promise<void>[] = [];

      while (
        activeDownloads < SegmentDownloader.PARALLEL_DOWNLOADS &&
        segmentBuffer.size < BUFFER_SIZE_CAP
      ) {
        // Prefer retry queue over new fetches
        let seq: number;
        if (retryQueue.length > 0) {
          seq = retryQueue.shift()!;
        } else if (nextToFetch < targetSeq) {
          seq = nextToFetch++;
        } else {
          break;
        }

        activeDownloads++;

        const promise = this.fetchSegmentData(seq)
          .then((result) => {
            if (result) {
              segmentBuffer.set(result.seq, result.data);
              totalFetched++;
            } else {
              // 404 — segment not available yet, retry like error case
              const retries = (retryCounts.get(seq) || 0) + 1;
              retryCounts.set(seq, retries);
              if (retries < MAX_SEGMENT_RETRIES) {
                retryQueue.push(seq);
              }
            }
          })
          .catch((e) => {
            // Track retries for failed segments
            const retries = (retryCounts.get(seq) || 0) + 1;
            retryCounts.set(seq, retries);
            if (retries < MAX_SEGMENT_RETRIES) {
              retryQueue.push(seq);
              this.logger.debug(
                `[Downloader] Parallel fetch error for seq ${seq} (retry ${retries}/${MAX_SEGMENT_RETRIES}): ${e.message}`,
              );
            } else {
              this.logger.warn(
                `[Downloader] Parallel fetch failed for seq ${seq} after ${MAX_SEGMENT_RETRIES} retries: ${e.message}`,
              );
            }
          })
          .finally(() => {
            activeDownloads--;
          });

        downloadPromises.push(promise);
      }

      // Wait a bit for downloads to complete
      if (downloadPromises.length > 0) {
        await Promise.race([
          Promise.all(downloadPromises),
          new Promise((r) => setTimeout(r, 50)),
        ]);
      }

      // Write available segments in order
      while (segmentBuffer.has(nextToWrite)) {
        const data = segmentBuffer.get(nextToWrite)!;
        await fs.write(this.fileHandle, data);
        this.bytesWritten += data.byteLength;
        segmentBuffer.delete(nextToWrite);
        nextToWrite++;

        // Emit progress periodically
        if (nextToWrite % 20 === 0) {
          const reportedHead = Math.max(headSeq, nextToWrite);
          this.emit("progress", {
            seq: nextToWrite,
            bytes: this.bytesWritten,
            headSeq: reportedHead,
            total: reportedHead,
            catchingUp: true,
          });
        }
      }

      // Check if we're stuck: all downloads done, nothing more to fetch,
      // no retries pending, and the next segment to write is missing (gap from failed fetch)
      if (activeDownloads === 0 && nextToFetch >= targetSeq && retryQueue.length === 0 && !segmentBuffer.has(nextToWrite)) {
        if (nextToWrite < targetSeq) {
          // Find the next available segment in the buffer to skip past the gap
          let gapEnd = nextToWrite + 1;
          while (gapEnd < targetSeq && !segmentBuffer.has(gapEnd)) {
            gapEnd++;
          }
          this.logger.warn(
            `[Downloader] Segment gap detected: seq ${nextToWrite}–${gapEnd - 1} lost after ${MAX_SEGMENT_RETRIES} retries`,
          );
          this.emit("gap", { from: nextToWrite, to: gapEnd - 1 });
          // Skip past the gap and continue writing buffered segments
          nextToWrite = gapEnd;
          // If there are still buffered segments, continue the loop
          if (segmentBuffer.size > 0) {
            continue;
          }
        }
        break;
      }

      // Yield while waiting for in-flight downloads to complete
      if (downloadPromises.length === 0 && !segmentBuffer.has(nextToWrite)) {
        await this.cancellableDelay(100);
      }
    }

    // Write any remaining buffered segments
    while (segmentBuffer.has(nextToWrite)) {
      const data = segmentBuffer.get(nextToWrite)!;
      await fs.write(this.fileHandle, data);
      this.bytesWritten += data.byteLength;
      segmentBuffer.delete(nextToWrite);
      nextToWrite++;
    }

    const elapsed = (Date.now() - startTime) / 1000;
    const segmentsDownloaded = nextToWrite - this.currentSeq;
    const speed = segmentsDownloaded / elapsed;

    this.logger.info(
      `[Downloader] Parallel catch-up complete: ${segmentsDownloaded} segments in ${elapsed.toFixed(1)}s (${speed.toFixed(1)} seg/s)`,
    );

    // Emit final progress
    const finalReportedHead = Math.max(headSeq, nextToWrite);
    this.emit("progress", {
      seq: nextToWrite,
      bytes: this.bytesWritten,
      headSeq: finalReportedHead,
      total: finalReportedHead,
      catchingUp: false,
    });

    return nextToWrite;
  }
}
