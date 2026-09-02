# Arc 11 — Docker re-auth ingest: design

**Status:** settled by the owner on 2026-08-29 (ruling Q8: "This release, after Arc 10") on top of the
controller rulings in `.superpowers/sdd/2026-08-25-cookie-subsystem-remediation/docker-ingest-brief.md`
(2026-08-27, gitignored — its rulings are restated here so this tracked spec stands alone).
**Scope:** `internal/web/routes`, `internal/cookies` (one setter + its caller), `web/public` (settings
panel), `README.md`, `docker/entrypoint.sh` guidance, the four spec docs that describe re-auth.

## 1. The problem

A Docker user whose YouTube or Twitch session dies has no browser in the container and no way to put
fresh cookies in place except shell or filesystem access to the `./data` volume. Everything downstream
of "the bytes land in `cookies.txt`" already exists: `CookieJar.Reload`, the periodic and on-demand
`RefreshService.CheckNow`, the browser-free 30-minute YouTube rotation, and — since Arc 10 — the
fingerprint comparison that clears the Twitch mark and tells every live chat session to reconnect.
`operations.md` §Docker Image currently says the re-auth ingest path is UNBUILT; this arc builds it.

## 2. Required behaviour

**R1 — One endpoint, `POST /api/cookies/import`.** Accepts a Netscape-format cookie file as a pasted
body and/or a multipart upload, size-capped. Behind session auth and the CSRF Origin/Referer middleware,
on the `heavy` rate limiter. Allowed from any AUTHENTICATED client (not loopback-gated: the wizard's
loopback gate protects an unclaimed instance; this endpoint exists only behind a claimed one).

**R2 — Validate before touching disk.** Reject with a precise, value-free message when the body is not
Netscape format, has no parseable rows, or carries no recognised auth-cookie NAME for either platform
(the logged-out-export case). Cookie names may appear in diagnostics; values never.

**R3 — Merge, never replace.** Read the existing file through the `readCookieFile` seam (distinguish
"does not exist" from every other read error; abort on the latter with `ErrCookieFileUnreadable`);
merge through `mergeCookieFiles` (name+domain keyed; a YouTube-only paste leaves Twitch rows intact and
vice versa); write through `writeFileAtomic`; never write an empty-valued row; then `jar.Reload()`.

**R4 — The write ends in a re-check, like every other credential write.** After the reload the handler
runs `RefreshService.CheckNow` (the `recheckAfterCookieWrite` shape from Arc 10, detached context with
a timeout, flushed response) so the fingerprint comparison runs: a changed Twitch pair clears the mark
and reconnects live chat; the YouTube identity change drives the existing membership-park sweep. This
is row 16 of the reload-site table. The response carries the existing three-state verdict shape that
`/api/cookies/auto-refresh` returns, so a bad export is reported at paste time.

**R5 — No GET, ever.** The endpoint accepts credential bytes and never serves them; no "download
current cookies" affordance anywhere near the upload control.

**R6 — `FlagManualRelogin` returns with its caller.** `AutoCookieService.FlagManualRelogin` (deleted in
Arc 8 for having no callers) is re-added in the same commit as its call site: the re-login prompt names
the import as the remedy in a container (and on any install where the browser path is off), and a
successful import clears the relogin state.

**R7 — UI.** A paste textarea + file picker in the cookies settings panel, showing the returned verdict
inline; the re-login prompt links to it. TUI parity is optional — if skipped, say so in the docs.

**R8 — Docs.** `README.md` and the container guidance state plainly that dropping a fresh
`cookies.txt` on the volume already works and that the dashboard import is the browser-free path;
`operations.md` §Docker Image's "UNBUILT" sentence flips; `user-interfaces.md`'s Cookies endpoint table
gains the route; `data-and-storage.md`'s writer list names the fifth writer; `SPEC.md` § Cookies states
the import in one sentence.

## 3. Non-goals

No renewal logic (the 30-minute rotation already covers P2). No change to the wizard's loopback gate.
No browser launch. No new `CookieStatus` member. No change to the liveness pilot. No download of any
kind.

## 4. Invariants that must survive

Arc 2's writer catalogue (read `data-and-storage.md` §Cookies "the writers"): atomic write, merge
semantics, the empty-row trap, name+domain keying, the unreadable-file error and what renders it (never
"replace the file" advice for a file the endpoint just wrote). The single-file bind-mount trap: the
rename cannot replace a bind-mounted FILE — the endpoint fails with the same Docker hint `refresh.go`
already logs. Never a cookie value in a response, log, error, test name, or fixture (fixtures use
obviously fake values; the live gate takes a PATH). Every goroutine carries the inline recover; the
logger interface stays anonymous.

## 5. Tests

A valid YouTube-only paste leaves Twitch rows intact (and the reverse); a logged-out export is rejected
with the specific message; a malformed body is rejected before anything is written; a body over the cap
is rejected; the verdict returned equals what the verify produced; the write is followed by exactly one
`CheckNow` (the Arc 10 gate-shape test extends to this site); the merge never writes an empty-valued
row; `FlagManualRelogin` is set by the prompt path and cleared by a successful import; no response or
log line contains a value (a recording logger + the response body scanned for the fixture's fake
values). Standing rule: every assertion names the mutation that breaks it.
