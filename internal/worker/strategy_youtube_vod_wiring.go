package worker

import (
	"context"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// vodStreamID is the engine StreamID of one whole-file VOD stream: the video,
// the itag and the file's byte length (Format.ContentLength, the URL's clen).
// The staging names are fixed — every rendition lands in video.mp4 or
// audio.m4a — so the sidecar a run leaves is all that says WHICH file its
// bytes are a prefix of, and a resume after the selection moved (a changed
// max_video_resolution or prefer_60fps, a missing_pot degrade, a different
// format pool) must not append one rendition to another's checkpoint. The
// engine compares this before anything else (resumeIdentityMismatch) and
// starts a mismatched whole-file download over.
func vodStreamID(videoID string, f *youtube.Format) string {
	return fmt.Sprintf("%s/itag=%d/clen=%s", videoID, f.Itag, f.ContentLength)
}

// vodURLRefresh builds a whole-file downloader's OnCredentialRefresh for the
// stream it serves. served is the format the downloader was built from —
// after re-selection and any missing_pot swap — copied, so the closure holds
// what was actually fetched. The GVS binding is resolved once, here, for the
// reason refreshGvsCredentials' doc gives.
func vodURLRefresh(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, served youtube.Format, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider gvsTokenMinter, tag string) func() (string, string) {
	binding, _ := gvsBinding(job, videoInfo)
	return func() (string, string) {
		return refreshVodURL(ctx, job, videoInfo.PlayerURL, served, routedSolver, cipherSolver, potProvider, binding, tag)
	}
}

// refreshVodURL answers a whole-file chunk the origin refused with 403 or 410
// — a googlevideo URL lives about six hours, and a long transfer outlives it.
// It re-fetches the player response (refreshVideoInfo, the live refresh's own
// seam) and resolves the format serving the SAME file (sameVodFile): a URL
// for any other would append a different rendition mid-file, so none is
// returned and the engine's refusal stands. The engine checks the URL's
// fingerprint again before installing it (refreshDirectURL). A token-free
// stream the fresh pool does not serve is looked for in the cookieless chain
// too, where DownloadVod's missing_pot re-extract found it.
//
// The token half runs first, as in refreshGvsCredentials, and is gated by the
// SERVED stream's own client: a stream riding a missing_pot shadow was left
// bare and stays bare, which is why the served format is handed in rather
// than looked up by itag in the setup pool (formatSourceByItag would name the
// winner's client). bypassCache, because the cached token may be the
// credential that was just refused.
//
// Bounded by credentialRefreshTimeoutFor like the live refresh: the engine
// waits on this call. Never returns an error — an empty URL keeps the current
// one, an empty token the current token, and the chunk's 403 then stands.
func refreshVodURL(ctx context.Context, job *JobContext, playerURL string, served youtube.Format, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider gvsTokenMinter, binding, tag string) (baseURL, poToken string) {
	refreshCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(job.Config))
	defer cancel()

	if youtube.GvsTokenRequired(served.Source) && minterUsable(potProvider) {
		if token, err := potProvider.GeneratePoTokenString(refreshCtx, binding, true); err != nil {
			job.Logger.Warn("[POT] VOD URL refresh: re-mint failed",
				"jobID", job.Job.ID, "tag", tag, "err", err)
		} else {
			poToken = token
		}
	}

	if job.YT == nil {
		job.Logger.Warn("[POT] VOD URL refresh: no YouTube service to re-extract with — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag)
		return "", poToken
	}
	fresh, err := refreshVideoInfo(job.YT, refreshCtx, job.Job.VideoID)
	if err != nil || fresh == nil {
		job.Logger.Warn("[POT] VOD URL refresh: player response re-fetch failed — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "err", err)
		return "", poToken
	}
	f := sameVodFile(fresh.Formats, &served)
	if f == nil && !youtube.GvsTokenRequired(served.Source) {
		// A token-free stream DownloadVod's missing_pot re-extract served is
		// missing from the fresh pool for the reason it was missing from the
		// first: the cascade runs the cookieless chain only when web_creator
		// is inadequate, so an adequate pool carries no shadow to find. Ask
		// that chain again, as DownloadVod did, under the same bound.
		// sameVodFile reads every format and shadow it is handed, so the
		// fetch needs no merge into the pool to be searched. A stream that
		// needs a token cannot come from that chain, so its refresh does not
		// ask it.
		extra, rxErr := fetchCookielessFormats(job.YT, refreshCtx, job.Job.VideoID)
		if rxErr != nil {
			job.Logger.Warn("[POT] VOD URL refresh: cookieless re-extract failed",
				"jobID", job.Job.ID, "tag", tag, "err", rxErr)
		} else {
			f = sameVodFile(extra, &served)
		}
	}
	if f == nil {
		job.Logger.Warn("[POT] VOD URL refresh: the re-extraction serves no format for the same file — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "itag", served.Itag, "clen", served.ContentLength, "source", served.Source)
		return "", poToken
	}
	if fresh.PlayerURL != "" {
		playerURL = fresh.PlayerURL
	}
	resolved, err := resolveFormatURL(refreshCtx, f, routedSolver, cipherSolver, playerURL, job.Logger)
	if err != nil {
		job.Logger.Warn("[POT] VOD URL refresh: URL re-resolve failed — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "itag", f.Itag, "err", err)
		return "", poToken
	}
	job.Logger.Info("[POT] VOD URL refresh", "jobID", job.Job.ID, "tag", tag,
		"itag", f.Itag, "source", f.Source, "newToken", poToken != "")
	return resolved, poToken
}

// sameVodFile returns the format in formats that serves served's file — the
// same itag, audio track and byte length (ContentLength) — from a client of
// the same GVS token class (tokenClassChanged), or nil. A winner of the other
// class is looked behind for its token-free shadow (TokenFreeAlternate), the
// copy a missing_pot stream rides. A matching itag with another length is a
// re-encode, not the file the partial is a prefix of, and does not match.
func sameVodFile(formats []youtube.Format, served *youtube.Format) *youtube.Format {
	for i := range formats {
		for _, c := range []*youtube.Format{&formats[i], formats[i].TokenFreeAlternate} {
			if c != nil && c.URL != "" && c.Itag == served.Itag && c.AudioTrackID == served.AudioTrackID &&
				c.ContentLength == served.ContentLength && !tokenClassChanged(served.Source, c.Source) {
				return c
			}
		}
	}
	return nil
}
