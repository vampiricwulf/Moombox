## Bug Fixes

- **Orphaned History (`A O`): live Twitch recordings are no longer mislabelled as orphaned.** The orphaned-history list matched entries against the wrong job column, so every Twitch recording — whose history is keyed by its `tw_`-prefixed job ID, while the plain stream ID is stored separately — was reported as orphaned and offered for deletion even while it was actively recording. History is now matched by job ID for both YouTube and Twitch.
- **Orphaned overlay navigation.** Page Up / Page Down no longer jumps an extra page when the cursor steps past the files/history divider, and a pending delete confirmation now clears when you page away from the item.

## Improvements

- **The orphaned-history "Added" time now shows as relative time** (e.g. "2h ago"), matching the rest of the TUI and the web dashboard, instead of a raw UTC timestamp.
- Corrected the version indicator's hover hint (click to open the GitHub page).
- Hardened members-only discovery internally: panic-safe request cancellation and an escaped channel ID in the membership request.

## Internal

- Documented members-only `/membership` discovery, the orphaned-history API routes, and the `membership_discovery` / `probe_cooldown` config keys in `SPEC.md` and `docs/spec/`.
- Added regression tests for the Twitch orphaned-history fix, the overlay paging / divider-skip behaviour, and the `membership_discovery` config save/reload round-trip.
