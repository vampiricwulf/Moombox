# Updates, BotGuard and Twitch

This page covers three short sections:

- `[updates]` controls the automatic check for new Moombox releases. It also
  holds three values Moombox writes for its own bookkeeping.
- `[bgutils]` switches the BotGuard sidecar on or off. The sidecar is an
  embedded Node.js process that mints YouTube PO tokens and solves YouTube's
  signature cipher.
- `[twitch]` lets you replace the Client-ID that Moombox sends to Twitch.

```toml
[updates]
auto_check_updates = true
# Written by Moombox — do not edit:
# last_run_version = "x.y.z"
# last_failure_marker_seen = "..."
# skipped_version = "vx.y.z"

[bgutils]
use_sidecar = true

[twitch]
client_id = ""
```

!!! info "When changes take effect"
    `auto_check_updates` applies without a restart. `use_sidecar` and
    `client_id` are read once at startup: both UIs mark them **Requires
    restart**. You can restart from **Settings → System → Restart Moombox** in
    the dashboard.

    If you edit `config.toml` by hand, Moombox reads it at the next start.
    Stop Moombox first: an edit made while it runs is not picked up, and the
    next save from either UI writes the running settings back over it.

If an older `config.toml` has no `[bgutils]` section, Moombox writes it with
its default on the first start after the upgrade.

## Updates

Moombox checks GitHub for new releases of itself. When it finds one, the
dashboard shows an update badge, the TUI shows the pending version (read its
notes with ++r++ ++n++, then ++u++ to apply or ++s++ to skip), and
notification targets get an **Update Available** message (event
`update_available`) once per new version. Applying an update, rolling back and
verifying the binary's signature are described in
[Updating](../getting-started/updating.md).

### `updates.auto_check_updates`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` (`false` in the Docker image's seeded config) | `true` / `false` | Immediately | Dashboard: Settings → Updates → **Automatically check for updates**. TUI: Settings → Updates → **Auto-check updates**. |

When on, Moombox checks for a new release a few seconds after it starts and
every 24 hours after that. Turning it on while Moombox runs triggers a check
within a minute; turning it off stops the schedule at once.

When off, Moombox never checks by itself. The dashboard's **Check Now** button
(Settings → Updates) and **View Release Notes** still work, and a manual check
can still find and offer an update.

Leave it on for a 24/7 recorder: YouTube and Twitch change often, and
extraction fixes arrive as Moombox releases.

!!! note "Docker"
    The Docker image's seeded `config.toml` sets this to `false`. An in-app
    update replaces the binary inside the container, and that change is lost
    whenever the container is recreated from its image. Update by pulling a
    new image instead (`docker compose pull && docker compose up -d`). You can
    set it to `true` if you still want new-release notifications. See
    [Docker](../getting-started/docker.md).

```toml
[updates]
auto_check_updates = false
```

### `updates.last_run_version`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (absent) | Written by Moombox | — | Not shown in either UI. **Do not edit.** |

Internal bookkeeping. Moombox records the version it ran last. When a start
finds a different version (after a self-update or after you replaced the
binary by hand), it sends an **Update Applied** notification (event
`update_applied`) naming the previous and current versions and whether the
web dashboard came back up. It then stores the new version.

Nothing is announced when the value is empty (a first run, or a config from a
version that predates this key), or when the new version is the result of an
automatic rollback after a failed update. The value is only written once a
`config.toml` exists, so the first-run setup wizard is never skipped because
of it.

Editing it by hand would only cause, or suppress, one **Update Applied**
notification.

### `updates.last_failure_marker_seen`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (absent) | Written by Moombox | — | Not shown in either UI. **Do not edit.** |

Internal bookkeeping. When a self-update fails badly, the updater leaves a
marker file next to the Moombox binary. At every start Moombox looks for those
markers and sends a **Previous Update Failed** notification (event
`update_failed`) with the marker's path and contents. This key remembers
which markers (by file name and modification time) were already announced, so
that ordinary restarts, such as a restart after a settings change, do not
announce the same failure again.

Each marker is announced once. The notification asks you to check that the
running binary works and then delete the marker file; do that rather than
editing this key. Clearing this key would only make Moombox announce the
markers that are still there once more.

### `updates.skipped_version`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (absent) | A release tag such as `v2.9.0`, written by Moombox | Next automatic check | Set by **Skip this version** in the dashboard's update dialog, or ++s++ in the TUI's release notes. **Do not edit by hand.** |

