# Disk, memory and connectivity

Three small sections tune how Moombox looks after the machine it runs on:

- `[disk]` sets when a filling output drive raises an alert, and when Moombox
  stops starting backlog downloads onto it.
- `[memory]` puts soft and hard ceilings on the memory used by Moombox itself
  and by its BotGuard sidecar (the embedded Node.js process that mints YouTube
  PO tokens and solves signatures).
- `[connectivity]` names the internet hosts Moombox dials to decide whether
  the machine is online.

```toml
[disk]
disk_warn_percent = 90
disk_critical_percent = 95

[memory]
go_soft_limit_mb = 256
sidecar_soft_limit_mb = 200
sidecar_hard_limit_mb = 512

[connectivity]
probe_targets = ["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"]
```

!!! info "When changes take effect"
    The disk thresholds, `go_soft_limit_mb` and `sidecar_soft_limit_mb` apply
    without a restart. `sidecar_hard_limit_mb` and `probe_targets` are read
    once at startup: both UIs mark them **Requires restart**, and you can
    restart from **Settings → System → Restart Moombox** in the dashboard.

    If you edit `config.toml` by hand, Moombox reads it at the next start.
    An invalid value in a hand-edited file is replaced (the rules are listed
    under each key) and a warning is logged; the dashboard and the TUI refuse
    an invalid value when you save. Stop Moombox before editing the file: an
    edit made while it runs is not picked up, and the next save from either UI
    writes the running settings back over it.

If an older `config.toml` has no `[memory]` or `[connectivity]` section,
Moombox writes the section with its defaults on the first start after the
upgrade, so the values appear in the file and in both settings screens.

## Disk

