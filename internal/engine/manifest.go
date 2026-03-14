package engine

import (
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const defaultSegmentDuration = 2.0

// DashSegment represents a segment in a DASH timeline.
type DashSegment struct {
	T int64 // Timestamp (optional, 0 if unset)
	D int64 // Duration in timescale units
	R int   // Repeat count (0 means 1 occurrence, 1 means 2, etc.)
}

// DashStream represents a parsed DASH adaptation/representation.
type DashStream struct {
	Itag           int
	MimeType       string
	Codecs         string
	Width          int
	Height         int
	FPS            int           // From frameRate attribute (0 if not present)
	Bandwidth      int
	BaseURL        string        // Template URL with $Number$ placeholder
	StartNumber    int           // First segment sequence number
	Timescale      int           // Time unit divisor
	Segments       []DashSegment // Segment timeline
	Initialization string        // Init segment URL
}

// SegmentRange maps a time range to segment indices.
type SegmentRange struct {
	StartSegment    int     // Absolute segment index
	EndSegment      int     // Inclusive, -1 means to end
	TrimStartOffset float64 // Seconds into first segment
	TrimDuration    float64 // Total duration
}

// HlsVariant represents a variant in an HLS master playlist.
type HlsVariant struct {
	Bandwidth int
	Width     int
	Height    int
	FPS       int // From FRAME-RATE attribute (0 if not present)
	Codecs    string
	URL       string
	Name      string
	IsSource  bool
}

// HlsSegment represents a segment in an HLS media playlist.
type HlsSegment struct {
	Duration      float64
	URL           string
	Discontinuity int
}

// HlsPlaylist represents a parsed HLS media playlist.
type HlsPlaylist struct {
	Segments              []HlsSegment
	TargetDuration        float64
	MediaSequence         int
	DiscontinuitySequence int
	EndList               bool
}

// HlsParseResult contains either master variants or media segments.
type HlsParseResult struct {
	IsMaster bool
	Variants []HlsVariant
	Playlist *HlsPlaylist
}

// --- DASH Parsing ---

// XML structures for DASH MPD parsing.
type mpdXML struct {
	XMLName xml.Name    `xml:"MPD"`
	BaseURL string      `xml:"BaseURL"`
	Periods []periodXML `xml:"Period"`
}

type periodXML struct {
	BaseURL        string             `xml:"BaseURL"`
	AdaptationSets []adaptationSetXML `xml:"AdaptationSet"`
	SegmentList    *segmentListXML    `xml:"SegmentList"`
}

type adaptationSetXML struct {
	MimeType        string              `xml:"mimeType,attr"`
	Codecs          string              `xml:"codecs,attr"`
	BaseURL         string              `xml:"BaseURL"`
	Representations []representationXML `xml:"Representation"`
	SegmentTemplate *segmentTemplateXML `xml:"SegmentTemplate"`
	SegmentList     *segmentListXML     `xml:"SegmentList"`
}

type representationXML struct {
	ID              string              `xml:"id,attr"`
	Bandwidth       int                 `xml:"bandwidth,attr"`
	Width           int                 `xml:"width,attr"`
	Height          int                 `xml:"height,attr"`
	FrameRate       string              `xml:"frameRate,attr"` // e.g. "30", "60", "30/1"
	Codecs          string              `xml:"codecs,attr"`
	MimeType        string              `xml:"mimeType,attr"`
	BaseURL         string              `xml:"BaseURL"`
	SegmentTemplate *segmentTemplateXML `xml:"SegmentTemplate"`
	SegmentList     *segmentListXML     `xml:"SegmentList"`
}

type segmentTemplateXML struct {
	Media          string           `xml:"media,attr"`
	Initialization string           `xml:"initialization,attr"`
	StartNumber    int              `xml:"startNumber,attr"`
	Timescale      int              `xml:"timescale,attr"`
	Timeline       *segmentTimeline `xml:"SegmentTimeline"`
}

type segmentListXML struct {
	StartNumber    int              `xml:"startNumber,attr"`
	Timescale      int              `xml:"timescale,attr"`
	Initialization *initXML         `xml:"Initialization"`
	Timeline       *segmentTimeline `xml:"SegmentTimeline"`
}

type segmentTimeline struct {
	Segments []segmentTimelineS `xml:"S"`
}

type segmentTimelineS struct {
	T int64 `xml:"t,attr"`
	D int64 `xml:"d,attr"`
	R int   `xml:"r,attr"`
}

type initXML struct {
	SourceURL string `xml:"sourceURL,attr"`
}

var sqPattern = regexp.MustCompile(`/sq/\d+`)

// ParseDash parses a DASH MPD manifest and returns available streams.
func ParseDash(xmlContent string, manifestURL string) ([]DashStream, error) {
	var mpd mpdXML
	if err := xml.Unmarshal([]byte(xmlContent), &mpd); err != nil {
		return nil, fmt.Errorf("parse MPD XML: %w", err)
	}

	if len(mpd.Periods) == 0 {
		return nil, nil
	}

	mpdBaseURL := mpd.BaseURL
	if mpdBaseURL == "" && manifestURL != "" {
		if u, err := url.Parse(manifestURL); err == nil {
			if idx := strings.LastIndex(u.Path, "/"); idx >= 0 {
				u.Path = u.Path[:idx+1]
			}
			// Preserve query string — YouTube uses it for token auth
			mpdBaseURL = u.String()
		}
	}

	var streams []DashStream

	for _, period := range mpd.Periods {
		periodBase := mpdBaseURL
		if period.BaseURL != "" {
			if strings.HasPrefix(period.BaseURL, "http") {
				periodBase = period.BaseURL
			} else if mpdBaseURL != "" {
				periodBase = resolveURL(mpdBaseURL, period.BaseURL)
			}
		}

		for _, as := range period.AdaptationSets {
			for _, rep := range as.Representations {
				stream := parseDashRepresentation(rep, as, period, periodBase)
				if stream != nil {
					streams = append(streams, *stream)
				}
			}
		}
	}

	return streams, nil
}

func parseDashRepresentation(rep representationXML, as adaptationSetXML, period periodXML, baseURL string) *DashStream {
	stream := &DashStream{
		Bandwidth: rep.Bandwidth,
		Width:     rep.Width,
		Height:    rep.Height,
		FPS:       parseFrameRate(rep.FrameRate),
	}

	// Parse itag from ID
	if rep.ID != "" {
		var err error
		stream.Itag, err = strconv.Atoi(rep.ID)
		if err != nil {
			// Non-numeric representation ID — itag stays 0
			stream.Itag = 0
		}
	}

	// MimeType: representation > adaptation set
	stream.MimeType = rep.MimeType
	if stream.MimeType == "" {
		stream.MimeType = as.MimeType
	}

	// Codecs: representation > adaptation set
	stream.Codecs = rep.Codecs
	if stream.Codecs == "" {
		stream.Codecs = as.Codecs
	}

	// BaseURL resolution chain: Representation > AdaptationSet > Period > MPD
	repBase := baseURL
	if as.BaseURL != "" {
		if strings.HasPrefix(as.BaseURL, "http") {
			repBase = as.BaseURL
		} else if baseURL != "" {
			repBase = resolveURL(baseURL, as.BaseURL)
		}
	}
	if rep.BaseURL != "" {
		if strings.HasPrefix(rep.BaseURL, "http") {
			repBase = rep.BaseURL
		} else if repBase != "" {
			repBase = resolveURL(repBase, rep.BaseURL)
		} else {
			repBase = rep.BaseURL
		}
	}

	// YouTube live: convert /sq/N to /sq/$Number$
	if sqPattern.MatchString(repBase) {
		repBase = sqPattern.ReplaceAllString(repBase, "/sq/$Number$")
	}
	stream.BaseURL = repBase

	// Segment template: representation > adaptation set > period
	tmpl := rep.SegmentTemplate
	if tmpl == nil {
		tmpl = as.SegmentTemplate
	}

	segList := rep.SegmentList
	if segList == nil {
		segList = as.SegmentList
	}
	if segList == nil {
		segList = period.SegmentList
	}

	if tmpl != nil {
		stream.StartNumber = tmpl.StartNumber
		stream.Timescale = tmpl.Timescale
		if stream.Timescale == 0 {
			stream.Timescale = 1000
		}
		if tmpl.Media != "" {
			stream.BaseURL = resolveURL(repBase, tmpl.Media)
		}
		if tmpl.Initialization != "" {
			stream.Initialization = resolveURL(repBase, tmpl.Initialization)
		}
		if tmpl.Timeline != nil {
			stream.Segments = parseTimeline(tmpl.Timeline)
		}
	} else if segList != nil {
		stream.StartNumber = segList.StartNumber
		stream.Timescale = segList.Timescale
		if stream.Timescale == 0 {
			stream.Timescale = 1000
		}
		if segList.Initialization != nil && segList.Initialization.SourceURL != "" {
			stream.Initialization = resolveURL(repBase, segList.Initialization.SourceURL)
		}
		if segList.Timeline != nil {
			stream.Segments = parseTimeline(segList.Timeline)
		}
	}

	return stream
}

func parseTimeline(tl *segmentTimeline) []DashSegment {
	segments := make([]DashSegment, 0, len(tl.Segments))
	for _, s := range tl.Segments {
		segments = append(segments, DashSegment(s))
	}
	return segments
}

// SegmentURL builds the URL for a specific segment number from a template.
func SegmentURL(template string, seqNum int) string {
	return strings.ReplaceAll(template, "$Number$", strconv.Itoa(seqNum))
}

// CalculateSegmentRange maps a time range to segment indices in a DASH stream.
func CalculateSegmentRange(stream *DashStream, startTimeSec, endTimeSec float64) *SegmentRange {
	timescale := float64(stream.Timescale)
	if timescale == 0 {
		timescale = 1000 // Default fallback (matches TS)
	}

	// Expand segments with repeat counts
	type expandedSeg struct {
		duration float64 // seconds
	}
	var expanded []expandedSeg

	if len(stream.Segments) > 0 {
		for _, seg := range stream.Segments {
			dur := float64(seg.D) / timescale
			count := seg.R + 1 // R=0 means 1 occurrence
			for range count {
				expanded = append(expanded, expandedSeg{duration: dur})
				if len(expanded) > 100000 {
					return nil // Safety limit
				}
			}
		}
	}

	// If no segment timeline, use estimated 2s segment duration (YouTube live typical)
	const defaultSegDuration = 2.0
	useEstimate := len(expanded) == 0

	getSegDuration := func(idx int) float64 {
		if useEstimate {
			return defaultSegDuration
		}
		if idx < len(expanded) {
			return expanded[idx].duration
		}
		if len(expanded) > 0 {
			return expanded[len(expanded)-1].duration
		}
		return defaultSegDuration
	}

	result := &SegmentRange{
		StartSegment: stream.StartNumber,
		EndSegment:   -1,
	}

	// A8: Use stream.StartNumber to convert array index to actual segment number
	baseNum := stream.StartNumber

	// Walk through segments to find start index
	if startTimeSec > 0 {
		accumSec := 0.0
		segIdx := 0
		for {
			segDur := getSegDuration(segIdx)
			if accumSec+segDur > startTimeSec {
				result.StartSegment = baseNum + segIdx
				result.TrimStartOffset = startTimeSec - accumSec
				break
			}
			accumSec += segDur
			segIdx++
			if segIdx > 100000 {
				result.StartSegment = baseNum + segIdx
				result.TrimStartOffset = 0
				break
			}
		}
	}

	// Walk through segments to find end index
	if endTimeSec > 0 {
		accumSec := 0.0
		segIdx := 0
		for {
			segDur := getSegDuration(segIdx)
			segEnd := accumSec + segDur
			if segEnd >= endTimeSec {
				result.EndSegment = baseNum + segIdx
				result.TrimDuration = endTimeSec - startTimeSec
				break
			}
			accumSec = segEnd
			segIdx++
			if segIdx > 100000 {
				break
			}
		}
	}

	return result
}

// --- HLS Parsing ---

// ParseHls parses an HLS M3U8 playlist (master or media).
func ParseHls(m3u8Content string, baseURL string) *HlsParseResult {
	lines := strings.Split(strings.TrimSpace(m3u8Content), "\n")
	if len(lines) == 0 {
		return nil
	}

	// Check if master or media
	isMaster := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF") {
			isMaster = true
			break
		}
	}

	if isMaster {
		return parseMasterPlaylist(lines, baseURL)
	}
	return parseMediaPlaylist(lines, baseURL)
}