The release you chose to skip. Automatic checks stay silent about that exact
release: no badge, no TUI notice, no **Update Available** notification. The
next release with a different tag is offered normally. Skipping does not turn
off automatic checks.

Moombox also sets this key itself: when an update fails to start and the
launcher rolls back to the previous binary, the release that failed is marked
skipped so automatic checks stop offering it.

A manual check (**Check Now** in the dashboard) ignores the skip and offers the
release again, which is how you retry a skipped or rolled-back release. Only
the most recent skip is remembered.

## BotGuard

### `bgutils.use_sidecar`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `true` | `true` / `false` | Restart | Dashboard: Settings → BotGuard → **Enable sidecar** (Requires restart). TUI: Settings → BotGuard Sidecar → **Enable sidecar** (requires restart). |

Runs the BotGuard sidecar: a Node.js runtime with JSDOM, `bgutils-js` and
yt-dlp's `ejs` solver, all embedded in the Moombox binary (no Node.js install
is needed). On first start it is unpacked to a cache folder. The sidecar does
two jobs for YouTube:

- It mints **PO tokens** that Google's BotGuard challenge accepts. YouTube
  requires them for many formats and clients, and the
  [yt-dlp plugin](../guide/ytdlp-plugin.md) gets its tokens from the same
  place.
- It solves the **signature cipher** of current YouTube players, which some
  formats need before they can be downloaded.

When it is off, only an in-process fallback remains. BotGuard's timing check
rejects that fallback, so **no PO tokens are minted** and **current players'
signatures cannot be solved**. Formats that need a PO token or a signature
become unavailable; formats that need neither keep working (the fallback still
handles the separate `n` parameter). In practice expect fewer formats, lower
quality, or failed YouTube downloads. Twitch is not affected.

Turn it off only to diagnose a problem with the sidecar itself, or on a host
where it cannot run. It is supported on Windows x64, Linux x64 and Linux arm64,
and it runs in the Docker image.

While the sidecar is on, Moombox restarts it automatically if it crashes or
runs out of memory (see
[`memory.sidecar_hard_limit_mb`](disk-memory-connectivity.md#memorysidecar_hard_limit_mb)).
The TUI status bar shows **SIDECAR DOWN** while it is down. If it stays down
for 60 seconds, you get a **BotGuard Sidecar Down** notification (event
`sidecar_down`); **BotGuard Sidecar Restored** follows when it is back. While the
sidecar is off no sidecar alerts are sent, and a sidecar-down alert left open
by an earlier run is dropped without a Restored message.

!!! tip "Proxies"
    The sidecar uses the same outbound HTTPS proxy as the rest of Moombox
    (from `HTTPS_PROXY` / `NO_PROXY`), because YouTube refuses a token minted
    from a different address. A `socks5://` proxy cannot be used by the
    sidecar; it then connects directly and the log says so. Behind a
    TLS-intercepting proxy, give the sidecar the proxy's CA certificate with
    `NODE_EXTRA_CA_CERTS`. Set these before starting Moombox.

```toml
[bgutils]
use_sidecar = true
```

## Twitch

### `twitch.client_id`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| Empty (the built-in Client-ID) | Empty, or 1–64 letters and digits | Restart | Dashboard: Settings → Twitch → **Client-ID override** (Requires restart). TUI: Settings → Twitch → **Client-ID override** (requires restart). |

Every request Moombox makes to Twitch's GraphQL API and every playlist fetch
sends a Client-ID. Moombox has one built in. When this key is empty (the
default) that built-in one is used; when it is set, this value replaces it on
every Twitch request, including `moombox add` on the command line.

The Client-ID is the first thing Twitch changes when it tightens access, and
yt-dlp exposes the same override (its `twitch:client_id` extractor argument)
for the same reason. Leave it empty unless Twitch has rotated the built-in one
out (Twitch monitoring and downloads start failing across all channels at
once) and a newer working web Client-ID is known, for example from a yt-dlp
fix. Once a Moombox release ships the new built-in value, clear the override
again.

Validation: both UIs trim surrounding spaces and refuse anything other than
1–64 ASCII letters and digits. In a hand-edited file an invalid value is
replaced with empty (the built-in Client-ID) and a warning is logged.

```toml
[twitch]
client_id = "abcdefghijklmnopqrstuvwxyz0123"
```
