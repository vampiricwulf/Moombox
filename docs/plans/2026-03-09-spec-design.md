# SPEC.md Design Document

**Date:** 2026-03-09
**Purpose:** Create a comprehensive SPEC.md and supporting deep-dive docs, then reduce CLAUDE.md to control prompts only.

## Motivation

CLAUDE.md currently serves dual duty as both AI control prompts and project specification. This causes:
- Stale architectural details polluting every conversation's context
- No separation between "how to behave" and "what the project is"
- AI starts with potentially outdated information rather than a clean mindset

Solution: Split into SPEC.md (thorough reference, consulted on demand) and CLAUDE.md (lean control prompts, always in context).

## File Layout

```
SPEC.md                              <- Substantial standalone spec (~800-1500 lines)
docs/
  spec/
    vision-and-purpose.md            <- Why Moombox exists, who it's for
    design-philosophy.md             <- Priorities, constraints, code principles
    architecture.md                  <- Process model, packages, data flow, concurrency
    platform-services.md             <- YouTube, Twitch, BotGuard, Cipher deep-dive
    user-interfaces.md               <- Web UI, TUI, shared patterns, parity rules
    data-and-storage.md              <- Database, config, cookies, file output
    security.md                      <- Auth, CSRF, middleware, signing
    operations.md                    <- Build, CI, release, updates, launcher
    appendix-metrics.md              <- Volatile numbers (line counts, schema version, etc.)
```

## Design Decisions

### Audience: AI-first reference
Written primarily so Claude (or any LLM) can deeply understand the project when consulted. Optimized for machine comprehension -- explicit, unambiguous, no assumed context.

### Volatile metrics strategy
- SPEC.md contains no volatile metrics (line counts, schema versions, exact dep versions)
- Metrics live in `docs/spec/appendix-metrics.md`, clearly dated
- SPEC.md uses conceptual descriptions of scale/complexity instead

### SPEC.md depth (~800-1500 lines)
Each section is thorough enough that an AI could understand the project well without reading deep-dives. Deep-dive docs add implementation-level detail on top.

### Deep-dive doc format
Optimized for AI extraction:
- **Scope** (2-3 sentences) -- what this doc covers
- **Rules/Constraints** (bulleted) -- hard requirements an AI must follow
- **Body** (free-form, shaped by topic) -- clear H2/H3 headers
- **Cross-references** -- links to related docs and source files

### SPEC.md sections
1. **Vision & Purpose** -- Personal tool built to product quality. Archival appliance. Built for owner's workflow, usable by anyone wanting set-and-forget archiving.
2. **Design Philosophy** -- Priority ordering, code complexity principles, deployment simplicity.
3. **Architecture** -- Launcher/supervisor, init order, package graph, data flow, concurrency with parameters.
4. **Platform Services** -- Multi-client Innertube, Twitch GQL, BotGuard, cipher. Informed reimplementation tracking upstream.
5. **User Interfaces** -- Dual UI with platform strengths. Neither second-class.
6. **Data & Storage** -- Schema philosophy (not tables), config approach, output conventions.
7. **Security** -- Model and principles. Details in deep-dive.
8. **Operations** -- Launcher pattern, update mechanism. Details in deep-dive.

### Key design answers

- **Target user:** Owner first, then anyone wanting set-and-forget stream archiving. Technical enough to run a binary and configure TOML.
- **Priority ordering:** Correctness > Reliability > Resource efficiency > Simple deployment & UX > Polish > Feature completeness > Performance
- **Code complexity:** Acceptable when the solution demands it. Match solution complexity, not problem complexity. Contain behind clean interfaces.
- **UI parity:** Parity with platform strengths. TUI gets real-time + keyboard. Web UI gets rich media. Neither second-class.
- **Upstream relationship:** Informed reimplementation. Tracks upstream for awareness, adapts independently for Go/Moombox architecture.
- **Error philosophy:** Never crash, degrade gracefully, but always inform the user. Silent failures are as bad as crashes.
- **Schema documentation:** Design philosophy and patterns, not table definitions.
- **Historical TypeScript:** Skip entirely. Rewrite is complete.
- **Future roadmap:** Not included. Spec documents what exists.

### CLAUDE.md reduction
Keeps:
- Working style instructions
- Build/test commands
- Critical must-follow patterns (logger interface, partial updates, job status lifecycle, chord system)
- Pointer to SPEC.md and docs/spec/
- Release process
- Reference repo instructions

Drops: architecture, package graph, design philosophy, dependency table, security details, web UI file table, concurrency details.
