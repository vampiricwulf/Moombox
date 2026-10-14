## Improvements

- **A stream the feed-history backfill finds gets its job at once.** The backfill scan writes its rows after the monitor cycle that queued it has already walked the channel, so a stream only the backfill could see — YouTube's RSS feeds answering 404 for every channel, as they did on October 13, with DECAPI showing a newer video — waited a whole `feed_check_interval` for its job. A completed scan that added rows now re-runs the feed check immediately; several scans finishing in a row cost one extra cycle.

## Bug Fixes

- **An age-restricted stream that had already ended no longer re-announces itself every cycle.** With no cookies the probe sees such a stream with no formats, and it was read as upcoming: the monitor kept it in scope and logged "archive: creating job" and "Video found" every five minutes — for weeks, for a stream archived long before. It now reads as a finished broadcast and leaves the monitor's scope on the next cycle.
- **Go 1.27.2 and `golang.org/x/net` 0.60.0.** Ten standard-library findings the scheduled vulnerability scan reported, all in `net/http`, `crypto/tls` and `net/textproto` — HTTP/1 CONNECT desynchronisation on the client and the server, the Range header size, the MIME header memory limit, the ECH outer-extension check, and five HTTP/2 server findings. Relevant to anyone exposing the dashboard beyond their LAN.

## Internal

- `go fix` under Go 1.27.2: pointer helpers inlined as `new(v)`, `wg.Go`, `t.Context()`, `atomic.Int32`, `errors.AsType`, `strings.SplitSeq`, `slices.Backward`. No behaviour change.
- staticcheck pinned to 2026.2.2, which reads the export data Go 1.27.2 writes.
