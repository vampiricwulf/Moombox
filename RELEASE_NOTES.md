### Improvements

- **Go 1.26 upgrade** — bumped go.mod from 1.25.8 to 1.26, enabling Green Tea GC (10-40% less GC overhead), 2x faster `io.ReadAll`, and Go 1.26 language features
- **`go fix` modernization** — applied Go 1.26 modernizers across 33 files: range-over-int loops, `strings.SplitSeq` iterators, `any` type alias, built-in `min`, `strings.Builder` optimizations

### Dependencies

- **goja** `20260219` → `20260311` — fixes panic in try/finally with closures, broken `delete` with optional chaining, generator.return() with finally+yield
- **regexp2** `v1.11.4` → `v1.11.5` — fixes spurious timeout bug in regex matching (goja's regex engine)
- **go-runewidth** `v0.0.20` → `v0.0.21` — fixes Variation Selector characters measured as non-zero width (TUI alignment)
- **colorprofile** `v0.4.2` → `v0.4.3` — fixes io.Writer byte count contract violation
- **ultraviolet** `20260205` → `20260316` — synchronized output (less TUI flicker), Cassowary layout solver, buffer optimizations
- **sqlite** `v1.46.1` → `v1.47.0` — SQLite 3.51.3 engine bump
- **x/crypto** `v0.48.0` → `v0.49.0`, **x/sync** `v0.19.0` → `v0.20.0`, **x/text** `v0.34.0` → `v0.35.0` — maintenance updates