func parseMasterPlaylist(lines []string, baseURL string) *HlsParseResult {
	result := &HlsParseResult{IsMaster: true}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}

		attrs := line[len("#EXT-X-STREAM-INF:"):]
		variant := HlsVariant{}

		// Parse attributes
		for _, attr := range parseAttributes(attrs) {
			switch attr.key {
			case "BANDWIDTH":
				variant.Bandwidth, _ = strconv.Atoi(attr.value)
			case "RESOLUTION":
				if w, h, ok := strings.Cut(attr.value, "x"); ok {
					variant.Width, _ = strconv.Atoi(w)
					variant.Height, _ = strconv.Atoi(h)
				}
			case "FRAME-RATE":
				if v, err := strconv.ParseFloat(attr.value, 64); err == nil {
					variant.FPS = int(math.Round(v))
				}
			case "CODECS":
				variant.Codecs = strings.Trim(attr.value, "\"")
			case "VIDEO":
				name := strings.Trim(attr.value, "\"")
				variant.Name = name
				variant.IsSource = name == "chunked" || strings.Contains(strings.ToLower(name), "source")
			}
		}

		// Next non-comment line is the URL
		for i++; i < len(lines); i++ {
			nextLine := strings.TrimSpace(lines[i])
			if nextLine != "" && !strings.HasPrefix(nextLine, "#") {
				variant.URL = resolveURL(baseURL, nextLine)
				break
			}
		}

		result.Variants = append(result.Variants, variant)
	}

	return result
}

