# Docker

Moombox publishes a multi-architecture image (linux/amd64 and linux/arm64) at
`ghcr.io/vampiricwulf/moombox`. It contains the Moombox binary, FFmpeg, CA
certificates and time-zone data on a Debian base. Everything Moombox stores
lives in one volume, `/data`.

## Quick start with Docker Compose

```bash
mkdir moombox && cd moombox
wget https://raw.githubusercontent.com/vampiricwulf/Moombox/main/docker-compose.yml
docker compose up -d
```

Then open `http://<docker-host-IPv4-address>:774`. There is no setup wizard in
the container: the image writes a starter configuration on first start, and
you change everything else from the dashboard's **Settings** tab.

Everything — configuration, database, logs, staging and finished downloads —
appears under `./data` next to the compose file.

## Image tags

| Tag | What it is |
|---|---|
| `latest` | The newest stable release. Pre-releases never move it. |
| `X.Y.Z` (for example `2.9.8`) | One exact release. |
| `X.Y` (for example `2.9`) | The newest patch release of that minor version. |
| `edge` | A build of the `main` branch, pushed by hand for testing image changes. Not a release. |

Pin `X.Y.Z` if you want to choose when to upgrade. The image's binary reports
the same version as the release (`docker compose exec moombox moombox --version`).

## What the shipped compose file sets up

```yaml title="docker-compose.yml (abridged)"
services:
  moombox:
    image: ghcr.io/vampiricwulf/moombox:latest
    container_name: moombox
    restart: unless-stopped
    init: true                 # reaps stray ffmpeg / helper processes
    stop_grace_period: 1m      # lets in-flight muxing finish on shutdown
    ports:
      - "774:774"
    volumes:
      - ./data:/data
    healthcheck:
      test: ["CMD", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/774"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s

networks:
  default:
    enable_ipv6: true
    ipam:
      config:
        - subnet: "fd77:4d42::/64"
```

Keep `init: true` and a generous `stop_grace_period`. On a stop, Moombox needs
up to about 15 seconds to save in-progress downloads and cancel muxes cleanly;
Docker's default grace period is 10 seconds, after which it kills the
container.

