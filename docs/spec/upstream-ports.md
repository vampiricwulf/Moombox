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
| yt-dlp `a51ef56c0` (#17884) | Playability reason falls back to `errorScreen.playerInterstitialRenderer.content.interstitialViewModel` (`title.content` as the reason, `description.content` as the subreason) | `playabilityReason` in `internal/youtube/player_api_parsing.go` reads only `reason` and `messages[0]`. A refusal that arrives interstitial-only gets an empty reason, so `parsePlayabilityStatus`'s keyword match (member / private / country) falls through to the unknown verdict. | Low priority. Wants a real response capture for the fixture before porting. |
| yt-dlp `c7fb478d2` (issue #17666) | web_embedded can go SABR-only (itag 18 only); upstream gets HLS 91–96 back with a Safari UA | Swapping the UA on its own does nothing here: the port has three parts. | Owner deferred 2026-10-02. |

## Port log

Newest first. One row per Moombox commit that ports upstream work.

| Date | Moombox commit | Upstream | Summary |
|---|---|---|---|
| 2026-10-31 | `bb456eb0` | yt-dlp `5c8bcfdbc` (#14124), `715af0c63` (#14646) | Who waits for `available_at` is upstream's own `live_status` (`is_live` / `post_live`, read off `isPostLiveDvr`, `isLive` and `isLiveNow` as `get_first` over the extraction's responses, the watch page's first), not `StreamStatus`, which calls a finished WEB-answered broadcast `post_live` (`PlaybackWaitExempt`) |
| 2026-10-31 | `33185cd7` | yt-dlp `715af0c63` (#14646) | The downloader half of `available_at`: every YouTube strategy waits for its URLs' player response before its first media request (`waitForPlayback`), a credential refresh for its fresh response (`waitForFreshPlayback`); live and post-live never wait |
| 2026-10-31 | `1126da36` | yt-dlp `715af0c63` (#14646), `3c279b33c` (#17062) | `available_at` read off each player response's pre-roll ads (adPlacements START, adSlots BEFORE_CONTENT) and stamped on its formats and manifests (`playbackAvailableAt`) |
| 2026-10-22 | `15fe37f4` | yt-dlp `12ac9aca7` (#17703) | ytInitialData read from `<script id="yt-initial-data" type="application/json">` (staged rollout, cookie-authed pages): third anchor in both locator sets |
| 2026-10-31 | `8a9c4523` | yt-dlp `adbc4ec4b` (#888) | A DASH download's fragment 0 is fatal and the live generator never asks past `X-Head-Seqnum`: the first-segment hunt steps only below a known head and never off sq=0 of a manifest-free start (`huntMayAdvance`, `InitInlineAtStart`), an empty file never takes media above sq=0 before its init (`ensureInlineInit`), and the init fetch retries (`fetchInitSegment`, a credential refresh on a 403/410) and is fatal (`ErrInitSegmentUnavailable`) |
| 2026-10-31 | `e9bd795d` | yt-dlp `downloader/http.py` at `b77c9304e` ("Did not get any data blocks"); moonarchive `0da99f2` (`_dash.py`, `EmptyFragmentException`) | An empty 2xx segment body is a failed fetch, never a segment: the sequential DASH loop routes it through `handleHTTPError`'s at-edge path, and `fetchSegmentWithRetry` retries it like a 5xx |

## Reviewed, no action (most recent review)

Replace this section on each review. It is the reason the SHAs above moved past these commits.

- yt-dlp `7f7106cd3`: curl_cffi 0.17.x support. Moombox has no curl_cffi request handler.
- yt-dlp `7e3644cfa`, `f264a6cbd`: misc cleanup / release 2026.10.26; the only youtube-path change is the vendored-ejs `_info.py` metadata.
- yt-dlp `fa9886678`: Odnoklassniki extractor. Not supported by Moombox.
- yt-dlp build, CI and test commits (`34ea29166`, `c91f8de0b`, `755a01732`, `fd26888eb`, `7747073a2`, `15262b0b9`, `9b58cb33d`).
- ejs `d48c630`: GitHub Actions bump only. BgUtils `f842f8a`: brace-expansion dev-dep bump only.
- chatterino7: Qt/UI, CI and Helix refactors, no IRC/emote protocol change Moombox consumes. bgutil-ytdlp-pot-provider, moonarchive, moombox (Python): nothing new.
