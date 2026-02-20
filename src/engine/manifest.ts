import { XMLParser } from "fast-xml-parser";
import { Logger } from "../core/logger.js";

export interface DashSegment {
  t?: number;
  d: number;
  r?: number; // Repeat
}

export interface DashStream {
  itag: number;
  mimeType: string;
  codecs: string;
  width?: number;
  height?: number;
  bandwidth: number;
  baseUrl: string;
  startNumber: number;
  timescale: number;
  segments: DashSegment[];
  initialization?: string;
}

/**
 * Result of timestamp-to-segment mapping
 */
export interface SegmentRange {
  /** First segment index to download (0-based from startNumber) */
  startSegment: number;
  /** Last segment index to download (inclusive) */
  endSegment: number;
  /** Offset within the first segment where the start time falls (seconds) */
  trimStartOffset: number;
  /** Total duration to keep from the trimmed start (seconds), or 0 for no limit */
  trimDuration: number;
}

export interface ParsedHlsVariant {
  bandwidth: number;
  width: number;
  height: number;
  codecs: string;
  url: string;
}

export interface ParsedHlsSegment {
  duration: number;
  url: string;
  discontinuity: number;
}

export class ManifestParser {
  static parseDash(xmlContent: string, manifestUrl?: string): DashStream[] {
    const logger = Logger.getInstance();
    const parser = new XMLParser({
      ignoreAttributes: false,
      attributeNamePrefix: "@_",
    });
    const jsonObj = parser.parse(xmlContent);

    const mpd = jsonObj.MPD;
    if (!mpd) {
      logger.error("[ManifestParser] No MPD element found in manifest");
      return [];
    }

    const period = mpd.Period;
    if (!period) {
      logger.error("[ManifestParser] No Period element found in MPD");
      return [];
    }

    // BaseURL might be at MPD or Period level
    let mpdBaseUrl = "";
    if (mpd.BaseURL) {
      mpdBaseUrl =
        typeof mpd.BaseURL === "string" ? mpd.BaseURL : mpd.BaseURL["#text"];
    } else if (manifestUrl) {
      // Fallback to manifest URL base
      // YouTube manifest.dash URLs are like .../manifest.dash
      // Segments are usually .../sq/1
      // So we can remove "manifest.dash" or just append to the base path.
      // But usually YouTube DASH segments are siblings to manifest.dash in the URL structure logic.
      // Actually, for YouTube, the BaseURL is effectively the same as manifest URL minus the command.
      // Safest bet: If manifestUrl ends with 'manifest.dash', remove it.
      mpdBaseUrl = manifestUrl.replace("manifest.dash", "");
      if (!mpdBaseUrl.endsWith("/")) mpdBaseUrl += "/"; // ensure trailing slash if we are appending relative paths
    }

    // Handle AdaptationSets (Video/Audio)
    if (!period.AdaptationSet) {
      logger.error("[ManifestParser] No AdaptationSet found in Period");
      return [];
    }

    const adaptationSets = Array.isArray(period.AdaptationSet)
      ? period.AdaptationSet
      : [period.AdaptationSet];
    const streams: DashStream[] = [];

    logger.debug(
      `[ManifestParser] Found ${adaptationSets.length} AdaptationSet(s)`,
    );

    // Check for SegmentList at Period level (YouTube live streams)
    const periodSegmentList = period.SegmentList;
    let periodStartNumber = 0;
    let periodTimescale = 1000;
    const periodSegments: DashSegment[] = [];

    if (periodSegmentList) {
      periodStartNumber = parseInt(periodSegmentList["@_startNumber"] || "0", 10);
      periodTimescale = parseInt(periodSegmentList["@_timescale"] || "1000", 10);

      // Parse SegmentTimeline from Period-level SegmentList
      const timeline = periodSegmentList.SegmentTimeline;
      if (timeline && timeline.S) {
        const sList = Array.isArray(timeline.S) ? timeline.S : [timeline.S];
        for (const s of sList) {
          periodSegments.push({
            t: s["@_t"] ? parseInt(s["@_t"], 10) : undefined,
            d: parseInt(s["@_d"], 10),
            r: s["@_r"] ? parseInt(s["@_r"], 10) : undefined,
          });
        }
      }
      logger.debug(
        `[ManifestParser] Found Period-level SegmentList: startNumber=${periodStartNumber}, timescale=${periodTimescale}, segments=${periodSegments.length}`,
      );
    }

    for (const adaptSet of adaptationSets) {
      if (!adaptSet.Representation) {
        logger.debug(
          `[ManifestParser] AdaptationSet has no Representation, skipping`,
        );
        continue;
      }

      const representations = Array.isArray(adaptSet.Representation)
        ? adaptSet.Representation
        : [adaptSet.Representation];

      logger.debug(
        `[ManifestParser] AdaptationSet mimeType=${adaptSet["@_mimeType"]} has ${representations.length} Representation(s)`,
      );

      for (const rep of representations) {
        // Check for BaseURL at AdaptationSet or Representation level
        let repBaseUrl = mpdBaseUrl;
        if (period.BaseURL)
          repBaseUrl =
            typeof period.BaseURL === "string"
              ? period.BaseURL
              : period.BaseURL["#text"];
        if (adaptSet.BaseURL)
          repBaseUrl =
            typeof adaptSet.BaseURL === "string"
              ? adaptSet.BaseURL
              : adaptSet.BaseURL["#text"];
        if (rep.BaseURL)
          repBaseUrl =
            typeof rep.BaseURL === "string"
              ? rep.BaseURL
              : rep.BaseURL["#text"];

        const mimeType = rep["@_mimeType"] || adaptSet["@_mimeType"];
        const codecs = rep["@_codecs"] || adaptSet["@_codecs"];
        const width = rep["@_width"];
        const height = rep["@_height"];
        const bandwidth = parseInt(rep["@_bandwidth"], 10);
        const itag = parseInt(rep["@_id"], 10);

        // SegmentTemplate might be in Representation or AdaptationSet
        const segTemplate = rep.SegmentTemplate || adaptSet.SegmentTemplate;

        // For YouTube live streams: use Period-level SegmentList with Representation BaseURL
        if (!segTemplate && periodSegmentList && repBaseUrl) {
          // YouTube live format: BaseURL contains the full segment base URL
          // Log the full BaseURL to understand the format
          logger.debug(
            `[ManifestParser] Raw BaseURL for itag=${itag}: ${repBaseUrl}`,
          );

          let mediaTemplate = repBaseUrl;

          // Check if BaseURL already contains sq/ pattern anywhere
          if (mediaTemplate.includes("/sq/")) {
            // Replace the segment number with template placeholder
            mediaTemplate = mediaTemplate.replace(/\/sq\/\d+/, "/sq/$Number$");
          } else {
            // BaseURL is the base path, append sq/$Number$
            if (!mediaTemplate.endsWith("/")) {
              mediaTemplate += "/";
            }
            mediaTemplate += "sq/$Number$";
          }

          streams.push({
            itag,
            mimeType,
            codecs,
            width,
            height,
            bandwidth,
            baseUrl: mediaTemplate,
            startNumber: periodStartNumber,
            timescale: periodTimescale,
            segments: periodSegments,
            initialization: undefined, // YouTube live doesn't use separate init segments
          });
          logger.debug(
            `[ManifestParser] Final template for itag=${itag}: ${mediaTemplate}`,
          );
          continue;
        }

        if (!segTemplate) {
          logger.debug(
            `[ManifestParser] Representation itag=${itag} has no SegmentTemplate and no Period SegmentList, skipping`,
          );
          continue;
        }

        const startNumber = parseInt(segTemplate["@_startNumber"] || "0", 10);
        const timescale = parseInt(segTemplate["@_timescale"] || "1000", 10);
        let initialization = segTemplate["@_initialization"];
        let mediaTemplate = segTemplate["@_media"]; // "sq/$Number$" or similar

        // Resolve relative URLs
        if (repBaseUrl) {
          if (mediaTemplate && !mediaTemplate.startsWith("http")) {
            mediaTemplate = repBaseUrl + mediaTemplate;
          }
          if (initialization && !initialization.startsWith("http")) {
            initialization = repBaseUrl + initialization;
          }
        }

        // Parse SegmentTimeline
        const timeline = segTemplate.SegmentTimeline;
        const segments: DashSegment[] = [];
        if (timeline && timeline.S) {
          const sList = Array.isArray(timeline.S) ? timeline.S : [timeline.S];
          for (const s of sList) {
            segments.push({
              t: s["@_t"] ? parseInt(s["@_t"], 10) : undefined,
              d: parseInt(s["@_d"], 10),
              r: s["@_r"] ? parseInt(s["@_r"], 10) : undefined,
            });
          }
        }

        streams.push({
          itag,
          mimeType,
          codecs,
          width,
          height,
          bandwidth,
          baseUrl: mediaTemplate, // Store the template
          startNumber,
          timescale,
          segments,
          initialization,
        });
      }
    }

    return streams;
  }

