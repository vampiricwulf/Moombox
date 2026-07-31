## Features

- **Dated feed history with windowed archiving.** Moombox now keeps a dated, per-channel history of every video it has seen (new schema v16: `feed_items` + `channel_state`). On the first scan of each YouTube channel it backfills the channel's full catalog page by page, then archives everything published inside your archive window; entries that leave the window are pruned before they ever start downloading. Twitch VOD discovery honors the same window. New settings `archive_window_days` and `archive_slots` (global, with per-channel overrides) replace `max_feed_items` — old configs still load, the obsolete key is simply ignored.
- **Backlog scheduler and the new `Queued` status.** Backlog VODs enter as `Queued` and are admitted to download by a per-channel archive-slots scheduler, so a channel's back catalog downloads at a controlled pace. Live streams, premieres, and newly published videos never wait in the queue, and a queued job that turns out to be a broadcast releases its slot immediately. Queued jobs can be cancelled just like Upcoming ones.
- **Live broadcasts are never throttled.** The parallel-download pool now gates VOD downloads only — a live recording always starts immediately. The `num_parallel_downloads` default rises from 2 to 10.
- **Backfill visibility and manual re-scan.** Backfill progress appears in both the web dashboard and the TUI while a channel's catalog is being established, and the new `R B` chord (backed by a debounced API route) forces a full-catalog re-scan of every configured YouTube channel.
- **Members-only escalation.** A probe that hits a members-only refusal now escalates to the authenticated probe (for channels with membership discovery enabled), so members-only streams surface from the regular monitor cycle as well.

## Improvements

- **Outage alerts reworked.** Going offline no longer attempts to send a "Connectivity Lost" webhook — a lost connection has nothing to ride on. Instead, the restore sends a single warning-orange "Outage Alert" carrying the outage's start and end as viewer-local Discord timestamps plus its duration. The `connectivity_lost` event is retired from the notification vocabulary; existing `connectivity_restored` target allowlists keep working unchanged.
- **The monitor cycle is rebuilt store-driven.** Each channel now walks its sources serially with per-source early exit, chooses the probe type by source, and normalizes publish dates (UTC `Z` suffix, day-precision dates pinned to end of day) so window decisions are consistent everywhere.

## Internal

- The feed-history system was designed up front: a first-principles specification hardened through dozens of adversarial review rounds, five implementation plans, and a full v16 documentation sweep across `SPEC.md` and `docs/spec/`.
- The legacy `last_videos` store and its API endpoint are removed — the dated feed history replaces them.
- YouTube gains a `/browse` continuation client (tab params harvested from the channel's own strip, `UU` playlist fallback on tab-identity mismatch) that powers the backfill scanner.
- Connectivity's `ReportSuccess` collapsed to a single passive lock.
- New regression tests across the monitor, database, worker, and YouTube packages: probe-date phasing, the v16 migration fall-through, Z-suffix date invariants, backfill window stops, orphaned-history re-jobbing, and the backlog scheduler's admission rules.
