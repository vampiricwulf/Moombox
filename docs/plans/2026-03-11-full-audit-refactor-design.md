# Full Code Audit & Refactor — Design

**Date:** 2026-03-11
**Scope:** Fresh deep audit of all 59K lines (161 Go files) + 245KB frontend, with structural refactoring at natural seams.

## Goals

1. Re-audit every file with fresh eyes — previous audits (TUI 71 fixes, Web UI multiple rounds, Backend 6 phases) may have missed issues
2. Refactor large files at natural domain boundaries
3. Expand test coverage inline with each phase
4. Maintain all existing patterns defined in CLAUDE.md

## Audit Checklist (per file)

9 categories applied to every file:

1. **Security** — injection, auth bypass, token leaks, path traversal, unvalidated input, TOCTOU
2. **Bugs** — logic errors, race conditions, resource leaks, nil derefs, off-by-ones, overflow
3. **Error handling** — swallowed errors, missing cleanup on error paths, panic without recovery, inconsistent wrapping
4. **Dead code** — unused functions/methods/fields/constants, unreachable branches, stale comments
5. **Deduplication** — repeated patterns that should be extracted into shared helpers
6. **Anti-patterns** — goroutine leaks, mutex misuse, context.Background() misuse, unbounded growth
7. **Consistency** — naming, error message style, log levels, parameter ordering, comment style
8. **Tests** — add tests for bugs found, strengthen weak tests, verify edge cases
9. **Structural** — natural seams for file splitting, overly complex functions, readability improvements

## Refactoring Rules

- Split files only at clear domain boundaries, not arbitrary line limits
- Extract helpers only when there's genuine reuse or a function does multiple distinct things
- Don't break CLAUDE.md patterns (anonymous logger interface, UpdateJobFields, chord system, etc.)
- Each refactoring must maintain all existing tests passing
- Prefer moving code to new files within the same package over creating new packages

## Phase Structure (bottom-up dependency order)

| Phase | Packages | ~Lines | Description |
|-------|----------|--------|-------------|
| 1 | goja, errors, constants, disk, logger | 3.1K | Foundation leaf packages |
| 2 | database, config, cookies | 6.2K | Data & config layer |
| 3 | cipher, bgutils, youtube | 6.5K | YouTube platform services |
| 4 | twitch, chat | 5.3K | Twitch platform services |
| 5 | engine, monitor | 4.6K | Download engine & stream monitoring |
| 6 | worker (16 files) | 7.5K | Download pipeline — biggest refactor target |
| 7 | web (server + routes), notifications, updater | 8.5K | Web server & supporting services |
| 8 | tui (22 files) | 13K | TUI — largest package |
| 9 | cmd/moombox/main.go, utils | 4.1K | Entry point & cross-cutting utilities |
| 10 | Web frontend (HTML/CSS/JS) | ~245KB | Shoelace UI, modules, styles |

**Total:** ~59K Go lines + ~245KB frontend across 10 phases.

## Key Refactoring Targets

- **internal/web/routes/jobs.go** (2,525 lines) — split route handlers by resource type
- **internal/tui/app.go** (2,486 lines) — extract TUI subsystems (chord dispatch, state management)
- **internal/worker/orchestrator.go** (2,055 lines) — extract orchestration phases
- **internal/tui/settings.go** (2,143 lines) — extract form builders and validation
- **cmd/moombox/main.go** (2,119 lines) — extract service initialization blocks

## Commit Strategy

One commit per phase:
```
fix: audit & refactor phase N (group) — X fixes, Y refactors across Z files

Critical: ... Important: ... Minor: ... Quality: ... Structural: ... Tests: ...
```

## Success Criteria

- `go build ./...` passes after every phase
- `go test ./...` passes after every phase
- `go vet ./...` clean after every phase
- No file over ~800 lines without clear justification
- All bugs found have corresponding tests
- No regressions in existing functionality
