### Bug Fixes

- **YouTube watch-page memory leak.** Quality-monitor polling was retaining ~5 MB of HTML per probe (~5 MB/min sustained, ~30 MB per 10-min window in pprof heap diff — 89% of all heap growth attributable to `FetchWatchPage`). Two compounding causes: substring aliasing from regex submatches in `YtcfgData` (`VisitorData`, `DelegatedSessionID`, `DataSyncID`) pinned the underlying ~5 MB HTML byte array whenever a downstream cache held them; and `WatchPageResult.HTML` kept the full body as a struct field even though only chat-continuation extraction (one-shot per stream) ever needed it. Fix: `strings.Clone` the regex submatches, move chat-continuation extraction into the watch-page parse step, drop the `HTML` field, delete dead `Service.FetchWatchPageHtml`. Verified with pprof — `FetchWatchPage` no longer appears in the inuse_space top 30; total 10-min heap delta dropped from 32.96 MB to 5.21 MB.

### Internal

- **`MOOMBOX_PPROF=1` opt-in pprof endpoint.** When set, the child process binds the standard `net/http/pprof` handlers on `localhost:6060` (loopback-only, no auth, zero overhead when unset). Used to find the watch-page leak above. Documented in CLAUDE.md and README.md.