The `networks:` block is a security measure, explained under
[Network access in a container](#network-access-in-a-container).

### Running with `docker run`

```bash
docker run -d --name moombox \
  --init --restart unless-stopped --stop-timeout 60 \
  -p 0.0.0.0:774:774 \
  -v "$PWD/data:/data" \
  ghcr.io/vampiricwulf/moombox:latest
```

The `0.0.0.0:` prefix stops the published port from accepting IPv6, which the
default bridge network would otherwise let through looking like a private
address (see below). Use `127.0.0.1:774:774` to make the dashboard reachable
from the Docker host only.

### Building the image yourself

From a checkout of the repository, `docker build -t moombox .` (or
`docker build --platform linux/arm64 -t moombox:arm64 .`) builds the same image,
helper bundle and all, with no local Go or Node.js toolchain. In the compose
file, comment out `image:` and uncomment `build: .`.

## The seeded configuration

On the first start, when `/data/config.toml` does not exist, the image's
entrypoint writes one containing only what a container needs:

```toml title="/data/config.toml as first written"
[network]
network_access = "lan"
port = 774

[paths]
database_path = "/data/moombox.db"
log_file_path = "/data/moombox.log"
output_directory = "/data/output"
staging_directory = "/data/staging"

[cookies]
cookie_file = "/data/cookies.txt"

[updates]
auto_check_updates = false

[memory]

[bgutils]

[connectivity]
```

Every other setting keeps its built-in default. Two of these differ from a
desktop install on purpose:

[`network.network_access`](../configuration/network.md#networknetwork_access) = `lan`
:   Requests reach the container over Docker's bridge network, never over
    loopback, so the desktop default (`localhost`) would make the dashboard
    unreachable.

[`updates.auto_check_updates`](../configuration/updates-bgutils-twitch.md#updatesauto_check_updates) = `false`
:   An in-app update would replace the binary inside the container, and
    recreating the container from its image would silently undo it. Update by
    pulling a new image instead (see [Updating the container](#updating-the-container)).

An existing `config.toml` is never touched by the entrypoint.

!!! warning "config.toml loses its comments"
    Moombox saves `config.toml` by writing out every setting, which drops
    comments. It does so on its first start (to record the version that ran),
    after each update, and whenever a setting or channel is changed from the
    dashboard or the terminal UI. Do not keep notes in it.

    The entrypoint also writes `/data/README-docker.txt`, which explains each
    seeded setting and the container-specific advice on this page. Moombox
    never saves over it. A newer image refreshes it only while it still holds
    an image's text word for word; once you edit it, your copy is kept and the
    newer text goes to `README-docker.txt.new` beside it, once.

To change a setting, use the dashboard's Settings tab. To edit `config.toml`
by hand, stop the container first (`docker compose stop`), edit, then start
it again: Moombox reads the file only at startup, and a save from the
dashboard while it runs would overwrite your edit.

## Volumes and files

Everything is under `/data` (the working directory, and `HOME`, inside the
container):

| Path in the container | What it is |
|---|---|
| `/data/config.toml` | The configuration. |
| `/data/README-docker.txt` | The image's guidance for the seeded settings. |
| `/data/moombox.db` | The job database. |
| `/data/moombox.log` | The log file (the log is also written to the container's output: `docker compose logs -f`). |
| `/data/output/` | Finished archives. |
| `/data/staging/` | Downloads in progress. |
| `/data/cookies.txt` | Your cookie file. |
| `/data/browser-profile/` | Optional Firefox profile to import cookies from. |
| `/data/.cache/Moombox/sidecar/` | The BotGuard helper's runtime, unpacked once and reused across container recreations. |

To keep finished downloads on a bigger disk, add a second mount over the
output directory:

```yaml
    volumes:
      - ./data:/data
      - /mnt/media/vods:/data/output
```

!!! danger "Do not bind-mount cookies.txt on its own"
    Put your cookie file at `./data/cookies.txt` on the host; the `./data`
    mount already exposes it at `/data/cookies.txt`, where the seeded
    configuration points. A line such as `- ./cookies.txt:/data/cookies.txt`
    (with or without `:ro`) breaks Moombox's cookie upkeep: about every 30
    minutes it rewrites the file with the session values YouTube rotates back,
    using a temporary file and a rename, and a rename cannot replace a
    single-file bind mount. The write fails with only a warning in the log,
    and the session quietly expires until downloads start failing for a
    reason that looks unrelated.

### Running as a non-root user

The image runs as root by default. It has no `PUID`/`PGID` variables; use
Docker's own `user:` setting instead, after making `./data` writable by that
user:

```yaml
    user: "1000:1000"
```

## Environment variables

| Variable | Default in the image | Effect |
|---|---|---|
| `TZ` | unset (UTC) | Time zone for `${start_date}` / `${start_time}` in new file names and for log timestamps, for example `TZ=America/Los_Angeles`. Existing archives are not renamed. |
| `MOOMBOX_NO_TUI` | `1` | Keeps Moombox headless even if the container has a terminal. Leave it set. |
| `HOME` | `/data` | Keeps the helper's cache on the volume. |
| `MOOMBOX_CONFIG` | `/data/config.toml` | Where the entrypoint looks for (and seeds) the configuration, and the `-config` it starts Moombox with. `README-docker.txt` goes in the same folder. |

The general variables in [Command line → Environment variables](../reference/cli.md#environment-variables)
work too.

## Network access in a container

The dashboard listens on port 774 inside the container. **The port publish is
your main exposure control**: with `network_access = "lan"`, anything that can
reach the published port and has a private (LAN) address can use the
dashboard without a password.

- To restrict the dashboard to the Docker host, publish
  `"127.0.0.1:774:774"`.
- If you change [`network.port`](../configuration/network.md#networkport),
  change the container side of the publish and the compose health check to
  match.
- To reach the dashboard from outside your LAN, read
  [Security](../reference/security.md) first. A VPN is the recommended route;
  `external` access requires a dashboard password.

!!! warning "Reach the dashboard over IPv4"
    Moombox listens on both IPv4 and IPv6 in the container and judges each
    client by its real address. With the shipped IPv6-enabled network (and
    Docker Engine 27 or later), an IPv6 client keeps its own address: ULA
    (`fc00::/7`) and link-local (`fe80::/10`) clients are allowed on `lan`,
    and a global IPv6 address gets **403 Forbidden**. Most home networks give
    devices global IPv6 addresses, and browsers do not retry over IPv4 after a
    403, so a host name with an AAAA record fails. Use the host's IPv4 address
    or an IPv4-only name, or publish as `"0.0.0.0:774:774"` so the port stops
    accepting IPv6.

    Without that network block (or on Docker Engine older than 27), Docker's
    userland proxy re-sends IPv6 connections from the bridge's private IPv4
    address, so an internet IPv6 client would look like a LAN client. That is
    the hole the block closes. If `docker compose up` fails to create the
    network, your host cannot do IPv6: remove the `networks:` block and publish
    as `"0.0.0.0:774:774"` instead.

!!! note "Docker Desktop (Windows and macOS)"
    Docker Desktop passes every connection through its VM, so Moombox sees
    all clients as the same private gateway address and the `lan` filter
    cannot tell them apart. Keep the publish host-only or firewalled.

!!! note "Host firewalls"
    On Linux, published ports bypass `ufw` and `firewalld` rules, because
    Docker inserts its own forwarding rules. Restrict the publish itself
    rather than relying on the host firewall.

### Setting a dashboard password

The dashboard only lets you set the *first* password from the Moombox
machine itself (a loopback connection), and a container never sees one. Set
it through the configuration instead: stop the container, put the password in
plain text in `config.toml`, and start it again.

```toml
[network]
password_hash = "your password here"
```

On startup Moombox replaces the plain text with a secure hash and saves the
file. Once a password exists you can change it from the dashboard. See
[`network.password_hash`](../configuration/network.md#networkpassword_hash).

## Signing in (cookies)

The image has no browser, so the interactive browser sign-in is not available.
Use one of these:

**Paste or upload a cookie file** (works from any device, including a phone)
:   In the dashboard, Settings → Cookies accepts a Netscape-format
    `cookies.txt` by paste or upload. It is merged into the cookie file (a
    YouTube-only export leaves Twitch alone), loaded at once, and you are told
    whether it authenticates. Export from a private browser window and close
    that window afterwards: browsing on in the same session invalidates it.

**Drop a file in the volume**
:   Write the export to `./data/cookies.txt` on the host. It is picked up
    within 30 minutes, or immediately when you press **Refresh cookies** in
    the dashboard header.

**Import from a Firefox profile**
:   Bind-mount a Firefox profile directory (the one containing `prefs.js` and
    `cookies.sqlite`) at `/data/browser-profile`, for example by copying it
    into `./data/browser-profile`. Close Firefox before copying, and copy
    `cookies.sqlite-wal` along with `cookies.sqlite`, or recent cookies are
    missing. When there is no `cookies.txt` yet, Moombox imports the profile
    once, shortly after it starts. After that it reads the profile only when
    you ask: **Refresh cookies from browser profile** on the Settings page,
    shift+click on the header's **Refresh cookies**, or ++r++ ++f++ in the
    terminal UI. Moombox never writes into the profile.

Leave [`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled)
off in the container: it only drives a headless-browser refresh timer, and
there is no browser to run. The background refresh that keeps an imported
YouTube session alive runs regardless. More in
[Cookies and sign-in](../guide/cookies.md).

## Adding a video from the command line

`moombox add` works inside the running container and writes to the database
the running Moombox uses:

```bash
docker compose exec moombox moombox -config /data/config.toml add "https://www.youtube.com/watch?v=VIDEO_ID"
```

See [Command line → moombox add](../reference/cli.md#moombox-add).

## Updating the container

```bash
docker compose pull
docker compose up -d
```

Your data stays in `./data`. On the first start of the new version Moombox
migrates the database if needed and records the version change (an
**Update Applied** notification is sent if you have webhooks set up).

In the dashboard, **Check Now** under Settings → Updates still works, and the
update dialog still shows release notes and **Skip this version**. **Update
Now** is disabled there, with a note to pull the new image instead: Moombox
installs updates only for a browser on the Moombox machine itself, and a
container never qualifies. If you want to hear about new releases, turn
`auto_check_updates` back on; it only notifies.

!!! warning "Going back to an older image"
    A newer release may migrate the database. An older image then refuses to
    start against it ("database schema … is newer than this binary
    supports"). Back up `./data/moombox.db` before upgrading if you might want
    to roll back.

## Troubleshooting the container

- **Container restarts in a loop, log says "Startup error".** Moombox exits
  with code 3 when it cannot start (a configuration it cannot parse, a port it
  cannot bind). Fix the cause named in `docker compose logs`; restarting does
  not help.
- **Dashboard returns 403 Forbidden.** You are connecting over global IPv6, or
  from an address that `lan` does not admit. See
  [Network access in a container](#network-access-in-a-container).
- **"failed to update cookie file — rotated session cookies were discarded"
  warnings.** Usually `cookies.txt` is bind-mounted on its own (or read-only).
  Remove that volume line and keep the file in `./data`.
- **Members-only downloads fail after a few days.** The cookie session
  expired. Paste a fresh export in Settings → Cookies.

More in [Troubleshooting](../help/troubleshooting.md).
