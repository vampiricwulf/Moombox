# Upstream Ports — Ledger

Where the last upstream review left off, so the next one (a local session, a scheduled run, or a cloud session with no `references/` checkout) starts from a known point instead of from "whatever the local clone last pulled". The workflow that reads and updates this file is the `moombox-upstream-porting` skill (`.claude/skills/moombox-upstream-porting/SKILL.md`).

**Update this file at the end of every upstream review, including one that ports nothing.** Moving a repo's *Last reviewed* SHA forward is the record that the commits up to it were triaged; an item not ported goes under *Open upstream items*, so moving the SHA past it does not lose it.

## Last review

- **Date:** 2026-10-29
- **Last port:** `15fe37f4` — fix(youtube,chat): read ytInitialData from the dedicated script element ← yt-dlp `12ac9aca7` (#17703)
- **Ported into:** `internal/youtube/channel_membership.go` (`ytInitialDataAnchors`, which `internal/youtube/watch_page.go` shares) and `internal/chat/api.go` (its twin anchor set)

## Upstream repos — last reviewed commit

A cloud session has no `references/` (it is gitignored). Clone what the review needs and list what is new since the SHA below:

```bash
git clone --filter=blob:none https://github.com/yt-dlp/yt-dlp.git /tmp/yt-dlp
git -C /tmp/yt-dlp log --oneline 34ea29166e37d6f16849217982d0429aafc463e8..HEAD -- yt_dlp/extractor/youtube yt_dlp/extractor/twitch.py yt_dlp/downloader yt_dlp/networking yt_dlp/cookies.py
```

Locally, `bash references/update-all.sh` pulls all seven, but its "new commits" are only those since the local clone's previous pull. The SHAs below are the durable record: compare against them, not against the script's summary.

| Repo | Clone URL | Last reviewed | Commit date | Notes |
|---|---|---|---|---|
| yt-dlp | https://github.com/yt-dlp/yt-dlp.git | `34ea29166e37d6f16849217982d0429aafc463e8` | 2026-10-29 | release 2026.10.26 |
| ejs | https://github.com/yt-dlp/ejs.git | `d48c6303a5a6455a42ef9e0b445149cadea51279` | 2026-10-26 | CI-only commits past the vendored pin |
| BgUtils | https://github.com/LuanRT/BgUtils.git | `f842f8a14435a35800ff469d315bb1a7e6d7a64a` | 2026-10-23 | dev-dependency bump only |
| bgutil-ytdlp-pot-provider | https://github.com/Brainicism/bgutil-ytdlp-pot-provider.git | `26475e9d3665b972c4aeb83b4dbaafcf64b88438` | 2026-10-07 | |
| moonarchive | https://github.com/nosoop/moonarchive.git | `0da99f26c3da3cf1c22e230be1eafc08a0369927` | 2026-08-08 | |
| chatterino7 | https://github.com/SevenTV/chatterino7.git | `49f9b31cff599e88c6a64efea808ae1521186413` | 2026-10-26 | |
| moombox (Python original) | https://github.com/nosoop/moombox.git | `968d42c10a6750b8b6037070ec09f83d7e15556e` | 2026-06-08 | |

## Vendored pins (sidecar)

These are what actually ships. A repo's *Last reviewed* SHA can be ahead of its pin when the newer commits held nothing worth re-vendoring.

| Source | Pin | Where |
|---|---|---|
| ejs | `6f8587bb7009a1fc81038538071d2cb66b8b8ed0` | the VERSION file in `bgutil-sidecar/vendor/ejs/` |
| bgutils-js | `^4.0.3` | `bgutil-sidecar/package.json` |
| meriyah / astring | `6.1.4` / `1.9.0` (lockstep with ejs's own `package.json`) | `bgutil-sidecar/package.json` |

## Open upstream items

Reviewed and not ported. Newest first. Remove an entry when it is ported (and add it to the port log) or definitively rejected.

| Upstream | What | Moombox exposure | Status |
|---|---|---|---|
| yt-dlp `c7fb478d2` (issue #17666) | web_embedded can go SABR-only (itag 18 only); upstream gets HLS 91–96 back with a Safari UA | Swapping the UA on its own does nothing here: the port has three parts. | Owner deferred 2026-10-02. |

## Port log

Newest first. One row per Moombox commit that ports upstream work.

| Date | Moombox commit | Upstream | Summary |
|---|---|---|---|
| 2026-10-31 | `b816ae67` | yt-dlp `if not formats:` at `b77c9304e` (`_video.py:4045`), the condition `26feac3dd` (#12958) and `f484c5159` (#12939) word the rate limit and the captcha under | A `rate_limited` verdict on an extraction that came back with formats is playable: Process downloads it and a stale VOD's re-extraction is taken, where both waited the limit out with the formats in hand; the waiting loop's go-live already read it so (`rateLimitedFetch`) |
| 2026-10-31 | `c1df7d3f` | yt-dlp `26feac3dd` (#12958), `f484c5159` (#12939) | A rate-limit or captcha verdict (`rate_limited`) is worded as upstream words it ("Your account" / "The current session has been rate-limited by YouTube for up to an hour") and, beyond upstream, which only words it, waited out: backlog admission held for the hour, a backlog VOD back to `Queued` for it outside the retry budget, any other job fetching again every 30 minutes in place (`outlastRateLimit`) |
| 2026-10-31 | `1c4eff0f` | yt-dlp `_extract_response` at `b77c9304e` (`_base.py:1322-1356`; ytdl-org #28289, yt-dlp #839) | A player call's 429 is returned at once with its `Retry-After` (`HTTPRateLimitError`), where it was retried four times on every client; a 404 and a 200 whose last `ERROR` alert says "unknown error" are retried within the ladder |
| 2026-10-31 | `a1a86f61` | yt-dlp `a51ef56c0` (#17884), `f484c5159` (#12939), `26feac3dd` (#12958); `_video.py:4048-4081` at `b77c9304e` | The playability reason is read as upstream reads it (`readPlayabilityText`): the error screen's reason ahead of the status's, its subreason appended, the interstitial's title and description; a geo subreason is `region_blocked` whatever the status; a captcha challenge or a "try again later" is `rate_limited`. The fixtures are built from upstream's traversal paths: no capture yet shows the interstitial, the captcha or the rate limit's status code |
| 2026-10-31 | `28745ce5` | yt-dlp `8c1f07d81` (#16771) | The live adaptive arm leaves a VOD whose pool also holds a whole-file video-only + audio-only pair (`hasWholeFileSplit`) to the whole-file path: upstream's `was_live`, which skips the live adaptive formats and downloads the files |
| 2026-10-31 | `61720562` | yt-dlp `cc2db8780`, `8c1f07d81` (#16771) | A missing_pot degrade's selection is held to `segmentedStreamLost` too, over what both of its whole-file re-filters left out: a wanted stream whose only token-free copy is segmented fails the job, where it archived the rest silently |
| 2026-10-31 | `6ad3085e` | yt-dlp `cc2db8780`, `8c1f07d81` (#16771) | The whole-file VOD pool leaves segmented formats out (`wholeFileVodPool`) and fails clearly on a wanted stream served segmented alone (`segmentedStreamLost`); a `not_a_stream` VOD with a segmented pool and a manifest goes to DASH |
| 2026-10-31 | `b889d63b` | yt-dlp `8c1f07d81` (#16771) | A VOD whose split pool carries `targetDurationSec` takes the manifest-free path whatever its status (`hasLiveAdaptiveSplit`), so a Resume in the post-live window continues its capture |
| 2026-10-31 | `5d91dfab` | yt-dlp `cc2db8780`, `8c1f07d81` (#16771) | The format `type` read (`Format.IsOTF`; the commit body's `545cc85d1` is the commit that removed the old `stream_type == 3` skip); in the dedup a copy a whole-file GET cannot fetch (`Format.Segmented`: OTF always, live adaptive outside live processing) loses to another client's whole file of the same stream; the split gate takes only audio-less video (`Format.IsVideoOnly`) |
| 2026-10-31 | `ac9cc2ca` | yt-dlp `4d37720a0` (#5091) | `post_live` read off `videoDetails.isPostLiveDvr`, upstream's first `live_status` rule, at the extraction's exit (`finishExtraction`, the `get_first` verdict over its responses): TV's answer in the post-live DVR window, with no microformat, was a VOD and went to the whole-file path |
| 2026-10-31 | `e9bd795d` | yt-dlp `downloader/http.py` at `b77c9304e` ("Did not get any data blocks"); moonarchive `0da99f2` (`_dash.py`, `EmptyFragmentException`) | An empty 2xx segment body is a failed fetch, never a segment: the sequential DASH loop routes it through `handleHTTPError`'s at-edge path, and `fetchSegmentWithRetry` retries it like a 5xx |
| 2026-10-31 | `8a9c4523` | yt-dlp `814bba393` (#5274, fragment 0 fatal), `8c1f07d81` (#16771, `_live_adaptive_fragments`) | A DASH download's fragment 0 is fatal and the live generator never asks past `X-Head-Seqnum`: the first-segment hunt steps only below a known head and never off sq=0 of a manifest-free start (`huntMayAdvance`, `InitInlineAtStart`), an empty file never takes media above sq=0 before its init (`ensureInlineInit`), and the init fetch retries (`fetchInitSegment`, a credential refresh on a 403/410) and is fatal (`ErrInitSegmentUnavailable`) |
| 2026-10-31 | `8e716b4d` | yt-dlp `_extract_player_url` / `_extract_player_info` at `b77c9304e` (`_video.py:2149-2155`, `2194-2198`) | The watch page's player URL is the first match naming a `/s/player/<id>/` player (`watchPagePlayerURL`), as on every fallback page; a leading non-player script is never run as a player nor remembered |
| 2026-10-31 | `341e1496` | yt-dlp `d48e61260` (#12339) | The page retries confined to a window, the sooner of one attempt's timeout and a third of the caller's remaining deadline (`pageRetryShare`), so a failing watch page or iframe API read cannot eat the player calls' budget on the 403 refresh; waits and the timeout cap are Moombox's (upstream retries at once, and retries a timeout) |
| 2026-10-31 | `ff0e8bde` | yt-dlp `_initial_extract` reused by the live-from-start refresh at `b77c9304e` (`_video.py:3911`) | The 403 refresh's URL half and the DASH/HLS `OnCipherFailure` hooks are gated on a solver, not the setup extraction's player URL; the refresh resolves with the fresh response's player |
| 2026-10-31 | `3515754f` | yt-dlp `b6de707d1` (#860), `b7b7910d9` (#14430), `04d6974f5` (#16269) | Player URL fallbacks before STS (`resolvePlayerURL`): a page's within six hours (homepage, watch, embed), `/iframe_api` (`_download_player_url`), the last good one; every URL on the main variant (`_construct_player_url` from `b7b7910d9`, whose `main` default `1a895c18a` turned to `tv` and `04d6974f5` restored; the `en_US` locale beyond upstream); ytcfg `STS` preferred and no STS without a player (`extractSTS`) |
| 2026-10-31 | `de9b1e6b` | yt-dlp `d48e61260` (#12339) | The watch page is fetched through `fetchYouTubePage` (`_download_webpage_with_retries`: three retries, none on 403, 429 or the consent wall, deadline-bounded); a page that still fails leaves the extraction on the cached visitor data and — beyond upstream — the signed-in session (`fallbackYtcfg`) |
| 2026-10-31 | `fcc636ac` | yt-dlp `b7b7910d9` (#14430), `04d6974f5` (#16269), `07f04005e` (#12767) | Every player JS fetch asks for the player's main variant (`CanonicalPlayerURL` in `PlayerCache.Fetch`), so the id-keyed disk entry holds one body and the sidecar is never served another variant's code |
| 2026-10-31 | `e199f6c3` | yt-dlp `_extract_formats` at `b77c9304e` (`_video.py:3556-3579`) | A format URL with no encrypted signature and no `n` resolves unchanged with no player URL and no solver (`needsNoPlayer`, in `RoutedResolveURL` and `GojaResolver.ResolveURL`), where it failed with "player URL is required" |
| 2026-10-31 | `6915d216` | yt-dlp `0ad92dfb1` (#2823) | The dedup's per-itag collapse keeps an intact copy ahead of a damaged one before the track (`collapseToPreferredRendition`), as upstream keeps every track and ranks `ie_pref` ahead of `lang` (`FormatSorter.default`), so an intact dub is archived when every original-language copy is short |
| 2026-10-31 | `d48e51ed` | yt-dlp `cca80fe61` | `hq720` tried after `maxresdefault` in the finalize's thumbnail sizes (`copyAssets`); `mqdefault` stays ahead of the letterboxed 4:3 sizes, where upstream ranks those first |
| 2026-10-31 | `ee699d24` | yt-dlp `97afb093d` | A DASH `frameRate` or HLS `FRAME-RATE` of 1 or less is read as unstated (`knownFPS`) |
| 2026-10-31 | `2d2c3cba` | yt-dlp `97afb093d` | A player response's fps of 1 or less is stored as unstated (`parseFormats`), so it no longer wins the FPS rung under `prefer_60fps` false |
| 2026-10-31 | `0e8a04f8` | yt-dlp `0ad92dfb1` (#2823), `a1b2d8436` (#3083), `a25a42432` | A rendition whose `approxDurationMs` is under the video's length halved in whole seconds is damaged (`markDamagedFormats`); the selector passes over it while an intact one of its kind exists, ahead of the cap, as preference -10 ranks through `ie_pref` |
| 2026-10-31 | `bb456eb0` | yt-dlp `5c8bcfdbc` (#14124), `715af0c63` (#14646) | Who waits for `available_at` is upstream's own `live_status` (`is_live` / `post_live`, read off `isPostLiveDvr`, `isLive` and `isLiveNow` as `get_first` over the extraction's responses, the watch page's first), not `StreamStatus`, which calls a finished WEB-answered broadcast `post_live` (`PlaybackWaitExempt`) |
| 2026-10-31 | `33185cd7` | yt-dlp `715af0c63` (#14646) | The downloader half of `available_at`: every YouTube strategy waits for its URLs' player response before its first media request (`waitForPlayback`), a credential refresh for its fresh response (`waitForFreshPlayback`); live and post-live never wait |
| 2026-10-31 | `1126da36` | yt-dlp `715af0c63` (#14646), `3c279b33c` (#17062) | `available_at` read off each player response's pre-roll ads (adPlacements START, adSlots BEFORE_CONTENT) and stamped on its formats and manifests (`playbackAvailableAt`) |
| 2026-10-22 | `15fe37f4` | yt-dlp `12ac9aca7` (#17703) | ytInitialData read from `<script id="yt-initial-data" type="application/json">` (staged rollout, cookie-authed pages): third anchor in both locator sets |

## Reviewed, no action (most recent review)

Replace this section on each review. It is the reason the SHAs above moved past these commits.

- yt-dlp `7f7106cd3`: curl_cffi 0.17.x support. Moombox has no curl_cffi request handler.
- yt-dlp `7e3644cfa`, `f264a6cbd`: misc cleanup / release 2026.10.26; the only youtube-path change is the vendored-ejs `_info.py` metadata.
- yt-dlp `fa9886678`: Odnoklassniki extractor. Not supported by Moombox.
- yt-dlp build, CI and test commits (`34ea29166`, `c91f8de0b`, `755a01732`, `fd26888eb`, `7747073a2`, `15262b0b9`, `9b58cb33d`).
- ejs `d48c630`: GitHub Actions bump only. BgUtils `f842f8a`: brace-expansion dev-dep bump only.
- chatterino7: Qt/UI, CI and Helix refactors, no IRC/emote protocol change Moombox consumes. bgutil-ytdlp-pot-provider, moonarchive, moombox (Python): nothing new.
