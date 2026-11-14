# Security

Anyone who can use the Moombox dashboard has full control of it: they can add
and delete jobs, change every setting (including the output and FFmpeg
paths), read your notification webhook URLs, import cookies, and restart
Moombox. This page explains what stands between the dashboard and the people
you did not mean to give that to, and how to open it up safely.

The settings themselves are documented in [Network](../configuration/network.md).

## The short version

- **Default: this machine only.** A fresh install listens on `127.0.0.1` and
  answers only local requests. No password is needed or asked.
- **`lan` trusts your network.** Every device on a private network can use the
  dashboard without a password. Use it only on a network where you trust every
  device.
- **Remote access, strongest first:** a VPN that gives you a private address
  (WireGuard, Tailscale) with `lan`; a reverse proxy with HTTPS in front of
  `external`; direct `external` exposure with a password and HTTPS.
- **Never** run `external` without a password unless an authenticating proxy is
  the only route to the port.

## Who can reach the dashboard

[`network.network_access`](../configuration/network.md#networknetwork_access)
is the outer boundary. Every HTTP request and every WebSocket connection is
checked against it before anything else happens.

| Mode | Listens on | Who gets in | Who must log in |
|---|---|---|---|
| `localhost` | `127.0.0.1` | This machine | Nobody |
| `lan` | all interfaces (IPv4 and IPv6) | Loopback, private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), link-local, and `100.64.0.0/10` | Nobody |
| `external` / `public` | all interfaces | Everyone | Clients that are not loopback or private (link-local counts as private here; `100.64.0.0/10` does not), when a password is set |

Refused clients get `403 Forbidden`.

Things to know:

- **Loopback and private clients never log in**, on any mode. On `external`, a
  browser on your LAN reaches the dashboard without the password while one on
  the internet is asked for it.
- **Tailscale** addresses (`100.x.y.z`) are inside `100.64.0.0/10`, which `lan`
  admits. Some internet providers hand out addresses from the same range; if
  the Moombox host sits behind such a provider's NAT, its other customers may
  reach a `lan` dashboard too. Limit the port to the tailnet interface with a
  host firewall in that case. On `external`, a Tailscale client is treated as
  an internet client and asked for the password.
- **IPv6.** On `lan`, a client with a global IPv6 address is refused even if it
  is in your home. Reach the dashboard over IPv4 or a local IPv6 address.
- **Docker.** Connections through a published port are never loopback, which
  is why the image seeds `lan`. Restrict the publish itself
  (`127.0.0.1:774:774` for host-only): published ports bypass `ufw` and
  `firewalld`, and Docker Desktop makes every client appear to come from one
  private address. See [Docker](../getting-started/docker.md).

### Open it by IP address or `localhost`

