package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
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

// vodRefusalExpiryMargin is how close to the expiry a refused URL states
// (formatURLExpiry) the refusal must come to be read as that expiry. A
// googlevideo URL lives about six hours and a long transfer meets its end at
// whatever chunk is in flight, so a refusal with more than half an hour left
// on the URL was for something else — the field case's tv_auth URLs were
// refused at their first request, hours before their expire= — while the
// margin leaves room for a local clock that is off and an origin that stops
// a little early.
const vodRefusalExpiryMargin = 30 * time.Minute

// vodRefusalExpiryAge stands in for the expiry on a URL that states none: a
// refusal within this long of the URL being handed out is not its expiry. It
// is vodInfoMaxAge, the age at which a VOD's extraction is re-extracted
// before its download starts, for the same reason: well inside the usual
// lifetime.
const vodRefusalExpiryAge = vodInfoMaxAge

// vodRefreshNow is the whole-file refresh's clock — when a URL is handed out
// and when its refusal lands, which the token fallback's expiry rule reads
// (vodRefreshState.fallbackStep). A package var so the tests can land a
// refusal hours before an expiry, or an hour after a hand-out, without
// waiting for either. Production never writes it.
var vodRefreshNow = time.Now

// vodRefreshMinter is the minter DownloadVod hands each whole-file refresh,
// the job's *bgutils.PotProvider, behind a package var so a test can drive
// the token fallback end to end with a fake: the provider is concrete, and a
// zero one reaches for BotGuard. A nil provider stays a typed nil, which
// minterUsable reads as no minter. Production never writes it.
var vodRefreshMinter = func(p *bgutils.PotProvider) gvsTokenMinter { return p }

// errNoGvsMinter is the token fallback's cause when no PO token provider is
// wired to mint with (minterUsable false).
var errNoGvsMinter = errors.New("no PO token provider")

// takesNoWebPo reports whether a URL from source must never carry a WebPO
// GVS token, whatever a refusal suggests: every URL does but one from a
// client upstream binds a WebPO to. Those are pot/utils.py's WEBPO_CLIENTS —
// here TVHTML5 (tv_public, tv_auth), WEB_EMBEDDED_PLAYER (web_embedded) and
// the web family (watch_page, web, web_safari, web_creator) — and for any
// other client get_webpo_content_binding answers (None, None), so upstream
// sends it no WebPO, fetch_pot=always included: visionos, and android_vr and
// its DASH fallback, a WebPO on whose URLs is the 2026-08-15 403 cause the
// AuthLevel block names. An empty or unrecognised label takes none either,
// as GvsTokenRequired reads one: a URL nobody can attribute to a WebPO
// client gets no WebPO.
//
// Such a URL reaches the whole-file path as a missing_pot copy, or as the
// token-free copy a refresh finds of a tv or web_embedded stream whose own
// client no longer returns the file (sameVodFile reads a web-family winner's
// TokenFreeAlternate, and the cookieless chain answers nothing else). The
// token fallback never puts a token on one: it skips its first step for a
// refused URL of such a client, and a token minted for a stream whose fresh
// copy is one is for the token-carrying copy alone (refreshVodURL). The
// engine cannot clear a token once it holds one (SetPoToken ignores ""), so
// one left on such a URL would ride every request after.
func takesNoWebPo(source string) bool {
	switch source {
	case "tv_public", "tv_auth", "web_embedded", "watch_page", "web", "web_safari", "web_creator":
		return false
	}
	return true
}

