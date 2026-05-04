### Bug Fixes

- **Twitch: false "channel is offline" errors immediately after stream-found are now prevented.** A real-world incident (channel `shachimu`, 2026-05-03 22:05:56) showed the monitor confirming a stream live with a streamID, then `processTwitchLive` re-querying GQL <1s later and getting `Stream==nil`, failing the job until manual retry. The monitor's freshly-fetched `*twitch.TwitchStreamInfo` is now stashed in a take-once hint cache; the processor consumes the hint instead of re-querying — eliminating the redundant GQL call that exposed us to transient `StreamMetadata` flaps.

### Improvements

- **Twitch: errored jobs auto-recover when the same broadcast is still live.** When `processTwitchLive` reports `"twitch channel is offline"` and the next monitor tick (within ~15s) confirms the same `streamID` is still live, the job is automatically re-enqueued with a fresh hint via the new `AutoReinitializeJob` path. Capped at 2 auto-retries so persistent issues don't loop forever; the cap resets on user-driven Reinit/Resume. Retry-failure notifications are suppressed to prevent duplicate alerts on the same job.

### Internal

- New `auto_retry_count` column on `jobs` (schema v13) tracks monitor-driven recovery attempts.
- New take-once hint cache (`internal/worker/twitch_hint.go`) with mutex-protected stash/take and 60s TTL leak guard.
- New recoverable-error predicate (`internal/monitor/twitch_recover.go`) gates auto-recovery on a narrow shape: status==Error, exact `worker.TwitchOfflineErrMsg` match, no segments downloaded yet, retry budget remaining.
- New `OnStreamRecover` callback hook on `TwitchMonitor` — sibling of `OnStreamFound`, dispatched from `checkChannel`'s `HasProcessed` short-circuit.
- `worker.TwitchOfflineErrMsg` and `worker.MaxTwitchAutoRetries` exported so producer + consumer share a single source of truth.
- 14 new unit tests covering hint cache lifecycle, predicate gates, counter increment/reset, and column plumbing.