Moombox measures the volume that holds the
[output directory](paths-and-logs.md#pathsoutput_directory): how full it is,
in percent, and how many gigabytes are free. It takes a reading at startup,
every six minutes after that, and at once when you save new thresholds or a
new output or staging directory. The dashboard's disk gauge and the TUI status bar
(`Disk 45% (120G free)`) show the latest reading. While backlog admission is
still held but usage has already dropped below the critical threshold, the
dashboard gauge shows a separate "holding" state rather than plain warning,
and the TUI adds `holding` to its disk reading, so you can tell admission has
not resumed yet.

!!! note "A staging directory on another drive is watched too"
    If your [staging directory](paths-and-logs.md#pathsstaging_directory) is
    on a different volume, Moombox reads that volume at every check as well.
    The disk alerts and the **Held: disk full** backlog hold go by the fuller
    of the two, and the alert says whether it is the output drive or the
    staging drive. The gauge and the status bar still show the output
    volume only, so an alert or a hold can appear while the gauge looks
    healthy. If the staging volume cannot be read, the output volume is
    judged alone.

What the two thresholds set off:

| Usage | What happens |
|---|---|
| At or above `disk_warn_percent` | A **Disk Space Warning** notification (event `disk_warning`). The gauge turns to its warning colour. |
| At or above `disk_critical_percent` | A **Disk Space Critical** notification (event `disk_critical`; a target filtering on `disk_warning` receives it too). **Backlog admission stops**: rows in `Queued` stay there, shown as **Held: disk full**. |
| Back 2 points or more below a threshold | An open critical alert steps down once usage is 2 points below the critical threshold (to a **Disk Space Warning** if usage is still at or above the warning threshold), and backlog admission resumes at the same reading. An open warning closes with **Disk Space Recovered** (event `disk_ok`) once usage is 2 points below the warning threshold. |

The 2-point recovery margin stops a volume sitting right on the line from
sending a warning and a recovery on every reading, and from admitting one more
backlog download each time it dips a hair under the threshold. Between the
threshold and the margin nothing changes: an alert that is open stays open,
and a hold that is on stays on.

A level that stays the same is repeated at most every 30 minutes; a change of
level (warning to critical) is sent at once. If two readings in a row fail
(the volume went offline, an I/O error), Moombox sends **Disk Monitoring
Failed** and the gauge and alerts freeze at the last good reading until a
reading succeeds again (**Disk Monitoring Recovered**). A failed reading never
reopens a disk hold.

!!! warning "The critical hold does not stop running downloads"
    Only rows waiting in `Queued` are held: YouTube backlog VODs, the Twitch
    VOD backlog and pasted Twitch listings, and the Twitch clip sweep's clips.
    Live streams, upcoming streams, newly published videos, single videos
    or streams you add by hand, and every download already running carry on and keep writing to the
    disk. Free space before the disk actually fills.

Other things that respect the critical threshold:

- A backlog VOD whose download or mux fails because the disk is full goes back
  to `Queued` (keeping its staged files) instead of ending in `Error`. It
  waits 5 minutes before it can be admitted again (10, then 20 minutes on
  later attempts), and for as long as the disk hold lasts. After three such
  retries in a row the next failure ends it in `Error`.
- The Twitch ad-break gap fill waits while the volume is at the critical
  threshold.
- **Import Chapters** refuses to re-mux an archive when the temporary copy it
  writes would take the volume to the critical threshold.

The disk hold and an open `disk_critical` alert survive a restart: Moombox
starts with backlog admission still held, and the first reading 2 points below
the critical threshold releases it.

!!! note "Linux: Moombox's percentage can be higher than `df`'s"
    On Linux, Moombox counts as "free" only the space an ordinary user can
    write to. Space the file system reserves for the root user (commonly 5% on
    ext4) is counted as used, so Moombox may report a few points more than the
    `Use%` column of `df`. Leave room for that when you pick thresholds.

### `disk.disk_warn_percent`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `90` | Whole number 1–99, lower than `disk_critical_percent` | Immediately (a new reading is taken on save) | Dashboard: Settings → Disk → **Warning Threshold**. TUI: Settings → Disk → **Warning threshold**. |

The usage percentage at which Moombox sends a **Disk Space Warning**. It only
notifies; nothing is held at this level. The warning closes with **Disk Space
Recovered** once usage is 2 points or more below this value.

Lower it if you want earlier notice on a drive that fills quickly (several
simultaneous 4K captures can write tens of gigabytes an hour).

Validation: both UIs refuse a value outside 1–99, or one that is not lower than
the critical threshold. In a hand-edited file, an out-of-range value is reset
to `90`. If the pair is out of order, the critical threshold is raised to the
warning threshold plus 5 (at most 99); if the warning threshold is 95 or more,
both are reset to `90` and `95`.

```toml
[disk]
disk_warn_percent = 80
```

### `disk.disk_critical_percent`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `95` | Whole number 1–99, higher than `disk_warn_percent` | Immediately (a new reading is taken on save) | Dashboard: Settings → Disk → **Critical Threshold**. TUI: Settings → Disk → **Critical threshold**. |

The usage percentage at which Moombox sends a **Disk Space Critical**
notification and stops admitting `Queued` rows. Admission resumes when usage is
2 points or more below it (93% with the default). See the table above for
everything this threshold controls.

Set it low enough that the downloads that keep running after it is reached
(live captures are never paused) still fit in what is left. On a small drive,
5% may be only a few gigabytes.

Validation: both UIs refuse a value outside 1–99 or one that is not higher
than the warning threshold. In a hand-edited file, an out-of-range value is
reset to `95`; an out-of-order pair is repaired as described under
[`disk_warn_percent`](#diskdisk_warn_percent).

```toml
[disk]
disk_warn_percent = 85
disk_critical_percent = 92
```

## Memory

Moombox runs as two processes that hold real memory: Moombox itself (written
in Go) and, when [`bgutils.use_sidecar`](updates-bgutils-twitch.md#bgutilsuse_sidecar)
is on, the BotGuard sidecar (a Node.js/V8 process). The defaults sit above
normal steady-state use (Moombox around 240 MB with downloads running, the
sidecar around 150 MB after minting, with bursts of 400–500 MB), so the limits
only act on real growth. Moombox logs a memory line every two minutes at
`DEBUG` level, including the sidecar's RSS and V8 heap, which is the easiest
way to see where you stand before changing anything.

Setting any of the three keys to `0` switches that limit off.

### `memory.go_soft_limit_mb`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `256` | 0–65536 (MB); `0` = no limit | Immediately | Dashboard: Settings → Memory → **Go soft limit (MB)**. TUI: Settings → Memory → **Go soft limit (MB)**. |

A soft memory limit for the Moombox process. As memory use approaches it,
Moombox's garbage collector runs more often to stay under it. It is soft: if
Moombox genuinely needs more, it uses more rather than crashing. The cost of a
limit set too low is extra CPU spent on garbage collection, not a crash.

Raise it if you run many downloads at once (high
[`num_parallel_downloads`](downloader.md#downloadernum_parallel_downloads) or
large [reorder buffers](downloader.md#downloaderreorder_budget_mb)) and see
high CPU with steady memory. Lower it on a small device where you prefer
memory back over CPU.

`0` removes Moombox's limit. If the `GOMEMLIMIT` environment variable was set
when Moombox started, `0` falls back to that value instead of no limit.

Validation: both UIs refuse values outside 0–65536. A negative value in a
hand-edited file becomes `0` (no limit).

```toml
[memory]
go_soft_limit_mb = 512
```

### `memory.sidecar_soft_limit_mb`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `200` | 0–65536 (MB); `0` = off. Must be lower than `sidecar_hard_limit_mb` when both are non-zero | Within two minutes (next memory check) | Dashboard: Settings → Memory → **Sidecar soft limit (MB)**. TUI: Settings → Memory → **Sidecar soft limit (MB)**. |

Every two minutes Moombox reads the sidecar's resident memory (RSS). When it
is at or above this value, Moombox asks the sidecar to run a full garbage
collection and logs `[Memory] sidecar soft limit hit; ran GC` with how much
was reclaimed. Nothing is stopped or restarted. V8 has no soft limit of its
own, so this periodic nudge is the soft half of the pair.

`0` turns the nudge off. The check only runs while the sidecar is up and
answering.

Validation: both UIs refuse values outside 0–65536, and a soft limit at or
above a non-zero hard limit. In a hand-edited file a negative value becomes
`0`, and a pair where the hard limit is not above the soft limit is reset to
`200` and `512`.

```toml
[memory]
sidecar_soft_limit_mb = 300
```

### `memory.sidecar_hard_limit_mb`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `512` | 0–65536 (MB); `0` = V8's own default. Must be higher than `sidecar_soft_limit_mb` when both are non-zero | Restart | Dashboard: Settings → Memory → **Sidecar hard limit (MB)** (Requires restart). TUI: Settings → Memory → **Sidecar hard limit (MB)** (requires restart). |

The sidecar's V8 heap ceiling (Node's `--max-old-space-size`). This one is
hard: when the sidecar's heap reaches it, V8 aborts the sidecar with an
out-of-memory error. Moombox restarts it automatically after 5 s, then 15 s,
then 60 s, then every 5 minutes while it keeps failing. While it is down, PO
tokens cannot be minted and signature-ciphered YouTube formats are
unavailable. The TUI status bar shows **SIDECAR DOWN** (**POT** on a narrow
terminal) while it is down. If it stays down for 60 seconds you get a
**BotGuard Sidecar Down** notification (event `sidecar_down`); **BotGuard
Sidecar Restored** follows when it is back.

Keep it comfortably above the soft limit: BotGuard bursts of 400–500 MB are
normal, and the default 512 sits just above them. If your log shows repeated
sidecar restarts after out-of-memory aborts, raise it (768 or 1024).

`0` passes no ceiling, so V8 uses its own default, which depends on the
machine's memory.

Validation: the same rules as
[`sidecar_soft_limit_mb`](#memorysidecar_soft_limit_mb).

```toml
[memory]
sidecar_soft_limit_mb = 300
sidecar_hard_limit_mb = 1024
```

## Connectivity

Moombox keeps track of whether the machine can reach the internet, so that it
does not spend requests, retries and error budgets on a dead link. Every five
seconds it races TCP connections to the probe targets; the first one that
completes a handshake proves the link is up. When no probe target has
connected after about one second, or all of them have failed sooner, Moombox
also tries `www.youtube.com:443` and `gql.twitch.tv:443` in the same poll and
counts either one as online. If nothing answers within three seconds, the
poll fails. Two failed polls in a row put Moombox **offline**;
one good poll puts it back online.
Besides the active probe, a burst of failed requests (five or more, from at
least two different parts of Moombox, within 30 seconds) also puts it offline
at once; when Moombox went offline that way, requests that succeed again can
bring it back online without waiting for a poll.

While Moombox is offline:

- The dashboard shows an **Internet connection lost** banner and the TUI status
  bar shows **OFFLINE**.
- Channel monitors skip their polls, and the backlog scheduler admits nothing:
  `Queued` rows show **Held: offline**.
- Running downloads wait for the link instead of counting failures against
  their retries.
- Notifications are held and delivered when the link returns, marked as
  delayed by an outage.
- After an outage of at least 60 seconds, an **Outage Alert** (event
  `connectivity_restored`) reports when it started, when it ended and how long
  it lasted; a flapping link is reported as one episode. A live Twitch capture
  is restarted only after an outage of at least 60 seconds.

See [Notifications](../guide/notifications.md) for the outage events and
[Jobs](../guide/jobs.md) for how downloads ride out an outage.

### `connectivity.probe_targets`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `["1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"]` | One or more `host:port` entries | Restart | Dashboard: Settings → Network → **Connectivity Probe Targets** (comma-separated; Requires restart). TUI: Settings → Network → **Connectivity probe targets** (comma-separated; requires restart). |

The hosts Moombox connects to to decide whether the internet is reachable. Only
a TCP handshake is made; nothing is sent. The defaults are three public DNS
providers' anycast addresses on port 443 (HTTPS), chosen because port 443 gets
through networks that block outbound DNS, and three of them so one provider's
outage cannot make Moombox think it is offline.

Change it when your network blocks those addresses: a corporate firewall, a
country that blocks one of the providers, or a host that can only reach the
internet through specific endpoints. If none of the targets is reachable,
Moombox stays online as long as YouTube or Twitch answers (see the fallback
above) and logs a warning that names this setting; it is offline only when
those fail too. The fallback can add up to a second to each poll, so still pick hosts that
are reliably up and reachable from where Moombox runs. Use more than one. A target given by name is looked up with the system's DNS on
every poll, so it also fails while DNS does; IP addresses avoid that.

Each entry must be:

- `host:port`, where the host is an IP address or a name (names are not looked
  up when you save) and the port is 1–65535 or a TCP service name the system
  knows (such as `https`). IPv6 addresses go in brackets: `[2606:4700:4700::1111]:443`.
- Not this machine. An empty host (`:443`), `localhost`, a name ending in
  `.localhost`, a loopback address (`127.0.0.1`, `::1`) or an unspecified
  address (`0.0.0.0`, `::`) is refused, because a local service would answer
  through a real outage.

Validation: both UIs refuse an entry that breaks these rules. Leaving the field
empty in either UI keeps the targets you already have; it never clears the
list. In a hand-edited file, an empty list, or a list where any entry is
invalid, is replaced as a whole with the three defaults.

!!! note "Outbound proxies"
    The probe is a direct TCP connection; it does not go through
    `HTTPS_PROXY`. On a network where only a proxy can reach the internet,
    list the proxy's own `host:port` as a target (this only works when the
    proxy runs on another machine: a proxy on this machine's loopback address
    is refused by the rules above).

```toml
[connectivity]
probe_targets = ["1.1.1.1:443", "208.67.222.222:443", "www.example.com:443"]
```
