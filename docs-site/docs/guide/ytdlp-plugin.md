# yt-dlp plugin

Moombox can lend its PO tokens to [yt-dlp](https://github.com/yt-dlp/yt-dlp).
It installs a small yt-dlp plugin that, whenever yt-dlp needs a YouTube PO
token, asks the Moombox running on the same machine for one. You get working
web-client downloads in yt-dlp without setting up a separate token server.

This is optional. Moombox itself never uses yt-dlp, and nothing in Moombox
depends on the plugin being installed.

## What it does

YouTube requires a **PO token** ("proof of origin") on many requests from its
web player clients, and yt-dlp cannot make one by itself: it relies on a
*PO token provider* plugin. Moombox already makes real PO tokens for its own
downloads with its BotGuard sidecar (see
[YouTube](youtube.md)). The plugin is a
PO token provider named `moombox` that forwards yt-dlp's request to Moombox
and hands the token back.

- It serves yt-dlp's web-family clients, for video-stream, player and
  subtitle tokens.
- yt-dlp passes it the video or session the token must be bound to; Moombox
  mints a token for exactly that binding.
- Before asking for a token it checks that Moombox is reachable (at most once
  a minute, 5-second timeout). If Moombox is not running, yt-dlp prints a
  one-time warning, *Moombox server not reachable at … Make sure Moombox is
  running.*, and carries on without this provider.
- A token request waits up to 20 seconds for Moombox's answer.

You need:

- a yt-dlp version that supports PO token provider plugins;
- Moombox running on the **same machine** as yt-dlp (see
  [Access rules](#access-rules));
- the BotGuard sidecar enabled
  ([`bgutils.use_sidecar`](../configuration/updates-bgutils-twitch.md#bgutilsuse_sidecar),
  on by default; dashboard **Settings → BotGuard → Enable sidecar**). Without
  it Moombox mints no tokens: every request from the plugin is answered with
  an error (*failed to generate PO token* in Moombox's log), and yt-dlp
  reports *Error reaching Moombox POST /get_pot*.

## Installing it

Moombox writes the plugin file into yt-dlp's standard plugin folder for the
user account Moombox runs as:

=== "Windows"

    `%APPDATA%\yt-dlp\plugins\moombox\yt_dlp_plugins\extractor\getpot_moombox.py`

=== "Linux"

    `$XDG_CONFIG_HOME/yt-dlp/plugins/moombox/yt_dlp_plugins/extractor/getpot_moombox.py`,
    or `~/.config/yt-dlp/plugins/…` when `XDG_CONFIG_HOME` is not set.

yt-dlp loads plugins from that folder automatically, so once the file is
there, every yt-dlp run by the same user picks it up.

The file contains Moombox's address: `http://127.0.0.1:<port>` (or
`https://` when [`network.https_enabled`](../configuration/network.md#networkhttps_enabled)
is on), using the port Moombox is actually listening on.

### From the first-run wizard

Both setup wizards offer it: **Install yt-dlp Plugin** in the dashboard's
wizard, **Install yt-dlp plugin** (No / Yes) in the terminal UI's. Both are
off by default. See [First run](../getting-started/first-run.md).

### From the dashboard

Open **Settings → Integrations**. The **yt-dlp PO Token Plugin** card shows:

| Item | Meaning |
|---|---|
| Badge **Not installed** | No plugin file in the folder. |
| Badge **Installed** | The file is there and points at the address Moombox is serving now. |
| Badge **Config mismatch** | The file points at a different port, or at `http` while Moombox serves `https` (or the other way round). A warning says *The installed plugin configuration doesn't match. Click Update to fix.* |
| Badge **Unrecognized file** | A file with the plugin's name exists but Moombox cannot read its address (a truncated or hand-edited file). A warning says *Click Reinstall to replace it.* |
| Badge **Error** | The status could not be read. |
| **Plugin directory:** | yt-dlp's plugin folder on this machine. |
| **Plugin points to:** | The port (and scheme) in the installed file, with *current: …* added when it differs. |

The button below changes with the state:

- **Install** writes the file.
- **Update** (on a mismatch) rewrites it with the current port and scheme.
- **Reinstall** (when it is already correct) writes it again anyway.
- **Refresh** re-reads the status.

A toast confirms the result: *yt-dlp plugin installed successfully*, *yt-dlp
plugin already installed and up to date*, or *Failed to install plugin: …*
with the reason (for example a permission error).

### From the terminal UI

Press ++e++ ++y++ (**yt-dlp Plugin**). The dialog lists:

- **Installed:** `yes` or `not installed`
- **Plugin dir:** the folder, or *— (no standard yt-dlp plugin directory on
  this platform)*
- **Moombox port:** the port Moombox serves, and whether HTTPS is on
- **Plugin points:** the port and HTTPS setting in the installed file
- **Plugin state:** *not recognized — I reinstalls it*, for an unreadable file
- **Mismatch:** *yes — I rewrites it to match*, on a port or scheme mismatch
- **Plugin path:** the folder to give yt-dlp's `--plugin-dirs` option

Keys: ++i++ installs or reinstalls (always rewrites the file), ++r++
refreshes, ++esc++ or ++q++ closes.

## Keeping it in step with Moombox

Moombox does not rewrite the plugin by itself. If you change
[`network.port`](../configuration/network.md#networkport) or turn HTTPS on or
off, the installed file still points at the old address and yt-dlp will warn
that Moombox is not reachable. The card then shows **Config mismatch** (TUI:
**Mismatch: yes**); click **Update** or press ++i++ in ++e++ ++y++.

!!! tip
    If the configured port is already taken when Moombox starts, Moombox
    moves to a nearby free port (the log says *port in use, probing nearby
    ports*). The plugin then points at the wrong port until you click
    **Update**, or you can point yt-dlp at the right address yourself with
    the `base_url` option below.

## Using it with yt-dlp

With the plugin in the standard folder, nothing extra is needed:

```bash
yt-dlp "https://www.youtube.com/watch?v=VIDEO_ID"
```

To confirm yt-dlp sees it, run with `-v` and look for `moombox` among the PO
token providers yt-dlp lists. When yt-dlp asks for a token, Moombox's log
shows *[PotProvider] POT requested* and *[PotProvider] POT generated*.

Other ways to load or point it:

- **A different plugin folder.** The dashboard's **Manual usage** section (and
  the TUI's **Plugin path:** row) gives the folder to pass to yt-dlp:

    ```bash
    yt-dlp --plugin-dirs "/home/me/.config/yt-dlp/plugins/moombox" "URL"
    ```

    You can copy that `moombox` folder to another place, or another user, and
    point `--plugin-dirs` at the copy.

- **A different address.** The plugin reads a `base_url` option, which
  overrides the address written in the file:

    ```bash
    yt-dlp --extractor-args "youtubepot-moombox:base_url=http://127.0.0.1:8080" "URL"
    ```

!!! warning "The *extractor args without a plugin* line"
    The dashboard's **Manual usage** section also shows a command of the form
    `--extractor-args "youtube:player-client=web;po_token=web.gvs+http://127.0.0.1:…/get_pot"`.
    yt-dlp's `po_token` option expects a token itself, not an address, so
    that line does not fetch tokens from Moombox. Use the plugin.

## Access rules

The token endpoint (`/get_pot`) answers **only connections from the same
machine** (127.0.0.1 or ::1). Anything else gets `403` *Forbidden: loopback
only*, whatever your [`network.network_access`](../configuration/network.md#networknetwork_access)
setting. From the same machine no dashboard password is needed.

- It is rate-limited to **10 token requests per minute** per client; beyond
  that Moombox answers `429` with a `Retry-After` header. yt-dlp caches the
  tokens it gets, so normal use stays well under this.
- Requests that carry a browser `Origin` or `Referer` header get the
  dashboard's usual cross-site checks, so a web page open in your browser
  cannot spend the token budget or clear Moombox's token caches.
- With HTTPS on, the plugin talks to `https://127.0.0.1` and skips
  certificate verification for those local calls only, so a self-signed
  certificate works.

!!! note "Docker"
    In a container, **Install** writes the plugin into the container's own
    home folder (`/data/.config/yt-dlp/plugins/…`, on your data volume),
    where a yt-dlp on the host never looks, and yt-dlp on the
    host reaches Moombox through Docker's network rather than over loopback,
    which `/get_pot` refuses. The plugin is therefore practical only when
    yt-dlp runs on the same machine and network namespace as Moombox — for
    example inside the same container, or with host networking and the
    plugin file copied to the host's plugin folder.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| yt-dlp warns *Moombox server not reachable at …* | Moombox is not running, or the plugin points at an old port or scheme. Start Moombox, then check the card for **Config mismatch** and click **Update**. |
| yt-dlp reports *Error reaching Moombox POST /get_pot* | Moombox is up but could not mint a token (its log says *failed to generate PO token*). Check that the BotGuard sidecar is running (no **SIDECAR DOWN** in the TUI status bar or *PO tokens: sidecar down* in the dashboard) and that **Enable sidecar** is on. |
| `403` *Forbidden: loopback only* | yt-dlp is connecting from another machine or container. Run it on the Moombox machine. |
| `429` responses | More than 10 token requests a minute from one client. Slow down, or let yt-dlp's own token cache do its job. |
| Card shows **Unrecognized file** | Click **Reinstall** (TUI: ++i++). |
| *Failed to install plugin: …* | Moombox could not write the folder, for example because of permissions. Fix the folder's permissions or install it as the user yt-dlp runs as. |