  static parseHls(
    m3u8Content: string,
    baseUrl: string,
  ): {
    type: "master" | "media";
    variants?: ParsedHlsVariant[];
    segments?: ParsedHlsSegment[];
    targetDuration?: number;
    mediaSequence?: number;
    endList?: boolean;
    discontinuitySequence?: number;
  } {
    const lines = m3u8Content.split("\n");

    let isMaster = false;
    const variants: ParsedHlsVariant[] = [];
    const segments: ParsedHlsSegment[] = [];
    let targetDuration = 0;
    let mediaSequence = 0;
    let endList = false;
    let discontinuitySequence = 0;
    let currentDiscontinuity = 0;

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line) continue;

      if (line.startsWith("#EXT-X-STREAM-INF:")) {
        isMaster = true;
        const bandwidthMatch = line.match(/BANDWIDTH=(\d+)/);
        const resMatch = line.match(/RESOLUTION=(\d+)x(\d+)/);
        const codecsMatch = line.match(/CODECS="([^"]+)"/);

        let url = lines[i + 1]?.trim();
        if (url && !url.startsWith("#")) {
          if (!url.startsWith("http")) {
            url = new URL(url, baseUrl).toString();
          }

          variants.push({
            bandwidth: bandwidthMatch ? parseInt(bandwidthMatch[1], 10) : 0,
            width: resMatch ? parseInt(resMatch[1], 10) : 0,
            height: resMatch ? parseInt(resMatch[2], 10) : 0,
            codecs: codecsMatch ? codecsMatch[1] : "",
            url: url,
          });
          i++;
        }
      } else if (line.startsWith("#EXT-X-TARGETDURATION:")) {
        targetDuration = parseFloat(line.split(":")[1]);
      } else if (line.startsWith("#EXT-X-MEDIA-SEQUENCE:")) {
        mediaSequence = parseInt(line.split(":")[1], 10);
      } else if (line.startsWith("#EXT-X-DISCONTINUITY-SEQUENCE:")) {
        discontinuitySequence = parseInt(line.split(":")[1], 10);
      } else if (line === "#EXT-X-DISCONTINUITY") {
        currentDiscontinuity++;
      } else if (line === "#EXT-X-ENDLIST") {
        endList = true;
      } else if (line.startsWith("#EXTINF:")) {
        const duration = parseFloat(line.split(":")[1].split(",")[0]);
        let url = lines[i + 1]?.trim();
        if (url && !url.startsWith("#")) {
          if (!url.startsWith("http")) {
            url = new URL(url, baseUrl).toString();
          }
          segments.push({
            duration: duration,
            url: url,
            discontinuity: currentDiscontinuity,
          });
          i++;
        }
      }
    }

    if (isMaster) {
      return { type: "master", variants };
    } else {
      return {
        type: "media",
        segments,
        targetDuration,
        mediaSequence,
        endList,
        discontinuitySequence,
      };
    }
  }

  /**
   * Calculate which segments to download for a given time range.
   *
   * For DASH streams with SegmentTimeline, maps start/end timestamps (in seconds)
   * to segment indices. Returns the segment range and trim offsets for the muxer.
   *
   * @param stream - The DASH stream with segment timeline info
   * @param startTimeSec - Start time in seconds (0 = beginning)
   * @param endTimeSec - End time in seconds (undefined = end of stream)
   * @returns Segment range and trim info, or null if timeline is unavailable
   */
  static calculateSegmentRange(
    stream: DashStream,
    startTimeSec?: number,
    endTimeSec?: number,
  ): SegmentRange | null {
    const logger = Logger.getInstance();

    // YouTube live streams typically use ~2s segments without SegmentTimeline
    // For these, we estimate based on a fixed segment duration
    const segments = stream.segments;
    const timescale = stream.timescale || 1000;

    // Calculate segment durations in seconds
    // Expand the segment timeline (handling repeat counts)
    const expandedDurations: number[] = [];
    if (segments.length > 0) {
      for (const seg of segments) {
        const durationSec = seg.d / timescale;
        const repeatCount = (seg.r ?? 0) + 1; // r=0 means 1 occurrence, r=2 means 3
        for (let i = 0; i < repeatCount; i++) {
          expandedDurations.push(durationSec);
        }
      }
    }

    // If no segment timeline is available, estimate with default segment duration
    // YouTube live typically uses ~2 second segments
    const DEFAULT_SEGMENT_DURATION = 2.0;
    const useEstimate = expandedDurations.length === 0;

    if (useEstimate) {
      logger.debug(
        "[ManifestParser] No SegmentTimeline available, using estimated segment duration",
      );
    }

    const getSegmentDuration = (idx: number): number => {
      if (useEstimate) return DEFAULT_SEGMENT_DURATION;
      return idx < expandedDurations.length
        ? expandedDurations[idx]
        : expandedDurations[expandedDurations.length - 1] || DEFAULT_SEGMENT_DURATION;
    };

    // Walk through segments to find start and end indices
    let cumulativeTime = 0;
    let startSegment = stream.startNumber;
    let trimStartOffset = 0;

    if (startTimeSec != null && startTimeSec > 0) {
      let segIdx = 0;
      while (true) {
        const segDuration = getSegmentDuration(segIdx);
        if (cumulativeTime + segDuration > startTimeSec) {
          // Start time falls within this segment
          startSegment = stream.startNumber + segIdx;
          trimStartOffset = startTimeSec - cumulativeTime;
          break;
        }
        cumulativeTime += segDuration;
        segIdx++;

        // Safety: don't scan beyond a reasonable limit
        if (segIdx > 100_000) {
          startSegment = stream.startNumber + segIdx;
          trimStartOffset = 0;
          break;
        }
      }
    }

    let endSegment = -1; // -1 = download to end
    let totalRequestedDuration = 0;

    if (endTimeSec != null && endTimeSec > 0) {
      cumulativeTime = 0;
      let segIdx = 0;
      while (true) {
        const segDuration = getSegmentDuration(segIdx);
        cumulativeTime += segDuration;
        if (cumulativeTime >= endTimeSec) {
          endSegment = stream.startNumber + segIdx;
          break;
        }
        segIdx++;

        // Safety: don't scan beyond a reasonable limit
        if (segIdx > 100_000) {
          endSegment = stream.startNumber + segIdx;
          break;
        }
      }

      totalRequestedDuration = endTimeSec - (startTimeSec || 0);
    }

    logger.debug(
      `[ManifestParser] Segment range: ${startSegment}-${endSegment === -1 ? "end" : endSegment}, ` +
        `trimStart=${trimStartOffset.toFixed(2)}s, duration=${totalRequestedDuration > 0 ? totalRequestedDuration.toFixed(2) + "s" : "full"}`,
    );

    return {
      startSegment,
      endSegment,
      trimStartOffset,
      trimDuration: totalRequestedDuration,
    };
  }
}