// vodRefreshState is what one whole-file stream's refresh remembers between
// the refusals it answers: the format the stream rides, the URL it last
// handed the engine and that URL's circumstances, and how far the token
// fallback has gone (see refreshVodURL). One per downloader, from
// vodURLRefresh, never shared between the video and the audio.
type vodRefreshState struct {
	// mu is held for a whole refresh. The engine calls OnCredentialRefresh on
	// the goroutine that runs the whole-file download — the size probe, the
	// chunk loop and the streaming fallback run in turn on it, one request at
	// a time, and none starts a goroutine — so it is never contended. It is
	// here so that a second caller, should the engine ever grow one, reads
	// the state the first refresh left instead of racing it: each step is
	// decided on the one before.
	mu sync.Mutex
	// served is the format the stream rides: DownloadVod's choice, after
	// re-selection and any missing_pot swap, until the fallback's second step
	// moves the stream onto a token-carrying copy of the same file.
	served youtube.Format
	// url is the URL last handed to the engine — at first the one DownloadVod
	// resolved for served — and issued when it was handed out.
	url    string
	issued time.Time
	// urlSource is the client url came from (Format.Source): served's at
	// first, and each hand-out's own after. A refresh may hand out another
	// client's copy of the same class — a tv_auth stream's fresh token-free
	// copy can be android_vr's — without the stream changing class, and
	// whether a token may ride the current URL is that client's question
	// (takesNoWebPo), not served's.
	urlSource string
	// adEnd is when that URL's own pre-roll ad is over — its format's
	// AvailableAt, as the response it came from stamped it — or zero when that
	// response's verdict says nothing waits (adWindowEnd).
	adEnd time.Time
	// step is the last token-fallback step that ran: 0 none, 1 a token on
	// the token-free URLs, 2 the token-carrying copy looked for.
	step int
	// tokenised records that a minted token rides this token-free stream: a
	// fallback step handed one out, and every later refresh mints one too.
	tokenised bool
}

// newVodRefreshState seeds a stream's refresh state from the format it was
// built from and the URL its first request goes out on, handed out now.
// info is the extraction served came from, whose verdict says whether its ad
// is waited for — as DownloadVod waited on it.
func newVodRefreshState(info *youtube.VideoInfo, served youtube.Format, url string) *vodRefreshState {
	return &vodRefreshState{served: served, url: url, issued: vodRefreshNow(), urlSource: served.Source, adEnd: adWindowEnd(info, &served)}
}

// adWindowEnd is when f's pre-roll ad is over, as a download from info waits
// for it: f's AvailableAt, or zero when upstream's verdict on info exempts it
// from the wait (youtube.PlaybackWaitApplies) — a URL nobody waits for has no
// ad window to be refused inside.
func adWindowEnd(info *youtube.VideoInfo, f *youtube.Format) time.Time {
	if f == nil || !youtube.PlaybackWaitApplies(info) {
		return time.Time{}
	}
	return f.AvailableAt
}

// fallbackStep reads the refusal of the URL st last handed out, landing at
// now, and returns the token-fallback step it calls for — 0 for none —
// logging the verdict unless it is the expiry. Only a token-free stream asks.
//
// The expiry: a URL refused within vodRefusalExpiryMargin of the expiry it
// states, or past it, or — stating none — refused vodRefusalExpiryAge or more
// after it was handed out. That is the refusal this refresh was built for,
// and it runs as it always did.
//
// The ad: a URL refused before its own pre-roll ad was over (st.adEnd) was
// refused for the ad, not for a token, and the answer is a fresh URL, which
// waits out its own ad (waitForFreshPlayback), not a token. Every path waits
// before the first request on a URL — DownloadVod's waitForPlayback, the
// refresh's waitForFreshPlayback — so this does not normally happen; it is
// here so that the ad wait and the token fallback cannot fight, should a URL
// ever go out early or a local clock run behind YouTube's.
//
// Anything else is early: the next step, 1 and then 2, and 2 again for a
// stream that found no token-carrying copy to move to the last time. A
// refused URL from a client that takes no WebPO (takesNoWebPo of
// st.urlSource) goes to 2 at once: step 1 would put the token on another
// URL of that client.
func (st *vodRefreshState) fallbackStep(job *JobContext, tag string, now time.Time) int {
	if now.Before(st.adEnd) {
		job.Logger.Info("[POT] VOD URL refresh: refused inside its own pre-roll ad — a fresh URL, not the token fallback",
			"jobID", job.Job.ID, "tag", tag, "source", st.urlSource, "adLeft", st.adEnd.Sub(now).Round(time.Second).String())
		return 0
	}
	evidence := []any{"age", now.Sub(st.issued).Round(time.Second).String()}
	if exp, ok := formatURLExpiry(st.url); ok {
		if exp.Sub(now) <= vodRefusalExpiryMargin {
			return 0
		}
		evidence = []any{"expiresIn", exp.Sub(now).Round(time.Second).String()}
	} else if now.Sub(st.issued) >= vodRefusalExpiryAge {
		return 0
	}
	step := min(st.step+1, 2)
	if takesNoWebPo(st.urlSource) {
		step = 2
	}
	job.Logger.Info("[POT] VOD URL refresh: a token-free URL was refused long before its expiry — falling back to a GVS token",
		append([]any{"jobID", job.Job.ID, "tag", tag, "step", step, "source", st.urlSource}, evidence...)...)
	return step
}

