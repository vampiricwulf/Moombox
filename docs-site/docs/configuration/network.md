# Network

The `[network]` section decides where the web dashboard listens, who may
reach it, whether it is served over HTTPS, the dashboard password, how long a
remembered browser stays logged in, and how Moombox behaves behind a reverse
proxy. Read [Security](../reference/security.md) before you open the
dashboard to anything beyond your own machine.

```toml
[network]
port = 774
network_access = "localhost"
https_enabled = false
tls_cert_path = ""                # empty = ./moombox.crt
tls_key_path = ""                 # empty = ./moombox.key
# password_hash = "scrypt:..."    # written by Moombox
client_token_ttl_days = 365
trust_forwarded_proto = false
trusted_proxies = []
public_url = ""
```

!!! info "When changes take effect"
    The listener (port, bind address, HTTPS) is set up once, when Moombox
    starts. [`port`](#networkport), [`https_enabled`](#networkhttps_enabled),
    [`tls_cert_path`](#networktls_cert_path) and
    [`tls_key_path`](#networktls_key_path) therefore need a restart, and so
    does moving [`network_access`](#networknetwork_access) **off**
    `localhost` (the server only starts listening on the network at boot).
    Both settings UIs mark these fields as restart-required: the dashboard
    shows a **Restart** badge and offers to restart after you save, and the
    TUI colours the change marker yellow and shows a restart prompt.

    Everything else on this page — the access check itself, the password,
    the client-token lifetime, the proxy settings and the public URL — is read
    on every request and applies as soon as you save.

    If you edit `config.toml` by hand, stop Moombox first: the file is read at
    start, and a later save from either UI writes the running settings back
    over your edit. An invalid value in a hand-edited file is replaced with
    its default (or, for `public_url`, cleared) at load, and a warning is
    logged.

In the web dashboard these settings are under **Settings → Network**. In the
terminal UI, open Settings with ++grave++ and pick the **Network** section;
the password is managed from the same section (see
[`network.password_hash`](#networkpassword_hash)).

!!! note "Older config files"
    Configs from before the `[network]` section existed are migrated when
    Moombox loads them: top-level `port`, `network_access`, `https_enabled`,
    `tls_cert_path`, `tls_key_path` and `password_hash` move into `[network]`
    (unless `[network]` already sets them), and the old `allow_lan` /
    `allow_external` flags become `network_access` (`external` when both are
    true, `lan` when only `allow_lan` is) unless you also set
    `network_access` explicitly.

## Listener

### `network.port`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `774` | 1–65535 | Restart | Dashboard: Settings → Network → **Dashboard Port**. TUI: Settings → Network → **Port**. |

The TCP port the dashboard, the REST API, the WebSocket and the yt-dlp
plugin's PO-token endpoints all share.

If the port is already taken when Moombox starts, it tries the next ten ports
in turn (`775`, `776`, … `784`) and uses the first one that is free; the log
says `using alternative port`. The configured value in `config.toml` is not
changed, so the next start tries `774` again. If none of the eleven ports can
be bound, the log reports that the port "and nearby ports" are already in
use: without the terminal UI Moombox exits; with it, Moombox keeps running in
TUI-only mode with no dashboard.

When to change it: another program owns 774, you want a familiar port, or you
run several services behind one reverse proxy.

- **Docker**: the port must match the container side of your port publish
  (`774:774`) and the compose file's health check. Change all three together
  (see [Docker](../getting-started/docker.md)).
- **yt-dlp plugin**: the installed plugin points at a fixed port. After a
  port change, reinstall it (see [yt-dlp plugin](../guide/ytdlp-plugin.md)).
- **Linux**: ports below 1024 are privileged on many systems. If an ordinary
  user cannot bind 774 (the log still calls it "already in use"), pick a port
  of 1024 or above.

A value outside 1–65535 is refused by both settings UIs and the API; in a
hand-edited file it is replaced with `774` at load.

After a save in the dashboard, the restart prompt reloads the page at the new
port (and the new scheme, if you also toggled HTTPS).

```toml
[network]
port = 8774
```

### `network.network_access`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `localhost` (the Docker image seeds `lan`) | `localhost`, `lan`, `external`; `public` in `config.toml` only | The access check applies on save; leaving `localhost` needs a restart | Dashboard: Settings → Network → **Network Access**. TUI: Settings → Network → **Network access** (←/→ cycles). Also in both first-run wizards. |

Who may reach the dashboard and API at all, and who must log in.

| Value | Dashboard label | Listens on | Admitted clients | Password asked |
|---|---|---|---|---|
| `localhost` | Localhost Only — This machine only | `127.0.0.1` only | This machine (loopback) | Never |
| `lan` | LAN — Local network devices | All interfaces, IPv4 and IPv6 | Loopback, private ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`), link-local (`169.254.0.0/16`, `fe80::/10`) and `100.64.0.0/10` (carrier-grade NAT, which is what Tailscale uses) | Never |
| `external` | External — All networks (dangerous) | All interfaces, IPv4 and IPv6 | Everyone who can reach the port | Only to clients that are **not** loopback or private, and only when a password is set |
| `public` | *(not offered)* | All interfaces | As `external` | As `external` |

Any other client gets a plain `403 Forbidden`.

`public` behaves exactly like `external`. It exists to label a deployment
that sits behind a reverse proxy which does the authentication: you can only
set it by editing `config.toml`, and the settings UIs and the API refuse it.
While the file holds `public`, the dashboard's dropdown is blank with a note
saying so; saving other settings leaves the value alone, and picking an
option replaces it.

On `external` the carrier-grade NAT range (`100.64.0.0/10`) is **not** treated
as private: a Tailscale client is an internet client there and is asked for
the password.

Rules that apply when you change it:

- **A password is required before `external`.** The dashboard, the TUI, the
  config API and the setup wizards all refuse to switch to `external` while no
  password is set (the dashboard says *"A password must be set before
  enabling external access. Set one in Settings → Network → Password."*). Set
  the password first.
- **Removing the password resets access.** Removing it from the dashboard
  sets `network_access` back to `localhost` in the same save; removing it
  from the TUI does so when access was `external` or `public`.
- **A hand-edited `external`/`public` with no password still starts**, so a
  deployment fronted by an authenticating proxy keeps working, but Moombox
  logs a `SECURITY` warning at every start, and both UIs show a red banner
  (the dashboard's reads *"External access is enabled without a dashboard
  password — anyone who can reach this port has full control."*).
- **Restart behaviour.** The access check is applied to every request as
  soon as you save, but the listener's address is chosen at start. Going
  from `localhost` to `lan` or `external` needs a restart before other
  machines can connect. Going the other way takes effect at once (other
  clients get 403), though the server keeps listening on all interfaces until
  the next restart.

!!! warning "Open the dashboard by IP address or `localhost`"
    On `localhost` and `lan`, a request that names the server by a DNS name
    (`http://myserver:774`) is refused with *"Forbidden: unrecognized host —
    open the dashboard by IP address or localhost"*. This blocks DNS-rebinding
    attacks. While [`https_enabled`](#networkhttps_enabled) is on, a
    certificate you provide whose names include the host name lifts this for
    that name (Moombox's own self-signed certificate does not, and a
    certificate held by a reverse proxy plays no part). On `external`/`public` the same rule applies to clients on this
    machine or your LAN, with the host of [`public_url`](#networkpublic_url)
    also accepted. See [Security](../reference/security.md).

!!! note "Docker"
    The image seeds `lan` because connections through a published port arrive
    over Docker's bridge network and never look like loopback; with
    `localhost` the dashboard would be unreachable. Control exposure with the
    port publish instead (for example `127.0.0.1:774:774` for host-only). On
    Docker Desktop every client appears to come from the same private address.
    See [Docker](../getting-started/docker.md).

```toml
[network]
network_access = "lan"
```

## HTTPS

### `network.https_enabled`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true`, `false` | Restart | Dashboard: Settings → Network → **Enable HTTPS**. TUI: Settings → Network → **HTTPS enabled**. |

Serves the dashboard over HTTPS (TLS 1.2 or newer) using the certificate and
key named by [`tls_cert_path`](#networktls_cert_path) and
[`tls_key_path`](#networktls_key_path).

If either file does not exist when Moombox starts, Moombox **generates a
self-signed certificate** there: an ECDSA P-256 key, valid for about ten
years, naming `localhost`, `127.0.0.1` and `::1` — plus this machine's
current network addresses when `network_access` is `lan`, `external` or
`public` at that moment. The log lists the names (`[TLS] Certificate SANs:
…`). Browsers warn about a self-signed certificate until you accept it. The
certificate is not regenerated when your addresses change: delete both files
and restart to get a fresh one.

HTTP and HTTPS share the one port. With HTTPS on, a plain `http://` request
to the port is redirected (307) to `https://`. With HTTPS off but a
certificate pair still on disk from an earlier HTTPS run, an `https://`
request is redirected to `http://` instead.

Over HTTPS Moombox also sends `Strict-Transport-Security` (one year), and the
login cookies get the `Secure` flag.

If the certificate or key cannot be loaded (a broken file, a key that does not
match), the web server does not start; see [`network.port`](#networkport) for
what Moombox does then.

When to turn it on: the dashboard is reached over a network you do not fully
trust and no reverse proxy terminates TLS for it. With `external`, a password
and HTTPS off, Moombox logs a warning at every start that session cookies
travel unencrypted. Behind a TLS-terminating reverse proxy, leave it off and
use [`trust_forwarded_proto`](#networktrust_forwarded_proto) instead.

!!! tip "yt-dlp plugin"
    The yt-dlp plugin is installed for the scheme in use. Reinstall it after
    turning HTTPS on or off (see [yt-dlp plugin](../guide/ytdlp-plugin.md)).

```toml
[network]
https_enabled = true
```

### `network.tls_cert_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (means `./moombox.crt`) | A file path; no `..` segment | Restart | Dashboard: Settings → Network → **TLS Certificate Path**. TUI: Settings → Network → **TLS cert path**. |

The PEM certificate file used when [`https_enabled`](#networkhttps_enabled)
is on. A relative path is relative to Moombox's working directory (in the
Docker image, `/data`). Include the full chain if your certificate authority
issued intermediates.

When this file and the key both exist, Moombox uses them as they are. It
**reloads the certificate automatically** when the certificate file's
modification time changes — checked on each new TLS connection — so a
renewal (for example by an ACME client) needs no restart. Replace the key
file first and the certificate second: a pair that does not match is skipped
with the warning `[TLS] reload skipped — cert/key pair invalid`, and the
previous certificate stays in use.

The names in a certificate you provide matter while
[`https_enabled`](#networkhttps_enabled) is on: on `localhost`/`lan` they are
names the dashboard may be opened by, and on `external`/`public` a browser
must open the dashboard by one of them (wildcards count) for its changes to
be accepted. The names in Moombox's own self-signed certificate are not used
this way.

A path containing a `..` segment is refused by both settings UIs and the API.

```toml
[network]
https_enabled = true
tls_cert_path = "/etc/moombox/fullchain.pem"
tls_key_path = "/etc/moombox/privkey.pem"
```

### `network.tls_key_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (means `./moombox.key`) | A file path; no `..` segment | Restart | Dashboard: Settings → Network → **TLS Key Path**. TUI: Settings → Network → **TLS key path**. |

The PEM private key matching [`tls_cert_path`](#networktls_cert_path). A
relative path is relative to Moombox's working directory. A key Moombox
generates is written readable by its owner only.

Keep the key out of directories you share or back up to untrusted places.

```toml
[network]
tls_key_path = "./certs/moombox.key"
```

## Login

### `network.password_hash`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (no password) | Written by Moombox; see below | Immediately | Dashboard: Settings → Network → **Password** (**Set Password**, **Remove Password**). TUI: Settings → Network, then press `` ` `` to set or change it and `~` to remove it (from a toggle row, since on a text row the keys are typed into the field). Also in both first-run wizards. |

The dashboard password, stored as a salted scrypt hash
(`scrypt:<salt>:<hash>`). **Moombox writes this key; do not paste a hash into
it.** It is never sent to the dashboard or returned by the API.

The password is only ever asked of clients that are not loopback or private,
and only when [`network_access`](#networknetwork_access) is `external` or
`public`. On `localhost` and `lan` a password can be set but nobody is asked
for it.

Password rules (the same on every surface):

- 8 to 128 characters (counted in bytes, so non-ASCII characters count more
  than once).
- Used exactly as typed. Leading or trailing spaces are kept, with a warning;
  they are not trimmed.
- Changing a password asks for the current one.
- Setting the **first** password from the dashboard works only from this
  machine (a loopback connection) or from an already logged-in session. A LAN
  browser can change or remove an existing password with the current one.

What a change does: setting or changing the password logs out every other
browser and revokes every remembered client (see
[`client_token_ttl_days`](#networkclient_token_ttl_days)); removing it logs
everyone out and resets `network_access` to `localhost` (the TUI resets it
only from `external`/`public`).

**Setting a password by hand** (first password inside Docker, or a forgotten
password): stop Moombox, put the plain password in the file, and start it
again. Moombox notices that the value is not a scrypt hash, hashes it and
writes the hash back (`[Config] Plaintext password detected, converting to
secure hash`), so the plain text sits in the file only until that start. A
plain password longer than 128 characters is hashed but cannot be used to log
in; the log warns about it.

```toml
[network]
password_hash = "correct horse battery staple"   # replaced by its hash at next start
```

To remove a password by hand, stop Moombox and delete the line. If access is
`external` that leaves the dashboard open without a password (Moombox warns,
see [`network_access`](#networknetwork_access)), so lower access too.

### `network.client_token_ttl_days`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `365` | 1–3650 days | Immediately (existing tokens are checked against it on their next use) | Not shown in either settings UI. Edit `config.toml`, or send it with `PUT /api/config`. |

How long a browser that logged in stays remembered.

A successful login gives the browser two cookies: a session cookie that lasts
24 hours (renewed while you keep using it, and lost when Moombox restarts),
and a **client token** that lasts this many days. When the session is gone,
the client token silently starts a new one, so you are not asked for the
password again after a restart or a day away. The token is stored hashed in
the database and listed under **Settings → Network → Connected Clients** in
the dashboard, and under **Manage Client Tokens** (`A K`) in the TUI, where
you can revoke it.

The lifetime is counted from the **login**, not from the last use: after this
many days the browser must log in again however often it was used. An expired
token is refused, deleted, and logged (`expired client token refused and
removed`).

- Lowering the value applies to existing tokens at their next use.
- Raising it does not lengthen tokens already issued: the browser cookie's
  own lifetime was fixed at login.

A value outside 1–3650 is refused by the API and replaced with `365` at load.

```toml
[network]
client_token_ttl_days = 30
```

## Reverse proxies

These three keys matter only when something sits in front of Moombox — a
reverse proxy, a tunnel or a port forward. The
[Security](../reference/security.md) page walks through a full reverse-proxy
setup.

### `network.trust_forwarded_proto`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true`, `false` | Immediately | Dashboard: Settings → Network → **Trust X-Forwarded-Proto header**. TUI: Settings → Network → **Trust forwarded proto**. |

When on, a request carrying `X-Forwarded-Proto: https` is treated as having
arrived over HTTPS even though it reached Moombox as plain HTTP. That sets
the `Secure` flag on the login cookies and tells the origin checks that the
browser used `https://` (so a proxy on port 443 that forwards no port still
works).

Turn it on **only** when Moombox sits behind a TLS-terminating reverse proxy
(nginx, Caddy, Traefik) that removes any `X-Forwarded-Proto` the client sent
before adding its own, and when that proxy is the only way to reach the port.
On a directly reachable Moombox a client can send the header itself.

Listing the proxy in [`trusted_proxies`](#networktrusted_proxies) is also
enough for the origin checks to work when the proxy forwards no port, but only
this setting sets the `Secure` cookie flag.

```toml
[network]
trust_forwarded_proto = true
```

### `network.trusted_proxies`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `[]` (empty) | A list of IP addresses and/or CIDR ranges | Immediately | Dashboard: Settings → Network → **Trusted Proxies** (comma-separated). TUI: Settings → Network → **Trusted proxies** (comma-separated). |

The addresses of reverse proxies you control. For a request that comes
directly from one of them, Moombox reads `X-Forwarded-For` to find the real
client and applies the access check, the password requirement and rate
limiting to **that** client. It also takes the host from `X-Forwarded-Host`
when the proxy sends one. A bare address means exactly that address (`/32`
or `/128`).

While the list is empty, forwarded headers are never trusted. That is safe
for a directly exposed Moombox, but **dangerous behind a proxy**: every
request then appears to come from the proxy's own address, which is usually
loopback or private — so internet traffic passes `lan` and skips the password
on `external`.

- List the narrowest thing that works, ideally one address. Anything inside a
  listed range is trusted to say who the client is, including claiming to be
  loopback.
- Configure the proxy to **append to** (or replace) `X-Forwarded-For`, never
  to pass the client's header through unchanged. Moombox takes the rightmost
  address that is not itself a listed proxy, which is only safe if the proxy
  writes what it saw at the end. In nginx use
  `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`; Caddy and
  Traefik append by default.
- Declare the address Moombox actually sees. A proxy on the Docker host
  reaching a published port arrives from the bridge gateway (often
  `172.17.0.1`); a proxy container on a shared Docker network arrives from its
  own container address; a proxy on the same machine as a native Moombox
  arrives from `127.0.0.1`. While the list is empty, the `[Auth]` login lines
  in the log show the direct peer — the value to declare.
- With a proxy resolving real clients, use `external` (or `public`), not
  `lan`: the real client is an internet address and `lan` would refuse it.

Some actions are limited to this machine — first-time password setup from
the dashboard, the first-run wizard's final step, opening a job's folder,
applying an update, the cookie login browser, and the PO-token endpoints.
These ignore this list and look only at the direct connection.

An entry that is not a valid IP or CIDR is refused by both settings UIs and the
API; in a hand-edited file it is dropped at load with a warning. Blank entries
are ignored.

```toml
[network]
network_access = "external"
trusted_proxies = ["172.18.0.2"]
```

### `network.public_url`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (unset) | An absolute `http://` or `https://` URL with a host; no query, fragment or `user:password@` | Immediately | Dashboard: Settings → Network → **Public Dashboard URL**. TUI: Settings → Network → **Public dashboard URL**. |

The address you type into a browser to reach this dashboard, for example
`https://moombox.example.com` or `http://192.168.1.10:774`. Moombox never
listens on it, never checks that it works and never redirects to it; it is
only told what the outside address is. It is used in two ways:

- **Notification links.** Each webhook notification's title links to
  `{public_url}/#job=<id>`, which opens that job's details in the dashboard,
  and the YouTube or Twitch page moves to the embed's author line. Unset,
  the title links to YouTube or Twitch as before. See
  [Notifications](../guide/notifications.md).
- **Trusted address for the dashboard itself.**
    - On `localhost`/`lan`, a page served on the public URL's **port** may
      drive the dashboard, as long as its address is one the mode already
      accepts (loopback or `localhost`, and on `lan` also a private address).
      Set it when a reverse proxy or port forward listens on a different port
      than Moombox.
    - On `external`/`public`, browsers on this machine or your LAN may open
      the dashboard by the public URL's **host name** (split DNS or hairpin
      NAT through your proxy). Without it they get
      `403 unrecognized host` while internet clients get in.

The value is tidied on save: the scheme is lowercased and a trailing slash is
removed. Anything containing `@`, a query (`?…`) or a fragment (`#…`), or
using another scheme, is refused by both settings UIs and the API; in a
hand-edited file it is cleared at load with a warning (with any
`user:password@` part cut out of the message).

```toml
[network]
public_url = "https://moombox.example.com"
```

## Outbound proxy

Everything above is about connections coming **in** to the dashboard. If the
machine running Moombox can only reach the internet through a forward proxy
(a corporate or school proxy, or a filtering proxy on your network), there is
no `config.toml` key for it: Moombox reads the standard proxy environment
variables instead.

- `HTTPS_PROXY` is used for `https://` addresses — which is almost all of
  Moombox's traffic: YouTube and Twitch requests and segment downloads, update
  checks against GitHub, and Discord notifications.
- `HTTP_PROXY` is used for plain `http://` addresses only.
- `NO_PROXY` lists hosts (or IP ranges) that go direct. Requests to
  `localhost` and loopback addresses never go through the proxy.
- Lowercase spellings (`https_proxy`, …) work too; `ALL_PROXY` is not read.
  A value without a scheme, such as `proxy.corp:3128`, is read as `http://`.

The BotGuard helper (the sidecar that mints PO tokens) is given the same HTTPS
proxy and `NO_PROXY`, so its tokens come from the same address as the
downloads that use them. It cannot use a `socks5://` proxy: with one, the
helper connects directly and the log says so. Behind a proxy that intercepts
TLS, also point `NODE_EXTRA_CA_CERTS` at the proxy's CA certificate, because
the helper does not read the system certificate store.

Set the variables in the environment Moombox starts from: your shell or
service unit on Linux, the user or system environment variables on Windows,
or `environment:` in Docker Compose. Changes need a restart. The full list is
in the [environment variables table](../reference/cli.md#environment-variables).

```bash
export HTTPS_PROXY=http://proxy.example.lan:3128
export NO_PROXY=nas.example.lan,192.168.0.0/16
./moombox
```

!!! warning "The connectivity probe does not use the proxy"
    Moombox decides whether it is online by opening a direct TCP connection to
    each of [`connectivity.probe_targets`](disk-memory-connectivity.md#connectivityprobe_targets).
    On a network where only the proxy can reach the internet, every probe
    fails, Moombox believes it is offline the whole time and holds back work
    accordingly. Set `probe_targets` to something reachable directly — usually
    the proxy's own `host:port`, provided the proxy runs on another machine
    (loopback targets are refused).
