## Bug Fixes

- **Memory-limit defaults retuned from observed field data.** v2.6.20 shipped with `go_soft_limit_mb = 100`, `sidecar_soft_limit_mb = 100`, `sidecar_hard_limit_mb = 256`. Those values sat *below* normal operating ranges:
  - Moombox Sys is ~60 MB idle but ~230 MB during active downloads (p99 = 238 MB), so the 100 MB Go soft cap forced constant GC pressure during every download.
  - Sidecar RSS is ~47 MB idle, ~150 MB post-mint (p75), 400-500 MB during BotGuard bursts. The 100 MB sidecar soft cap fired its GC trigger on every 2-minute check during normal operation; the 256 MB hard cap would have OOM-aborted the sidecar mid-mint.
  - New defaults: `go_soft_limit_mb = 256`, `sidecar_soft_limit_mb = 200`, `sidecar_hard_limit_mb = 512`. Each sits above its respective working set so GC only fires on actual growth, not routine streaming work.

## Documentation

- New `docs/spec/operations.md` "Memory Limits" section with the full design rationale (soft vs. hard, Go vs. V8) and tuning guide.
- `docs/spec/data-and-storage.md`: `[memory]` row added to the config-section table.
- `README.md`: `memory.*` rows in the Key Settings table.
- `config.example.toml`: commented-out `[memory]` block with field-by-field comments.
