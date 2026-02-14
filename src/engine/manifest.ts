import { XMLParser } from "fast-xml-parser";
import { Logger } from "../core/logger.js";
// import { Parser } from 'm3u8-parser'; // Types issues usually, better to just regex or use a simpler parser for HLS

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
      periodStartNumber = parseInt(periodSegmentList["@_startNumber"] || "0");
      periodTimescale = parseInt(periodSegmentList["@_timescale"] || "1000");

      // Parse SegmentTimeline from Period-level SegmentList
      const timeline = periodSegmentList.SegmentTimeline;
      if (timeline && timeline.S) {
        const sList = Array.isArray(timeline.S) ? timeline.S : [timeline.S];
        for (const s of sList) {
          periodSegments.push({
            t: s["@_t"] ? parseInt(s["@_t"]) : undefined,
            d: parseInt(s["@_d"]),
            r: s["@_r"] ? parseInt(s["@_r"]) : undefined,
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
        const bandwidth = parseInt(rep["@_bandwidth"]);
        const itag = parseInt(rep["@_id"]);

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

        const startNumber = parseInt(segTemplate["@_startNumber"] || "0");
        const timescale = parseInt(segTemplate["@_timescale"] || "1000");
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
              t: s["@_t"] ? parseInt(s["@_t"]) : undefined,
              d: parseInt(s["@_d"]),
              r: s["@_r"] ? parseInt(s["@_r"]) : undefined,
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
    variants?: any[];
    segments?: any[];
    targetDuration?: number;
    mediaSequence?: number;
    endList?: boolean;
    discontinuitySequence?: number;
  } {
    const lines = m3u8Content.split("\n");

    let isMaster = false;
    const variants: any[] = [];
    const segments: any[] = [];
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
            bandwidth: bandwidthMatch ? parseInt(bandwidthMatch[1]) : 0,
            width: resMatch ? parseInt(resMatch[1]) : 0,
            height: resMatch ? parseInt(resMatch[2]) : 0,
            codecs: codecsMatch ? codecsMatch[1] : "",
            url: url,
          });
          i++;
        }
      } else if (line.startsWith("#EXT-X-TARGETDURATION:")) {
        targetDuration = parseFloat(line.split(":")[1]);
      } else if (line.startsWith("#EXT-X-MEDIA-SEQUENCE:")) {
        mediaSequence = parseInt(line.split(":")[1]);
      } else if (line.startsWith("#EXT-X-DISCONTINUITY-SEQUENCE:")) {
        discontinuitySequence = parseInt(line.split(":")[1]);
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
}
