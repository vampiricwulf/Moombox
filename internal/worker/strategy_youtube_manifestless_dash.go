package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// HasManifestlessDashFormats reports whether videoInfo.Formats[] contains the
// split video + audio adaptive entries that make a manifest-free DASH
// (&sq=N-segmented) download viable. The signal is: at least one audio-only
// entry AND at least one video-only entry, both with non-empty URLs AND no
// contentLength. HLS variants are muxed (carry both video and audio in the same
// itag) so they don't pass; only true DASH-style adaptive splits do.
//
// The contentLength exclusion is critical. A format carrying a contentLength is
// a COMPLETE FILE (a regular VOD / premiere adaptive format) served whole over
// its URL — it is downloaded directly via byte-range, NEVER segment-addressed
// with &sq=N. Only open-ended live/OTF formats (no contentLength) are the
// manifest-free DASH segment case. This mirrors yt-dlp, which byte-range-chunks
// a contentLength-bearing format and only gives OTF/live formats &sq fragments.
// Feeding a whole-file URL to the &sq loop is catastrophic: YouTube ignores &sq
// and returns the ENTIRE file for every sequence number, so the segment loop
// re-downloads the whole file forever — no past-end 403 ever arrives to stop it
// (a premiere classified StreamPostLive filled the disk with hundreds of GB
// this way before this guard).
//
// Used by the orchestrator strategy switch to detect the experiment case where
// YouTube withholds dashManifestUrl but still ships adaptiveFormats[] in the
// watch-page player response (yt-dlp issue #15274).
func HasManifestlessDashFormats(formats []youtube.Format) bool {
	hasAudio, hasVideo := false, false
	for i := range formats {
		f := &formats[i]
		if f.URL == "" || f.ContentLength != "" {
			continue
		}
		if f.IsAudio() {
			hasAudio = true
		} else if f.Width != nil && *f.Width > 0 {
			hasVideo = true
		}
		if hasAudio && hasVideo {
			return true
		}
	}
	return false
}

// formatToDashStreamInfo converts a youtube.Format into the DashStreamInfo
// shape that SelectBestDashStream expects. Mirrors the conversion done in
// strategy_youtube_dash.go:160-172 for parsed-manifest streams; sharing a
// single helper would couple two unrelated types so we keep it inline.
func formatToDashStreamInfo(f *youtube.Format) DashStreamInfo {
	info := DashStreamInfo{
		Itag:      f.Itag,
		MimeType:  f.MimeType,
		Bandwidth: f.Bitrate,
		BaseURL:   f.URL,
	}
	if f.Width != nil {
		info.Width = *f.Width
	}
	if f.Height != nil {
		info.Height = *f.Height
	}
	if f.Fps != nil {
		info.FPS = *f.Fps
	}
	return info
}

// partitionManifestlessFormats splits adaptive formats into the video + audio
// pools SelectBestDashStream picks from. It excludes both URL-less formats and
// whole-file formats (ContentLength set) — the latter is critical: a complete
// file handed to the &sq segment loop returns the ENTIRE file for every
// sequence number, re-downloading it forever (the disk-filling runaway that
// HasManifestlessDashFormats guards against at the strategy gate). This MUST
// mirror that gate's exclusion: DashStreamInfo drops ContentLength, so
// SelectBestDashStream is contentLength-blind and would otherwise pick a
// higher-res whole-file itag as "best" from a MIXED pool (OTF video+audio that
// passes the gate PLUS a contentLength video) and feed its URL to the runaway.
func partitionManifestlessFormats(formats []youtube.Format) (video, audio []DashStreamInfo) {
	for i := range formats {
		f := &formats[i]
		if f.URL == "" || f.ContentLength != "" {
			continue
		}
		info := formatToDashStreamInfo(f)
		if f.IsAudio() {
			audio = append(audio, info)
		} else if info.Width > 0 {
			video = append(video, info)
		}
	}
	return video, audio
}

