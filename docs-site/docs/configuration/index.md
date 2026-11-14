# Configuration

Every Moombox setting lives in one file, `config.toml`. You rarely have to
open it: the setup wizard writes it on first run, and both the web dashboard
(**Settings** tab) and the terminal UI (the `` ` `` key) edit it for you.
This page explains how the file is found, read and saved, which changes need a
restart, and what Moombox does with values it cannot use. The pages listed
under [Settings by section](#settings-by-section) document every key.

## Where the file is

Moombox reads exactly one configuration file.

**Started with `-config <path>`:** that path is the only candidate. If the
file exists it is loaded. If it does not, Moombox starts on defaults, runs the
setup wizard, and the wizard creates the file at exactly that path. Moombox
never falls back to another file when the one you named is missing.

**Started without `-config`:** Moombox uses the first of these that exists:

1. `config.toml` in the current working directory;
2. `config/config.toml` under the current working directory;
3. `~/.config/moombox/config.toml` in your home folder (on Windows,
   `%USERPROFILE%\.config\moombox\config.toml`).

If none exists, the setup wizard runs and the file is created in the current
working directory. Every later save goes back to the file that was loaded, so
a configuration found in `config/` stays there.

The first lines of the log name the file in use:

```text
Configuration loaded path=/home/me/moombox/config.toml
```

or, when nothing was found:

```text
No configuration file found — running on defaults; a save will create one path=…
```

!!! warning "Relative paths follow the working directory"
    Relative paths inside the file (`./moombox.db`, `./output`, …) are
    resolved against the directory Moombox was **started from**, not the
    directory the configuration file is in. Double-clicking `Moombox.exe`
    starts it in its own folder, so on Windows everything ends up beside the
    executable. If you start Moombox from different places (a shortcut, a
    terminal, a service), use absolute paths or always start it from the same
    directory.

=== "Windows"

    Usually `config.toml` beside `Moombox.exe`, because that is the working
    directory when you double-click it.

=== "Linux"

    `config.toml` in the directory you start `moombox` from, or
    `~/.config/moombox/config.toml` if you prefer a fixed place. Under systemd,
    set `WorkingDirectory=` or pass `-config`.

=== "Docker"

    `/data/config.toml` inside the container (`./data/config.toml` on the host
    with the shipped compose file). The image's entrypoint passes
    `-config /data/config.toml` itself (set the `MOOMBOX_CONFIG` environment
    variable to use another path), and creates a minimal file on the first
    start. Beside it, the entrypoint keeps a `README-docker.txt` explaining the
    settings it seeded, because `config.toml` itself loses its comments at the
    first save. See [Docker](../getting-started/docker.md).

More detail, including how `moombox add` finds the same database, is on
[Command line](../reference/cli.md).

## Three ways to change settings

| Where | How | When it is written |
|---|---|---|
| Web dashboard | **Settings** tab. Pick a section on the left, edit, then **Save** (an *You have unsaved changes* banner offers **Save** and **Discard**). | On **Save**. |
| Terminal UI | Press `` ` `` (backtick) to open Settings. Move with the arrow keys or ++tab++, edit a row, and leave with **[ Save & Return ]**. Pressing ++esc++ with unsaved edits asks *Save changes? [Y]es / [N]o / [Esc] Cancel*. | On save. |
| By hand | Edit `config.toml` in a text editor **while Moombox is stopped**, then start it. | Read at the next start. |

The dashboard and the TUI show the same sections:

| Dashboard section | TUI section | Settings documented on |
|---|---|---|
| Network | Network | [Network](network.md); the connectivity probe targets on [Disk, memory and connectivity](disk-memory-connectivity.md) |
| Paths | Paths | [Paths and logs](paths-and-logs.md) |
| Logs | Logs | [Paths and logs](paths-and-logs.md) |
| Monitors | Monitors | [Monitors](monitors.md) |
| Downloader | Downloader | [Downloader](downloader.md) |
| Cookies | Cookies | [Cookies](cookies.md) |
| Disk | Disk | [Disk, memory and connectivity](disk-memory-connectivity.md) |
| Updates | Updates | [Updates, BotGuard and Twitch](updates-bgutils-twitch.md) |
| System | — | Only the **Restart Moombox** button. In the TUI, restart with ++r++ ++p++. |
| BotGuard | BotGuard Sidecar | [Updates, BotGuard and Twitch](updates-bgutils-twitch.md) |
| Twitch | Twitch | [Updates, BotGuard and Twitch](updates-bgutils-twitch.md) |
| Memory | Memory | [Disk, memory and connectivity](disk-memory-connectivity.md) |
| Channels | Channels | [Channels](channels.md) (and the guide, [Channels and monitoring](../guide/channels.md)) |
| Integrations | Integrations | [Notifications](notifications.md) |

A few keys have no control in either UI and can only be set by hand, for
example [`cookies.dpapi_profile_dir`](cookies.md#cookiesdpapi_profile_dir)
and the `"public"` spelling of
[`network.network_access`](network.md#networknetwork_access). Each key's
table on its section page says where it can be changed.

### Hand edits and saves do not mix while running

Moombox reads `config.toml` once, at startup, and does not watch it. Every
save — from the dashboard, from the TUI, or one Moombox makes on its own —
writes out the whole configuration it holds in memory. So:

- **An edit made while Moombox is running is lost** at the next save, and it
  is not used before then. Stop Moombox first, edit, then start it.
- **Comments are not kept.** The file is rewritten without comments, in
  Moombox's own key order, every time it is saved. Keep notes elsewhere.
- Moombox saves on its own more often than you might expect: on the first
  start after an upgrade that added a section, when it records the version
  that ran, when it hashes a plain-text password, and when you skip a release.

Saves are atomic: Moombox writes a temporary file beside `config.toml` and
renames it over the original, so an interrupted save never leaves half a
file. On Linux the file is written readable by your user only (mode `0600`),
because it can hold the password hash and notification webhook URLs.

## When changes take effect

Most settings apply as soon as they are saved, or the next time they are used
(the next monitor check, the next download, the next refresh). Each key's
table on its section page has a column saying which.

These settings are read only when Moombox starts, so a change needs a restart:

| Section | Keys |
|---|---|
| Network | `port`, `https_enabled`, `tls_cert_path`, `tls_key_path`, and `network_access` when moving **off** `localhost` (who is admitted changes at once; listening on the network starts only at startup) |
| Paths | `database_path`, `log_file_path` |
| Logs | `log_max_file_size`, `log_max_files` |
| Cookies | `cookie_file`, `browser_profile_dir`, `refresh_interval`, and the refresh timer half of `auto_enabled` |
| Connectivity | `probe_targets` |
| Memory | `sidecar_hard_limit_mb` |
| BotGuard | `use_sidecar` |
| Twitch | `client_id` |

Both UIs mark these fields:

- The dashboard puts a **Restart** badge on them, and after a save that
  changed one it shows a **Restart Required** prompt: *Some settings require
  a restart to take effect (port, network access, HTTPS/TLS, connectivity
  probe targets, database path, log settings, cookie settings, sidecar
  settings, Twitch Client-ID). Restart Moombox now?* The list is always the
  same; it names the kinds of setting that need a restart, not only the ones
  you changed. Answering **Restart** restarts Moombox in place; in-progress
  downloads are saved on shutdown and resume on startup. Only values you
  saved from this page count, and only when they differ from what the form
  showed when it was loaded.
- The TUI tags an edited field **[restart required]** and, after the save,
  shows a banner: *⚠ Restart required — saved config differs from running
  process. Press R P P to restart Moombox, or restart it yourself.*

What the dashboard does after the prompt:

- **If you decline**, nothing restarts and the saved values wait in
  `config.toml`. The prompt comes back on every later save from the same
  page until you accept it or reload the page.
- **If the port or HTTPS changed**, the browser cannot stay where it is. A
  notice reads *Redirecting to `<scheme>://<host>:<port>/` in a few
  seconds...* and about 3.5 seconds later the page moves there. The host is
  the one already in your address bar; the scheme follows
  `https_enabled`.
- **If only HTTPS changed**, the redirect keeps the port the page is
  currently served from, not the configured `port`. Behind a published
  Docker port, or when Moombox fell back to a nearby port because its own
  was taken, that is the address you were already using, which is usually
  what you want. When you change the port itself, the redirect goes to the
  new configured port, so a Docker mapping or a fallback port can leave it
  pointing somewhere nothing answers; open the right address by hand.
- **If the restart request fails**, the redirect is cancelled and the
  dashboard shows *Failed to restart: …*. The settings are still saved;
  restart from **Settings → System → Restart Moombox** or restart Moombox
  yourself.

You can restart at any time from the dashboard (**Settings → System →
Restart Moombox**) or the TUI (++r++ ++p++, confirmed with a third
++p++ within 3 seconds).

!!! note "Logging level is live"
    [`logs.log_level`](paths-and-logs.md#logslog_level) applies the moment
    it is saved. For a one-off debug run without changing the file, start
    Moombox with `-log-level DEBUG` instead (see
    [Command line](../reference/cli.md)).

!!! note "Changing the staging directory"
    New jobs use a new
    [`paths.staging_directory`](paths-and-logs.md#pathsstaging_directory) at
    once, but staged data under the old directory is no longer found. Let
    running jobs finish first, or move the old staging contents across.

## Values Moombox cannot use

What happens depends on where the bad value comes from.

**Saved from the dashboard or the TUI:** the save is refused and nothing is
written. The dashboard names the field and the problem. The TUI names the
problem under the form, or shows `Save failed: …` for a rule it does not
check itself. Fix the value and save again.

**Hand-edited into `config.toml`, but the file is valid TOML:** Moombox
starts anyway. Each out-of-range value, and each value outside a key's fixed
choices (for example `network_access = "everyone"`), is replaced in memory and
logged at startup as a warning:

```text
Config value replaced by its default — the next save writes the default to the file issue="monitors.archive_slots 0 out of range 1..100"
```

The replacement is usually the default. A few keys fall back differently:

- [`monitors.decapi_check_interval`](monitors.md#monitorsdecapi_check_interval)
  and [`monitors.twitch_check_interval`](monitors.md#monitorstwitch_check_interval)
  out of range become unset (dynamic timing for DECAPI, 15 seconds for
  Twitch);
- an unusable [`network.public_url`](network.md#networkpublic_url) is cleared;
- an invalid entry in [`network.trusted_proxies`](network.md#networktrusted_proxies)
  is dropped and the valid ones are kept;
- one unusable entry in
  [`connectivity.probe_targets`](disk-memory-connectivity.md#connectivityprobe_targets),
  or an empty list, resets the whole list to the defaults.

As the warning says, the next save writes the replacement into the file, so
the bad value does not survive. Channel entries are corrected the same way;
see [Channels](channels.md).

**The file is not valid TOML, or a value has the wrong type** (text where a
number belongs, a duration string Moombox cannot read): Moombox does not start.
It prints and logs

```text
Startup error: load config: failed to parse config /path/config.toml: …
```

and exits with code 3. The message names the line. On an interactive terminal
it waits for ++enter++ so you can read it; under Docker or systemd it exits
at once, and the launcher does not restart it.

!!! tip "Check what was actually loaded"
    After a hand edit, read the first few log lines (the dashboard's **Logs**
    tab, filtered to **Warn**, or `moombox.log`). Every replaced value and
    every retired key is listed there. A **misspelled or unknown key is not
    reported**: Moombox silently ignores it, and the next save drops it from
    the file. If a hand edit seems to have no effect, check the key's spelling
    and the `[section]` it sits under against the section pages.

## Durations

Six settings accept either a plain number or a duration string:

| Key | A plain number means |
|---|---|
| [`monitors.feed_check_interval`](monitors.md#monitorsfeed_check_interval) | minutes |
| [`monitors.hide_finished_age_days`](monitors.md#monitorshide_finished_age_days) | days |
| [`monitors.probe_cooldown`](monitors.md#monitorsprobe_cooldown) | seconds |
| [`downloader.interruption_timeout`](downloader.md#downloaderinterruption_timeout) | minutes |
| [`downloader.incomplete_staging_expiry_days`](downloader.md#downloaderincomplete_staging_expiry_days) | days |
| [`cookies.refresh_interval`](cookies.md#cookiesrefresh_interval) | minutes |

A duration string is a number followed by one unit: `ms`, `s`, `m`, `h`, `d`
or `w` (milliseconds, seconds, minutes, hours, days, weeks). Decimals are
allowed, a space before the unit is allowed, and case does not matter. Only
one unit per value: `"1.5h"` works, `"1h30m"` does not.

```toml
[monitors]
feed_check_interval = "10m"      # same as 10
hide_finished_age_days = "2w"    # same as 14
probe_cooldown = "2m"            # same as 120 (seconds)

[cookies]
refresh_interval = "6h"          # same as 360 (minutes)
```

Things to know:

- A plain number may have a fraction (`feed_check_interval = 2.5`) and may be
  written as a quoted number (`"10"`).
- The string is converted into the key's own unit, so
  `probe_cooldown = "30s"` is 30 seconds and `refresh_interval = "1d"` is
  1440 minutes.
- Moombox writes the value back as a plain number in that unit the next time
  it saves (`"6h"` becomes `360`).
- A string it cannot read (`"10 minutes"`, `"1h30m"`) stops startup with a
  parse error. A negative number is replaced by the default with a warning.
- In the dashboard and the TUI you type a number in the unit the field names.
- Every other time-like setting is a **plain integer only**, in the unit its
  name or description gives: for example
  [`downloader.maximum_timeout`](downloader.md#downloadermaximum_timeout)
  (seconds), [`monitors.archive_window_days`](monitors.md#monitorsarchive_window_days)
  (days) and [`downloader.progress_interval_ms`](downloader.md#downloaderprogress_interval_ms).
  Writing `"3d"` there stops startup with a parse error.

## Upgrading from older versions

### Automatic migration

Older Moombox versions used a flat file and some differently named sections.
Moombox reads those layouts and moves each value to its current place:

| Old form | Current key |
|---|---|
| `allow_lan = true` / `allow_external = true` (top level) | [`network.network_access`](network.md#networknetwork_access) `"lan"` / `"external"` |
| top-level `port`, `network_access`, `https_enabled`, `tls_cert_path`, `tls_key_path`, `password_hash` | the same keys under `[network]` |
| top-level `log_level`, `log_max_file_size`, `log_max_files` | the same keys under `[logs]` |
| top-level `log_file_path`, `database_path` | the same keys under `[paths]` |
| top-level `feed_check_interval`, `decapi_check_interval`, `twitch_check_interval`, `hide_finished_age_days` | the same keys under `[monitors]` |
| `[tasklist] hide_finished_age_days` | [`monitors.hide_finished_age_days`](monitors.md#monitorshide_finished_age_days) |
| `[downloader] output_directory`, `staging_directory`, `ffmpeg_path` | the same keys under `[paths]` |
| `[downloader] cookie_file` | [`cookies.cookie_file`](cookies.md#cookiescookie_file) |
| `[auto_cookies] enabled` | [`cookies.auto_enabled`](cookies.md#cookiesauto_enabled) |
| `[auto_cookies] browser_profile_dir`, `refresh_interval`, `platforms` | the same keys under `[cookies]` |

Migration never overrides a value that is already in its new place: an old
key is used only when the current key is absent. For network access, an
explicit `network_access` anywhere in the file always wins over
`allow_lan`/`allow_external`. The old keys are dropped from the file the next
time Moombox saves it.

### New sections are added for you

When the file has no `[memory]`, `[bgutils]` or `[connectivity]` section
(a file from before they existed), Moombox fills them with defaults and saves
the file once at startup, logging *Auto-persisted new config sections to
disk*. That way the current values show up in both UIs. Any other missing key
simply takes its default.

### Retired keys

`downloader.po_token` and `downloader.visitor_data` are no longer used. They
were accepted as a manual proof-of-origin token override, but PO tokens are
now minted per session by the BotGuard sidecar
([`bgutils.use_sidecar`](updates-bgutils-twitch.md#bgutilsuse_sidecar)), and
a pasted token would expire within hours anyway. If your file still has them,
Moombox ignores them, logs

```text
Config key is no longer used and is ignored — the next save removes it from the file key=downloader.po_token
```

and drops them at the next save. You can delete the lines yourself (with
Moombox stopped).

### Keys Moombox writes itself

Some keys are bookkeeping that Moombox writes and reads back; leave them
alone:

- [`network.password_hash`](network.md#networkpassword_hash) — the hashed
  dashboard password. You may put a **plain** password there by hand while
  Moombox is stopped; it is hashed at the next start.
- [`cookies.platforms`](cookies.md#cookiesplatforms) — the platforms whose
  cookies were verified.
- [`updates.last_run_version`](updates-bgutils-twitch.md#updateslast_run_version),
  [`updates.last_failure_marker_seen`](updates-bgutils-twitch.md#updateslast_failure_marker_seen),
  [`updates.skipped_version`](updates-bgutils-twitch.md#updatesskipped_version).

## Settings by section

| Page | Sections and arrays it covers |
|---|---|
| [Network](network.md) | `[network]`: port, who may connect, HTTPS, the dashboard password, reverse proxies, the public URL |
| [Paths and logs](paths-and-logs.md) | `[paths]` and `[logs]`: database, output, staging, FFmpeg, log file, rotation and level |
| [Monitors](monitors.md) | `[monitors]`: archive window and slots, polling intervals, probe cooldown, members-only discovery, when finished jobs move to Archived |
| [Downloader](downloader.md) | `[downloader]`: file naming, quality, concurrency, memory for reordering, chat, stall and interruption timeouts |
| [Cookies](cookies.md) | `[cookies]`: cookie file, browser login and refresh, profile import, status-bar platforms, Windows DPAPI fallback |
| [Disk, memory and connectivity](disk-memory-connectivity.md) | `[disk]`, `[memory]`, `[connectivity]` |
| [Updates, BotGuard and Twitch](updates-bgutils-twitch.md) | `[updates]`, `[bgutils]`, `[twitch]` |
| [Channels](channels.md) | `[[channels]]`: one block per monitored channel, with per-channel overrides |
| [Notifications](notifications.md) | `[[notifications]]`: Discord webhooks |

## A complete example

This file spells out every setting with its default value. You never need a
file this long: any key you leave out takes the default shown. Lines
commented out are keys that are absent by default.

```toml title="config.toml"
[network]
port = 774                      # dashboard port; if taken, the next 10 are tried
network_access = "localhost"    # "localhost" | "lan" | "external" ("public" = "external", file only)
https_enabled = false           # self-signed certificate generated on first HTTPS start
# tls_cert_path = "./moombox.crt"
# tls_key_path = "./moombox.key"
client_token_ttl_days = 365     # "remember me" lifetime, 1-3650 days
# trust_forwarded_proto = false # only behind a TLS-terminating reverse proxy
# trusted_proxies = []          # reverse-proxy IPs/CIDRs; leave empty without one
# public_url = ""               # external address of the dashboard, used in notification links
# password_hash = ""            # written by Moombox

[paths]
database_path = "./moombox.db"
log_file_path = "./moombox.log"
output_directory = "./output"   # finished archives
staging_directory = "./staging" # downloads in progress
# ffmpeg_path = ""              # empty = ffmpeg from PATH

[logs]
log_level = "INFO"              # DEBUG | INFO | WARN | ERROR
log_max_file_size = 10485760    # bytes (10 MB), 1 KB-1 GB
log_max_files = 5               # rotated files kept, 1-100

[monitors]
archive_window_days = 3         # how far back to archive, 1-3650 days
archive_slots = 3               # backlog downloads per channel at once, 1-100
feed_check_interval = 10        # minutes, 1-1440
# decapi_check_interval = 60    # seconds, 15-3600; absent = dynamic
# twitch_check_interval = 15    # seconds, 5-3600; absent = 15
hide_finished_age_days = 30     # days, 0-365; 0 = archive at once
probe_cooldown = 0              # seconds; 0 = re-probe every cycle
membership_discovery = true     # scan members-only tabs (needs cookies)

[downloader]
output_template = "${channel}/${start_date} ${title} [${id}]"
max_video_resolution = 2160     # shorter frame edge; 0 = always the largest
num_parallel_downloads = 10     # VOD downloads at once; live never waits on this
segment_workers = 12            # segments fetched at once per download; >16 is logged as risky
reorder_buffer_mb = 1024        # 256 on arm64; 0 = unbounded
reorder_budget_mb = 4096        # 1024 on arm64; 0 = unbounded
progress_interval_ms = 16       # minimum gap between progress reports
download_chat = true
prefer_60fps = true
prefer_hdr = true
maximum_timeout = 600           # seconds, minimum 30
interruption_timeout = 120      # minutes; 0 = never wait
incomplete_staging_expiry_days = 7  # days; 0 = keep forever

[cookies]
cookie_file = "./cookies.txt"
auto_enabled = false            # headless refresh timer + one browser retry on auth failure
browser_profile_dir = "./browser-profile"
refresh_interval = 360          # minutes, 10-10080
acquisition = "auto"            # "auto" | "profile"
# browser_path = ""             # empty = auto-detect
# browser_type = ""             # required with browser_path
# active_platforms = ["youtube", "twitch"]  # absent = automatic
# dpapi_fallback = false        # Windows only
# dpapi_profile_dir = ""        # Windows only, file only
# platforms = []                # written by Moombox

[disk]
disk_warn_percent = 90          # 1-99
disk_critical_percent = 95      # above the warning; backlog waits from here

[updates]
auto_check_updates = true       # on startup and every 24 hours
# last_run_version, last_failure_marker_seen, skipped_version: written by Moombox

[bgutils]
use_sidecar = true              # BotGuard sidecar for PO tokens and signatures

[memory]
go_soft_limit_mb = 256          # 0 disables
sidecar_soft_limit_mb = 200     # 0 disables
sidecar_hard_limit_mb = 512     # 0 = V8's own default

[connectivity]
probe_targets = ["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"]

[twitch]
client_id = ""                  # empty = built-in Client-ID

# One block per channel.
[[channels]]
id = "UCxxxxxxxxxxxxxxxxxxxxxx" # YouTube channel ID
name = "Some VTuber"
terms = "(?i)karaoke|singing"   # optional title filter (RE2 regex)
include_non_live_content = false
# archive_window_days = 3       # per-channel overrides
# archive_slots = 3
# output_directory = "./output/special"
# quality_preference = "1080p60"

[[channels]]
id = "somestreamer"             # Twitch login name
platform = "twitch"
quality_preference = "best"
twitch_gap_fill_ad_breaks = false
twitch_vod_backlog = false
twitch_archive_clips = false

# One block per Discord webhook.
[[notifications]]
url = "https://discord.com/api/webhooks/ID/TOKEN"
enabled = true
events = ["found", "finished", "error"]   # absent = every event
mention = "<@&123456789012345678>"
# mention_events = ["error", "auth"]      # absent = the default six
mode = "separate"               # "separate" | "edit"
```

The repository's `config.example.toml` has the same settings with longer
comments; it is the file to copy if you prefer to start by hand rather than
through the setup wizard.

!!! note "Docker defaults differ"
    The image's first start writes a short file with
    `network_access = "lan"`, every path under `/data`, and
    `auto_check_updates = false`. Everything else keeps the defaults above.
    See [Docker](../getting-started/docker.md).