// noteToken records a refresh's token: one handed out on a token-free stream
// rides it from now on (a refresh that hands out no URL leaves the token on
// the current one), and a fallback step that minted one has run.
func (st *vodRefreshState) noteToken(token string, step int) {
	if token != "" && !youtube.GvsTokenRequired(st.served.Source) {
		st.tokenised = true
	}
	if step > 0 {
		st.step = step
	}
}

// handOut records the URL a refresh hands the engine: resolved from f, a
// format of the fresh extraction info, with token. A format of the other
// token class is the fallback's second step, and the stream rides that copy
// from now on.
func (st *vodRefreshState) handOut(info *youtube.VideoInfo, f *youtube.Format, url, token string, step int) {
	if tokenClassChanged(st.served.Source, f.Source) {
		st.served = *f
		st.served.TokenFreeAlternate, st.served.TokenAlternate = nil, nil
	}
	st.noteToken(token, step)
	st.url, st.issued, st.urlSource, st.adEnd = url, vodRefreshNow(), f.Source, adWindowEnd(info, f)
}

// vodURLRefresh builds a whole-file downloader's OnCredentialRefresh for the
// stream it serves. served is the format the downloader was built from —
// after re-selection and any missing_pot swap — copied, so the closure holds
// what was actually fetched, and servedURL is the URL resolved from it, the
// one the downloader's first request goes out on; together they seed the
// stream's vodRefreshState, which the token fallback steps from. The GVS
// binding is resolved once, here, for the reason refreshGvsCredentials' doc
// gives.
func vodURLRefresh(ctx context.Context, job *JobContext, videoInfo *youtube.VideoInfo, served youtube.Format, servedURL string, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider gvsTokenMinter, tag string) func() (string, string) {
	binding, _ := gvsBinding(job, videoInfo)
	st := newVodRefreshState(videoInfo, served, servedURL)
	return func() (string, string) {
		return refreshVodURL(ctx, job, videoInfo.PlayerURL, st, routedSolver, cipherSolver, potProvider, binding, tag)
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
// The token fallback. A token-free stream — tv, web_embedded, a missing_pot
// copy — whose URL is refused long before its expiry was not refused for the
// expiry. The field case's tv_auth URLs were refused at their very first
// request, every fresh one too, for a signed-in account without Premium,
// while a Premium account downloaded the same video: most likely YouTube
// wanting a GVS PO token on those URLs from an account upstream does not
// exempt (the web family's policy is not_required_for_premium). So such
// a refusal (vodRefreshState.fallbackStep) moves the stream one step on:
//
//  1. a fresh URL of the same token-free class, with a minted token;
//  2. the same file from a client that requires the token — the fresh
//     pool's token-carrying copy (Format.TokenAlternate; sameVodFile's
//     identity) — with a minted token. The stream rides that copy from then
//     on (st.served becomes it), so every later refresh is the ordinary
//     token-requiring one. With no such copy it is step 1's shape again, and
//     an Info line says there is none.
//
// No token ever rides a URL from a client that takes no WebPO (takesNoWebPo:
// visionos, android_vr). A refused URL of one skips step 1, and a fresh
// token-free copy of one — a missing_pot copy's, or the one a tv stream's
// refresh finds behind a web-family winner once tv no longer returns the
// file — is passed over for the token-carrying copy whenever a token would
// ride it: a fresh one, or the one a tokenised stream already holds, which
// the engine would send on it unasked. With no copy to move to, a stream not
// yet tokenised is refreshed bare — the token was for the copy alone — and a
// tokenised one keeps its current URL, the one URL it can still send.
//
// The engine asks at most directRefreshAttempts (2) refreshes per refused
// request, so a size probe refused at once runs exactly those two steps.
// Once a token rides a token-free stream it rides every later refresh too
// (st.tokenised): the engine cannot clear one, and URLs that worked with one
// are not sent without it. A refusal that reads as the expiry, or one inside
// the URL's own pre-roll ad window, is no reason to change the token class
// and runs the refresh as it always ran: a fresh URL of the same class, bare
// unless already tokenised. With no token to mint — no usable minter, or a
// mint that fails or comes back empty — no step can run: the refresh runs
// bare as it always did, and a Warn says so. A download that is never
// refused, a token-requiring stream and an expiry are untouched by all of it.
//
// The token half runs first, as in refreshGvsCredentials, and is gated by the
// SERVED stream's own client: a stream riding a missing_pot shadow was left
// bare and stays bare until the fallback says otherwise, which is why the
// served format is held in st rather than looked up by itag in the setup
// pool (formatSourceByItag would name the winner's client). bypassCache,
// because the cached token may be the credential that was just refused.
//
// The re-mint, re-fetch and resolve are bounded by credentialRefreshTimeoutFor
// like the live refresh: the engine waits on this call. The fresh response's
// pre-roll ad is then waited out on ctx, beyond that bound
// (waitForFreshPlayback; credentialRefreshTimeoutFor says why), whichever
// step the URL is for: each comes from a player response with an ad of its
// own, the token-carrying copy from its own client's. Never returns an error
// — an empty URL keeps the current one, an empty token the current token, and
// the chunk's 403 then stands. st is the stream's state (vodURLRefresh), held
// locked throughout.
func refreshVodURL(ctx context.Context, job *JobContext, playerURL string, st *vodRefreshState, routedSolver cipher.Solver, cipherSolver *cipher.GojaResolver, potProvider gvsTokenMinter, binding, tag string) (baseURL, poToken string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	refreshCtx, cancel := context.WithTimeout(ctx, credentialRefreshTimeoutFor(job.Config))
	defer cancel()

	served := st.served
	bare := !youtube.GvsTokenRequired(served.Source)
	step := 0
	if bare {
		step = st.fallbackStep(job, tag, vodRefreshNow())
	}
	// keep is every exit that hands out no fresh URL: the current URL stays,
	// and the token rides it — step 1's shape, for a token-free stream — save
	// on a URL from a client that takes no WebPO (st.urlSource), for which a
	// token was minted for the copy alone. A fresh response's ad wait cut
	// short by the job's cancel ends here too and still hands the token back,
	// as a token-requiring stream's refresh always has: a "refreshed" line in
	// a cancelled job's log, and nothing more — the download ends on the
	// cancel either way.
	keep := func() (string, string) {
		if bare && takesNoWebPo(st.urlSource) {
			return "", ""
		}
		st.noteToken(poToken, step)
		return "", poToken
	}
	if !bare || st.tokenised || step > 0 {
		var mintErr error = errNoGvsMinter
		if minterUsable(potProvider) {
			if poToken, mintErr = potProvider.GeneratePoTokenString(refreshCtx, binding, true); mintErr != nil {
				poToken = ""
				if step == 0 {
					job.Logger.Warn("[POT] VOD URL refresh: re-mint failed",
						"jobID", job.Job.ID, "tag", tag, "err", mintErr)
				}
			} else if poToken == "" {
				mintErr = errEmptyGvsToken
			}
		}
		if step > 0 && poToken == "" {
			job.Logger.Warn("[POT] VOD URL refresh: no GVS token to fall back to — refreshing the token-free URL bare",
				"jobID", job.Job.ID, "tag", tag, "step", step, "err", mintErr)
			step = 0
		}
	}

	if job.YT == nil {
		job.Logger.Warn("[POT] VOD URL refresh: no YouTube service to re-extract with — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag)
		return keep()
	}
	fresh, err := refreshVideoInfo(job.YT, refreshCtx, job.Job.VideoID)
	if err != nil || fresh == nil {
		job.Logger.Warn("[POT] VOD URL refresh: player response re-fetch failed — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "err", err)
		return keep()
	}
	var f *youtube.Format
	if step == 2 {
		f = vodFileCopy(fresh.Formats, &served, true)
	}
	if f == nil {
		f = sameVodFile(fresh.Formats, &served)
	}
	if f == nil && bare {
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
		return keep()
	}
	// The token-free copy found may be from a client that takes no WebPO —
	// a missing_pot copy's own, or android_vr's behind a web-family winner
	// for a tv stream whose client no longer returns the file — whatever
	// client the stream was built from. A token that would ride it, minted
	// for a step or already held by a tokenised stream (the engine sends
	// that one unasked), goes on the token-carrying copy instead; with none,
	// a fresh token is dropped and the copy goes out bare, as it always did,
	// while a tokenised stream keeps the URL it has.
	if bare && takesNoWebPo(f.Source) && (poToken != "" || st.tokenised) {
		if c := vodFileCopy(fresh.Formats, &served, true); c != nil {
			job.Logger.Info("[POT] VOD URL refresh: the same file's token-free copy is from a client that takes no WebPO — moving onto its token-carrying copy",
				"jobID", job.Job.ID, "tag", tag, "itag", served.Itag, "clen", served.ContentLength, "source", f.Source)
			f, step = c, 2
		} else if st.tokenised {
			job.Logger.Warn("[POT] VOD URL refresh: the same file's only fresh copy is from a client that takes no WebPO, and a token rides this stream — keeping the current URL",
				"jobID", job.Job.ID, "tag", tag, "itag", served.Itag, "clen", served.ContentLength, "source", f.Source)
			return keep()
		} else {
			job.Logger.Info("[POT] VOD URL refresh: no token-carrying copy of the same file — its token-free copy is from a client that takes no WebPO, so it is refreshed bare",
				"jobID", job.Job.ID, "tag", tag, "itag", served.Itag, "clen", served.ContentLength, "source", f.Source)
			poToken, step = "", 0
		}
	} else if step == 2 && !youtube.GvsTokenRequired(f.Source) {
		job.Logger.Info("[POT] VOD URL refresh: no token-carrying copy of the same file — the token rides a token-free URL again",
			"jobID", job.Job.ID, "tag", tag, "itag", served.Itag, "clen", served.ContentLength, "source", served.Source)
	}
	if fresh.PlayerURL != "" {
		playerURL = fresh.PlayerURL
	}
	resolved, err := resolveFormatURL(refreshCtx, f, routedSolver, cipherSolver, playerURL, job.Logger)
	if err != nil {
		job.Logger.Warn("[POT] VOD URL refresh: URL re-resolve failed — keeping the current URL",
			"jobID", job.Job.ID, "tag", tag, "itag", f.Itag, "err", err)
		return keep()
	}
	// The fresh response carries a pre-roll ad of its own, and its URL is
	// refused until it is over (waitForFreshPlayback). Waited out on ctx, not
	// refreshCtx: the bound is for the work above, which can hang, and this
	// wait ends at an instant the response named — see
	// credentialRefreshTimeoutFor. Every fallback step's URL waits here too,
	// gated on the fresh extraction's own verdict.
	if !waitForFreshPlayback(ctx, job, fresh, f, tag) {
		return keep()
	}
	st.handOut(fresh, f, resolved, poToken, step)
	job.Logger.Info("[POT] VOD URL refresh", "jobID", job.Job.ID, "tag", tag,
		"itag", f.Itag, "source", f.Source, "newToken", poToken != "", "step", step)
	return resolved, poToken
}

// sameVodFile returns the format in formats that serves served's file from a
// client of the same GVS token class (vodFileCopy), or nil: a refresh never
// changes the class on its own — see tokenClassChanged — and only the token
// fallback's second step does, on purpose and with a minted token
// (refreshVodURL).
func sameVodFile(formats []youtube.Format, served *youtube.Format) *youtube.Format {
	return vodFileCopy(formats, served, youtube.GvsTokenRequired(served.Source))
}

// vodFileCopy returns the format in formats that serves served's file — the
// same itag, audio track and byte length (ContentLength) — from a client whose
// GVS token requirement (youtube.GvsTokenRequired) is tokenRequired, or nil.
// Each entry is read with its shadow: a winner of the other class carries the
// copy of this one behind it — the token-free shadow (TokenFreeAlternate) a
// missing_pot stream rides, or the token-carrying one (TokenAlternate) the
// token fallback moves a stream onto, and a stream that moved stays on. A
// matching itag with another length is a re-encode, not the file the partial
// is a prefix of, and does not match.
func vodFileCopy(formats []youtube.Format, served *youtube.Format, tokenRequired bool) *youtube.Format {
	for i := range formats {
		for _, c := range []*youtube.Format{&formats[i], formats[i].TokenFreeAlternate, formats[i].TokenAlternate} {
			if c != nil && c.URL != "" && c.Itag == served.Itag && c.AudioTrackID == served.AudioTrackID &&
				c.ContentLength == served.ContentLength && youtube.GvsTokenRequired(c.Source) == tokenRequired {
				return c
			}
		}
	}
	return nil
}