// DownloadManifestlessDash sets up a download for streams that ship adaptive
// formats with direct URLs but no DASH manifest. Triggered by the YouTube
// account experiment that strips dashManifestUrl from cookied responses
// (yt-dlp issue #15274) — the watch-page player response still carries
// streamingData.adaptiveFormats[] with full URLs that we can fetch with
// `&sq=N` appended.
//
// Pipeline:
//  1. Filter videoInfo.Formats[] into video and audio adaptive entries.
//  2. Use SelectBestDashStream to pick best video + audio per the user's
//     quality preference (same selector DASH manifest path uses).
//  3. Decrypt the n-param in each selected URL via the routed cipher solver
//     (sidecar primary, goja fallback). parseFormatsWithCipher already does
//     this during player-response parsing, but we re-route here so that any
//     orchestrator-driven URL refresh on cipher rotation goes through the
//     same path.
//  4. Mint a POT bound to videoID — the experiment's GVS POT policy for
//     cookied clients is video-id-bound, not visitor-data-bound.
//  5. Build engine.SegmentDownloader instances. The downloader's
//     buildSegmentURL auto-detects query-style adaptive URLs and appends
//     `&sq=N`, while applyPoTokenQuery appends `&pot=POT` per fetch. No
//     separate init segment — sq=0 carries ftyp+moov inline.
func DownloadManifestlessDash(
	ctx context.Context,
	job *JobContext,
	videoInfo *youtube.VideoInfo,
	routedSolver cipher.Solver,
	cipherSolver *cipher.GojaResolver,
	potProvider *bgutils.PotProvider,
	isOnline func() bool,
) (*DownloadResult, error) {
	// Partition videoInfo.Formats into video + audio adaptive pools (excludes
	// whole-file contentLength formats — see partitionManifestlessFormats).
	videoStreams, audioStreams := partitionManifestlessFormats(videoInfo.Formats)

	// Per-job itag selection overrides config defaults.
	videoItag := job.Config.VideoItag
	audioItag := job.Config.AudioItag
	if job.Job.SelectedVideoItag != nil {
		videoItag = *job.Job.SelectedVideoItag
	}
	if job.Job.SelectedAudioItag != nil {
		audioItag = *job.Job.SelectedAudioItag
	}
	if job.Job.QualityPreference == "audio_only" && job.Job.SelectedVideoItag == nil {
		videoItag = -1
		job.Logger.Info("audio_only preference: skipping video stream")
	}

	var videoStream, audioStream *DashStreamInfo
	if videoItag != -1 {
		videoStream = SelectBestDashStream(videoStreams, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference)
	}
	if audioItag != -1 {
		audioStream = SelectBestDashStream(audioStreams, audioItag, 0, false, "")
	}
	if videoStream == nil && videoItag != -1 {
		return nil, fmt.Errorf("manifestless DASH: no suitable video adaptive format")
	}
	if audioStream == nil && audioItag != -1 {
		return nil, fmt.Errorf("manifestless DASH: no suitable audio adaptive format")
	}
	if videoStream == nil && audioStream == nil {
		return nil, fmt.Errorf("manifestless DASH: no streams selected")
	}

	job.Logger.Info("manifestless DASH selected",
		"videoItag", itagOf(videoStream),
		"audioItag", itagOf(audioStream),
		"videoQuality", qualityOf(videoStream))

	// Stage 3 of the cipher pipeline rework deferred sig+n decryption
	// from parse-time to here: parseFormats leaves Format.URL raw (with
	// EncryptedSig captured for sigCipher entries), and we resolve the
	// chosen video + audio URLs right before constructing the segment
	// downloader. Bound the re-selection to one fallback so a fully
	// broken cipher state surfaces as a clean setup error rather than
	// looping every itag.
	excludedVideoItags := map[int]bool{}
	excludedAudioItags := map[int]bool{}
	if videoStream != nil {
		resolved, retryStream, err := resolveManifestlessStream(ctx, videoInfo.Formats, videoStreams, videoStream, videoItag, job.Config.MaxVideoResolution, true, job.Job.QualityPreference, routedSolver, cipherSolver, videoInfo.PlayerURL, excludedVideoItags, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve video URL: %w", err)
		}
		if retryStream != nil {
			videoStream = retryStream
		}
		videoStream.BaseURL = resolved
	}
	if audioStream != nil {
		resolved, retryStream, err := resolveManifestlessStream(ctx, videoInfo.Formats, audioStreams, audioStream, audioItag, 0, false, "", routedSolver, cipherSolver, videoInfo.PlayerURL, excludedAudioItags, job.Logger)
		if err != nil {
			return nil, fmt.Errorf("manifestless DASH: resolve audio URL: %w", err)
		}
		if retryStream != nil {
			audioStream = retryStream
		}
		audioStream.BaseURL = resolved
	}

	// Mint a POT bound to the videoID rather than visitor data. The
	// experiment that strips dashManifestUrl from cookied clients also
	// switches GVS POT enforcement to videoID-binding (yt-dlp logs
	// "Detected experiment to bind GVS PO Token to video id"). Falling
	// back to visitorData here would produce a POT that YouTube's CDN
	// rejects on every segment fetch.
	var pot string
	if potProvider != nil {
		token, err := potProvider.GeneratePoTokenString(ctx, job.Job.VideoID, false)
		if err != nil {
			job.Logger.Warn("manifestless DASH: PO token mint failed", "err", err)
		} else if token != "" {
			pot = token
			job.Logger.Info("[POT] PO token ready for manifestless DASH",
				"binding", "videoID", "tokenLength", len(token))
		}
	}

	result := &DownloadResult{}

	var cookieHeader string
	if job.YT != nil {
		cookieHeader = job.YT.GetCookieHeader()
	}

	// Orchestrator-provided start sequences (quality recovery / split path);
	// fall back to DB persisted last seq for crash recovery of a LIVE capture.
	// dbResumeSeq deliberately returns 0 for a finished post-live/VOD stream —
	// manifestless segments carry their ftyp+moov init only at sq=0, so a VOD
	// must start there; an interrupted VOD download resumes via the engine's
	// .resume.json sidecar, not the (possibly stale) job seq column.
	videoStartSeq := 0
	forceVideoSeq := false
	if job.VideoStartSeq > 0 {
		videoStartSeq = job.VideoStartSeq
		forceVideoSeq = true
	} else {
		videoStartSeq = dbResumeSeq(videoInfo.StreamStatus, job.Job.LastVideoSeq)
	}
	audioStartSeq := 0
	forceAudioSeq := false
	if job.AudioStartSeq > 0 {
		audioStartSeq = job.AudioStartSeq
		forceAudioSeq = true
	} else {
		audioStartSeq = dbResumeSeq(videoInfo.StreamStatus, job.Job.LastAudioSeq)
	}

	if videoStream != nil {
		result.HasVideo = true
		result.VideoPath = filepath.Join(job.StagingDir, "video_stream")
		result.VideoWidth = videoStream.Width
		result.VideoHeight = videoStream.Height
		result.VideoFps = videoStream.FPS
		// A part that force-starts mid-stream (quality split / restart) begins
		// at sq>0, but manifest-free DASH carries the ftyp+moov init only inline
		// at sq=0. Point InitURL at sq=0 so the engine prepends the extracted
		// init; otherwise the part file is a bare moof+mdat that won't mux.
		videoInitURL := ""
		if forceVideoSeq && videoStartSeq > 0 {
			videoInitURL = manifestlessSq0URL(videoStream.BaseURL)
		}
		result.VideoDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:         videoStream.BaseURL,
			OutputFile:      result.VideoPath,
			StartSeq:        videoStartSeq,
			ForceStartSeq:   forceVideoSeq,
			InitURL:         videoInitURL,
			InitFromSegment: videoInitURL != "",
			PoToken:         pot,
			CookieHeader:    cookieHeader,
			MaxTimeout:      time.Duration(job.Config.MaximumTimeout) * time.Second,
			IsOnline:        isOnline,
			Logger:          job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		// Same 403 invalidation chain as DASH-manifest path. The shared
		// helper handles cipher solver + POT cache + visitor-data wipe.
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.VideoDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH video")
				return ""
			}
		}
		if videoStream.Width > 0 || videoStream.Height > 0 {
			job.DB.UpdateJobFields(job.Job.ID, map[string]any{
				"video_width":  videoStream.Width,
				"video_height": videoStream.Height,
			})
		}
	}

	if audioStream != nil {
		result.HasAudio = true
		result.AudioPath = filepath.Join(job.StagingDir, "audio_stream")
		audioInitURL := ""
		if forceAudioSeq && audioStartSeq > 0 {
			audioInitURL = manifestlessSq0URL(audioStream.BaseURL)
		}
		result.AudioDownloader = engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:         audioStream.BaseURL,
			OutputFile:      result.AudioPath,
			StartSeq:        audioStartSeq,
			ForceStartSeq:   forceAudioSeq,
			InitURL:         audioInitURL,
			InitFromSegment: audioInitURL != "",
			PoToken:         pot,
			CookieHeader:    cookieHeader,
			MaxTimeout:      time.Duration(job.Config.MaximumTimeout) * time.Second,
			IsOnline:        isOnline,
			Logger:          job.Logger,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				info, err := job.YT.ProbeVideoStatus(ctx, job.Job.VideoID)
				if err != nil {
					return false, err
				}
				return info.StreamStatus != youtube.StreamLive, nil
			},
		})
		if videoInfo.PlayerURL != "" && (routedSolver != nil || cipherSolver != nil) {
			result.AudioDownloader.OnCipherFailure = func() string {
				invalidate403Caches(job, videoInfo.PlayerURL, cipherSolver, potProvider, "manifestless DASH audio")
				return ""
			}
		}
	}

	return result, nil
}