func parseMediaPlaylist(lines []string, baseURL string) *HlsParseResult {
	playlist := &HlsPlaylist{}
	discontinuity := 0
	var currentDuration float64

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			playlist.TargetDuration, _ = strconv.ParseFloat(line[len("#EXT-X-TARGETDURATION:"):], 64)

		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			playlist.MediaSequence, _ = strconv.Atoi(line[len("#EXT-X-MEDIA-SEQUENCE:"):])

		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			playlist.DiscontinuitySequence, _ = strconv.Atoi(line[len("#EXT-X-DISCONTINUITY-SEQUENCE:"):])

		case strings.HasPrefix(line, "#EXTINF:"):
			durationStr := line[len("#EXTINF:"):]
			durationStr, _, _ = strings.Cut(durationStr, ",")
			currentDuration, _ = strconv.ParseFloat(durationStr, 64)

		case line == "#EXT-X-DISCONTINUITY":
			discontinuity++

		case line == "#EXT-X-ENDLIST":
			playlist.EndList = true

		case line != "" && !strings.HasPrefix(line, "#"):
			// Segment URL
			segment := HlsSegment{
				Duration:      currentDuration,
				URL:           resolveURL(baseURL, line),
				Discontinuity: discontinuity,
			}
			playlist.Segments = append(playlist.Segments, segment)
			currentDuration = 0
		}
	}

	return &HlsParseResult{Playlist: playlist}
}