On `localhost` and `lan`, the dashboard answers only requests that name it by
an IP address (loopback, or private on `lan`) or by `localhost`. A request
that uses a DNS name gets *"Forbidden: unrecognized host — open the dashboard
by IP address or localhost"*. This stops a malicious web page from reaching
your dashboard through **DNS rebinding** (pointing its own domain name at your
machine). While Moombox itself serves HTTPS, a certificate you provide (see
[HTTPS](#https)) lifts this for the names it contains.

On `external` and `public` the same rule applies to the clients that skip the
password (this machine and your LAN). They may also use the host name of
[`network.public_url`](../configuration/network.md#networkpublic_url) or any
name, wildcards included, in a certificate you provide (again only while
Moombox serves HTTPS itself). Otherwise they get
*"…open the dashboard by IP address, localhost or network.public_url"*.

### Pages on other ports cannot drive it

On `localhost` and `lan`, only pages served on the dashboard's **own port**
may make changes. A page served by another program on the same address
(a dev server on `127.0.0.1:3000`, a router admin page) cannot. An address
with no port means the scheme's default (`80` for `http://`, `443` for
`https://`). The port of `network.public_url` is accepted as well, which is
how a proxy or port forward on another port keeps working.

## Password and login

The dashboard password is set in **Settings → Network → Password** (dashboard),
in the TUI's Settings → Network section, or in either first-run wizard. It is
stored as a salted scrypt hash in
[`network.password_hash`](../configuration/network.md#networkpassword_hash)
and never leaves the server.

- **Length:** 8 to 128 characters. Used exactly as typed; leading or trailing
  spaces are kept (with a warning).
- **External needs a password first.** Every settings surface and the config
  API refuse to switch to `external` while no password is set.
- **First password from this machine.** Setting the first password from the
  dashboard requires a loopback connection. Inside Docker, where no
  connection is loopback, put the plain password in `config.toml` and
  restart; Moombox hashes it on start.
- **Changing or removing** asks for the current password and works from this
  machine, the LAN, or a logged-in session.
- **Effects:** setting or changing the password logs out every other session
  and revokes every remembered client; removing it logs everyone out and puts
  `network_access` back to `localhost` (from the TUI, only when it was
  `external`/`public`).
- **Forgotten password:** stop Moombox, replace the `password_hash` value in
  `config.toml` with a new plain password, and start it again.

What a remote browser sees: when a password is required, any page it asks for
is answered with the login page (the address bar is kept), and any `/api/`
request gets `401 {"error":"Authentication required"}`. A small set of paths
stays open so the login page can load and the yt-dlp plugin can find
Moombox: the login page and its two scripts, the favicon,
`/api/auth/status`, `/api/auth/login`, `/ping` and `/minter_cache` (the last
is loopback-only anyway).

Once logged in, the status bar shows a logout button; it ends the session and
revokes the browser's client token.

### Sessions and remembered clients

A successful login sets two cookies, both `HttpOnly` and `SameSite=Lax`, and
`Secure` when the request came over HTTPS (or through a proxy trusted with
[`trust_forwarded_proto`](../configuration/network.md#networktrust_forwarded_proto)):

| Cookie | Lifetime | Stored on the server |
|---|---|---|
| `moombox_session` | 24 hours, renewed while you keep using it | In memory only; lost on restart |
| `moombox_client` | [`network.client_token_ttl_days`](../configuration/network.md#networkclient_token_ttl_days) (default 365), counted from the login | As a salted scrypt hash in the database |

When the session is missing or expired, a valid client token starts a new one
without asking for the password. Each browser holds one token: logging in
again replaces the previous one.

Remembered clients are listed in the dashboard under **Settings → Network →
Connected Clients**, and in the TUI with **Manage Client Tokens** (`A K`;
`D` twice revokes, `R` refreshes). Each entry shows a label built from the
browser's user agent and the address it logged in from, when it was created,
when it was last used, and its last address (shortened to its `/24` for
IPv4 or `/64` for IPv6). Revoking a token makes that browser log in again.

### Login rate limits

Password attempts are limited per client address:

| What | Limit |
|---|---|
| Login | 5 per minute |
| Set password and remove password (together) | 3 per minute |

Over the limit the answer is `429 Too Many Requests` with a `Retry-After`
header. Global IPv6 addresses are counted per `/64`, so one host cannot dodge
the limit by rotating addresses. Behind a reverse proxy, the limits apply to
the real client only when the proxy is listed in
[`trusted_proxies`](../configuration/network.md#networktrusted_proxies);
otherwise every client shares the proxy's budget.

Every failed login is logged at Warn as `[Auth] Failed login attempt from`
followed by the client's address, and every successful one at Info, so you can
watch the log for guessing.

Other limits per client address: adding a job, YouTube format lookups,
resolving a channel URL or `@handle` (including adding a channel given as
one), starting a trim, the FFmpeg check/install actions and
the heavier cookie actions share a budget of 20 per minute; uploading an
import is limited to 5 per minute; the yt-dlp plugin's PO-token requests to
10 per minute.

## HTTPS

Turn on [`network.https_enabled`](../configuration/network.md#networkhttps_enabled)
to encrypt the dashboard, or let a reverse proxy do it.

- **Self-signed:** with no certificate files present, Moombox creates a
  self-signed one at start (ECDSA P-256, about ten years) naming `localhost`,
  `127.0.0.1`, `::1`, and on `lan`, `external` or `public` this machine's network addresses
  at that moment. Browsers warn about it until you accept it. Its names are
  not trusted as dashboard host names.
- **Your own certificate:** point
  [`tls_cert_path`](../configuration/network.md#networktls_cert_path) and
  [`tls_key_path`](../configuration/network.md#networktls_key_path) at PEM
  files. A renewed certificate on disk is picked up without a restart. The
  certificate's DNS names and IP addresses become names browsers may open the
  dashboard by; on `external` a browser must use one of them for its changes
  to be accepted, so include every name and address you use. This applies
  only to a certificate Moombox serves itself: one held by a reverse proxy
  in front of Moombox plays no part.
- TLS 1.2 is the minimum, with modern ECDHE cipher suites only.
- Over TLS Moombox sends `Strict-Transport-Security` (one year, no
  subdomains). Plain `http://` requests on the HTTPS port are redirected to
  `https://`.
- With `external`, a password and HTTPS off, Moombox logs a warning at every
  start that session cookies travel unencrypted.

## Behind a reverse proxy

Terminate TLS at nginx, Caddy or Traefik and forward to Moombox:

```toml
[network]
network_access = "external"
trusted_proxies = ["172.18.0.2"]            # the address Moombox sees the proxy as
trust_forwarded_proto = true                # the proxy terminates TLS
public_url = "https://moombox.example.com"  # the address browsers type
```

- [`trusted_proxies`](../configuration/network.md#networktrusted_proxies) is
  what makes this safe. Without it, every request seems to come from the
  proxy's private or loopback address, so internet traffic would pass the
  access check and skip the password. With it, Moombox reads
  `X-Forwarded-For` from that peer only and judges the real client. List the
  narrowest address you can, and make the proxy append to `X-Forwarded-For`
  rather than pass the client's header through.
- [`trust_forwarded_proto`](../configuration/network.md#networktrust_forwarded_proto)
  makes the login cookies `Secure` and lets the origin checks know the browser
  used HTTPS. Enable it only when the proxy overwrites the client's
  `X-Forwarded-Proto`.
- [`public_url`](../configuration/network.md#networkpublic_url) lets browsers
  on your own network reach the dashboard by its public name, and makes
  notification embeds link to it.
- **nginx on a non-default port:** forward the port with the host —
  `$http_host` or `$host:$server_port`, not `$host`, which drops it —
  or the browser's changes are refused with 403. Caddy and Traefik forward
  the browser's own `Host` by default.
- **Make the proxy the only route.** Publish Moombox's port on `127.0.0.1`
  only, or keep proxy and Moombox on a private Docker network without
  publishing the port. A directly reachable port defeats both the proxy's
  authentication and `trusted_proxies`.

Then pick who checks the password:

- **Moombox:** set a dashboard password and use `external`.
- **The proxy:** set `network_access = "public"` in `config.toml`. It works
  exactly like `external` and can only be set by hand. With no Moombox
  password, Moombox logs a `SECURITY` warning at start and both UIs show a red
  banner, because it cannot verify that your proxy really authenticates.
  Setting a Moombox password as well removes the warning and adds a second
  lock.

If changes from the browser fail with a 403 behind a proxy, look in the log
for `CSRF: origin refused`: it names the origin and host that were compared.
The usual cause is a proxy that rewrites `Host` without being listed in
`trusted_proxies`.

## Built-in protections

These need no configuration.

Cross-site request forgery
:   Every request that changes something (`POST`, `PUT`, `DELETE`) must carry an
    `Origin` or `Referer` that names the dashboard itself. A request with
    neither gets `403 Forbidden: missing origin`; one that names another site
    gets `403 Forbidden: invalid origin` and a `CSRF: origin refused` log line.
    No cross-origin access is granted to other sites. The TUI talks to Moombox
    in-process and is not affected. Scripts must send an `Origin` header (see
    [API](api.md)).

Cross-site reads that act
:   A few `GET` requests do work (looking up a video's formats on YouTube with
    your cookies, running FFmpeg, fetching release notes, the setup status).
    The browser marks a request started by another site's page, and those are
    refused with `403 Forbidden: cross-site request`.

Security headers
:   Every response carries `X-Frame-Options: DENY`,
    `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, a
    `Permissions-Policy` that turns off camera, microphone, location and
    motion sensors, and a Content Security Policy. The policy allows scripts
    only from the dashboard and `cdn.jsdelivr.net` (where the Shoelace UI
    library is loaded from), images from the dashboard and the YouTube,
    Twitch and emote CDNs, and embedded players only from
    `youtube-nocookie.com` and `player.twitch.tv`. The dashboard's browser
    therefore needs to reach `cdn.jsdelivr.net`.

WebSocket
:   The live-update connection goes through the same access check and host rule
    as every other request, needs the same login cookies when a password is
    required, and is refused (and logged as `websocket upgrade rejected:
    origin refused`) when its `Origin` is not the dashboard. It accepts
    messages of at most 4 KiB from the browser.

Request sizes
:   Request bodies are limited to 1 MB, except the import upload, which is
    limited to 500 MB.

Slow clients
:   A client gets 30 seconds to send its request headers.

## Actions limited to this machine

Some actions are allowed only over a direct loopback connection, whatever
`network_access` says. They check the actual connection and ignore
`trusted_proxies`.

| Action | Why |
|---|---|
| Setting the **first** dashboard password (unless already logged in) | Nobody else can claim the dashboard first |
| Finishing the first-run setup wizard | Same; it can also set the password |
| The cookie login browser (start, finish, cancel) | It opens a browser on this machine's desktop |
| **Open Folder** for a job | It opens a file manager on this machine |
| **Update Now** (applying an update); the page's address must also be `localhost`, `127.0.0.1` or `::1` | Replacing the binary is high-impact |
| The yt-dlp plugin's PO-token endpoints (`/get_pot`, `/invalidate_caches`, `/invalidate_it`, `/minter_cache`, `/pot_stats`) | They mint tokens with your session |

In Docker none of these work through a published port, because no connection
there is loopback. That is why the first password is set in `config.toml`
instead, and why the image is updated by pulling a new image.

!!! warning "A proxy on the same machine"
    If your reverse proxy runs on the same machine as Moombox and connects to
    it over `127.0.0.1`, every proxied request is a loopback connection to
    Moombox. Remote users who get through the proxy (and past the password)
    can then use the actions in this table too.

## What Moombox keeps out of logs

Where Moombox writes these values into a log line, a job's error text or a
notification, it replaces the sensitive part with `<redacted>` first:

- The signed parts of YouTube media URLs (your public IP, signatures, PO
  tokens) and the playback session in Twitch stream URLs.
- PO tokens in crash output (which also cuts every URL to its scheme and
  host).
- Notification webhook URLs, which are cut down to the scheme and host (for
  example `https://discord.com/…<redacted>`).
- A `user:password@` part of `network.public_url` in warnings about it.

Importing a cookie file from the TUI shows or logs only its path, never its
contents.

The **Connected Clients** list shortens each last-seen address. The dashboard
password is never returned by the API.

What is not hidden: client addresses in login log lines, job titles, channel
names and file paths. And anyone who can use the dashboard can see the full
notification webhook URLs in Settings, and the cookie status.

## Cookie files

Your YouTube and Twitch cookies are the most valuable secret Moombox holds:
they are your signed-in sessions.

- Moombox writes the cookie file readable by its owner only (`0600` on Linux).
  On Windows it also restricts the folder that holds it to your user. On
  Linux it tightens that folder to owner-only too, unless the folder also
  holds the output, staging, database or log (as the Docker image's `/data`
  does).
- The paste or upload import in **Settings → Cookies** accepts up to 512 KiB,
  merges into the cookie file, and rolls back if the result does not verify.
- On Windows, the optional DPAPI fallback reads your real browser's cookies,
  that is your own signed-in session. It is off unless you turn it on.

See [Cookies](../guide/cookies.md) for every way to get cookies into Moombox.

## One instance at a time

Moombox refuses to start a second copy, so two processes never fight over the
same database, staging files and port.

=== "Windows"

    One Moombox per Windows session, plus a lock file at
    `%LOCALAPPDATA%\moombox\moombox.lock` that stops a second copy for the same
    user in another session. A crashed process releases both.

=== "Linux"

    A lock file at `~/.local/share/moombox/moombox.lock` (`/tmp` when `HOME`
    is not set). The error says *"another moombox instance is already running,
    or the last one is still shutting down"* with the lock path.

## Recommendations

1. **Stay on `localhost`** unless you need more.
2. **Use a VPN** for remote access when you can. With WireGuard or Tailscale
   your devices get private addresses, `lan` admits them, and no port is open
   to the internet.
3. **Use `lan` only on a network you trust.** Everyone on it has full control
   and no password is asked.
4. **For internet exposure**, put a reverse proxy with HTTPS in front, set
   `trusted_proxies`, `trust_forwarded_proto` and `public_url`, set a
   password, and make the proxy the only way to the port.
5. **If you expose Moombox directly**, set the password first, then
   `external`, then `https_enabled`, ideally with a real certificate.
6. **Review Connected Clients** now and then and revoke what you do not
   recognise; lower
   [`client_token_ttl_days`](../configuration/network.md#networkclient_token_ttl_days)
   if a year is too long for your taste.
7. **Keep `config.toml` private.** It holds your webhook URLs and the password
   hash.