// dbResumeSeq returns the DB-persisted last sequence to resume a crashed LIVE
// manifestless capture from, or 0 when none applies. It returns 0 for any
// non-live stream (post-live / VOD): manifestless segments carry their
// ftyp+moov init only at sq=0, so a finished stream must start there rather
// than seed a stale live seq left over from an earlier live recording of the
// same job. Interrupted VOD downloads resume via the engine's .resume.json
// sidecar instead, which is keyed to the actual bytes on disk.
func dbResumeSeq(streamStatus youtube.StreamStatus, lastSeq *int) int {
	if streamStatus != youtube.StreamLive {
		return 0
	}
	if lastSeq != nil && *lastSeq > 0 {
		return *lastSeq
	}
	return 0
}

// manifestlessSq0URL returns the sq=0 segment URL for a query-style manifest-
// free DASH base URL, used to fetch a part's inline ftyp+moov init. These
// adaptiveFormats URLs always carry query params, so &sq=0 is the correct
// separator (mirrors the engine's buildSegmentURL query-style form); the ?
// fallback is defensive.
func manifestlessSq0URL(base string) string {
	if strings.Contains(base, "?") {
		return base + "&sq=0"
	}
	return base + "?sq=0"
}

func itagOf(s *DashStreamInfo) int {
	if s == nil {
		return 0
	}
	return s.Itag
}

func qualityOf(s *DashStreamInfo) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%dx%d@%d", s.Width, s.Height, s.FPS))
}
