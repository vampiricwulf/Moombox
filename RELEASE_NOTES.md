### Bug Fixes

- **Twitch auto-recovery: closed a race that could delay recovery up to 60s.** Between `setJobError` writing `Error` to the DB and the deferred `queue.Complete` freeing the slot, a monitor-tick auto-recovery firing inside the window would call `AutoReinitializeJob` → `EnqueueJob`, which short-circuited because `IsProcessing(jobID)` still returned true. The recovery would land in the DB (counter incremented, status=Upcoming) but never get picked up until the 60s `pollForJobs` heartbeat. Fix: `setJobError` and `handleCancellation` each call `queue.Complete(job.ID)` at the top so the slot frees before any DB write commits the terminal state. The deferred `Complete` in `processJob` remains as a panic-safety net (idempotent).

### Improvements

- **Stats endpoint surfaces Twitch hint-cache hit/miss counters.** `/api/stats` now includes a `twitchHints` section (`{hits, misses}`) so operators can see whether the monitor → processor pass-through is firing in production. Without it, a future refactor that silently breaks the stash path would only show up as "no flap errors anymore" — easy to misinterpret as "no flaps occurring."

### Internal

- Schema migration spec table backfilled with v12 (`idx_history_added_at`) and v13 (`auto_retry_count`); `appendix-metrics.md` updated from stale schema v11 / app v2.4.8 to v13 / 2.6.10.
- New `TestTwitchHintCacheStatsCounter` verifies counter increments across stash, take-miss, and expiry-miss paths.
