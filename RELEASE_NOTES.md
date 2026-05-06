## Improvements

- The extracted Node sidecar binary is now named `moombox-sidecar.exe` / `moombox-sidecar` (was bare `node.exe` / `node`). It groups alphabetically with `moombox.exe` in Task Manager / `tasklist` / Process Explorer and no longer collides with unrelated Node processes (VS Code, npm dev servers, Electron apps) when investigating memory. The on-disk cache sweeps the old name on next extraction so upgrading users don't end up with both binaries.
- The 2-minute `[Memory]` log line now includes the sidecar Node process's resident memory and V8 heap when the sidecar is running:
  ```
  [Memory] Sys: 5019.8MB, Heap: 22.4 (+9.2)/4951.0MB, Stack: 5.0MB, GC: 82729 | Sidecar: RSS 215.3MB (V8 Heap 92.1/156.4MB)
  ```
  When the sidecar is disabled or down, the suffix is omitted (line shape matches the prior format). 1-second timeout on the sidecar memory call so a hung sidecar can't wedge the memory tick.

## Internal

- New `getMemoryStats` JSON-RPC method on the sidecar (returns `process.memoryUsage()` — rss, heapTotal, heapUsed, external, arrayBuffers).
- New `Sidecar.MemoryStats(ctx)` Go method wraps the JSON-RPC.
- `extractIfNeeded` runs `staleNodeBinaryNames()` cleanup after re-extract to remove orphan binaries from prior versions. Cache-hit path is unaffected (zero overhead on normal starts).