// --- Utility ---

type attribute struct {
	key   string
	value string
}

func parseAttributes(s string) []attribute {
	var attrs []attribute
	for len(s) > 0 {
		s = strings.TrimSpace(s)
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		s = rest

		var value string
		if len(s) > 0 && s[0] == '"' {
			// Quoted value
			if inner, after, ok := strings.Cut(s[1:], "\""); ok {
				value = inner
				s = after
			} else {
				value = s[1:]
				s = ""
			}
		} else {
			value, s, _ = strings.Cut(s, ",")
		}

		if len(s) > 0 && s[0] == ',' {
			s = s[1:]
		}
		attrs = append(attrs, attribute{key: key, value: value})
	}
	return attrs
}

func resolveURL(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	if base == "" {
		return ref
	}
	baseU, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refU, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseU.ResolveReference(refU).String()
}

// TotalDurationSec returns the total duration of a DASH stream in seconds.
func TotalDurationSec(stream *DashStream) float64 {
	if stream.Timescale == 0 || len(stream.Segments) == 0 {
		return 0
	}
	ts := float64(stream.Timescale)
	total := 0.0
	for _, seg := range stream.Segments {
		count := seg.R + 1
		total += float64(seg.D) / ts * float64(count)
	}
	return total
}

// TotalSegmentCount returns the total number of segments, expanding repeats.
func TotalSegmentCount(stream *DashStream) int {
	total := 0
	for _, seg := range stream.Segments {
		total += seg.R + 1
	}
	return total
}

// SegmentDurationSec returns the average segment duration in seconds.
func SegmentDurationSec(stream *DashStream) float64 {
	count := TotalSegmentCount(stream)
	if count == 0 {
		return defaultSegmentDuration
	}
	return TotalDurationSec(stream) / float64(count)
}

// EstimateSegmentCount estimates the number of segments for a given duration.
func EstimateSegmentCount(durationSec float64) int {
	return int(math.Ceil(durationSec / defaultSegmentDuration))
}

// parseFrameRate parses a DASH frameRate attribute value.
// YouTube uses plain integers ("30", "60") but the spec also allows
// fractional notation ("30000/1001"). Returns 0 if empty or unparseable.
func parseFrameRate(s string) int {
	if s == "" {
		return 0
	}
	// Try fractional "num/den"
	if idx := strings.Index(s, "/"); idx > 0 {
		num, err1 := strconv.Atoi(s[:idx])
		den, err2 := strconv.Atoi(s[idx+1:])
		if err1 == nil && err2 == nil && den > 0 {
			return int(math.Round(float64(num) / float64(den)))
		}
	}
	// Plain integer
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	// Float string (rare)
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return int(math.Round(v))
	}
	return 0
}
