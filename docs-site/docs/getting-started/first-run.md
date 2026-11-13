# First run

The first time Moombox starts and finds no configuration file, it opens a
setup wizard. The wizard writes `config.toml`, restarts Moombox with it, and
from then on Moombox starts straight into the dashboard.

The same wizard exists in two places, and both are live at once:

- in the **web dashboard**, which Moombox tries to open in your default browser
  at `http://localhost:774` when it starts;
- in the **terminal UI**, when Moombox was started in an interactive terminal.

Finish it in whichever you prefer. Finishing in either one saves the
configuration and restarts Moombox; if the other one is still on screen
afterwards, reload the page.

!!! note "Docker skips the wizard"
    The Docker image writes a starter `config.toml` before Moombox first
    starts, so no wizard appears. Everything the wizard asks is in the
    dashboard's Settings tab. See [Docker](docker.md).

## Choosing a mode

The first screen, **Welcome to Moombox**, offers three choices and shows
whether FFmpeg was found:

**Quick Setup** (recommended)
:   Two steps: sign in to YouTube and/or Twitch, then add channels. Every
    other setting keeps its default.

**Advanced Setup**
:   Walks through network, paths, logs, monitors, downloads, cookies,
    channels and integrations, eight steps in all.

**Use Defaults** (the dashboard labels it **Use Defaults & Skip Setup**)
:   Saves a configuration with every default and restarts at once. You can
    change anything later in Settings.

If the welcome screen says FFmpeg was not found, you can still finish setup;
you are asked to install it right after (see [After setup](#after-setup)).

!!! warning "The dashboard wizard only finishes from the Moombox machine"
    Saving the wizard from the web dashboard is accepted only from a browser
    on the same computer (a loopback connection). With the defaults the
    dashboard only listens on `127.0.0.1` anyway. If you set Moombox up on a
    server, see [Setting up a headless server](#setting-up-a-headless-server).

## Quick Setup

### Step 1: Cookies

Signing in lets Moombox archive members-only, age-restricted and
subscriber-only content. You can skip this step and do it later.

Select **Set up YouTube** or **Set up Twitch**. Moombox opens a browser window
**on the computer Moombox runs on**, at the Google or Twitch sign-in page:

1. Sign in in that window.
2. Back in the dashboard, select **I'm Logged In** (in the terminal UI, press
   ++enter++).
3. Moombox reads the cookies from that browser, checks that they really
   authenticate, and writes them to the cookie file. The platform gets a **Done** badge (a ✓ in the terminal UI).

Select **Cancel** (or ++esc++) to abandon a sign-in. In the terminal UI a
countdown shows how long Moombox will keep waiting for you (300 seconds). If
it runs out, press ++r++ to try again or ++s++ to skip.

Moombox needs a supported browser installed on that computer: Firefox, Chrome,
Brave, Edge, Opera, Waterfox, LibreWolf, Zen, Vivaldi or Thorium. Without one,
the step reports "no supported browser installed"; skip it and use one of the
other sign-in methods afterwards.

When at least one platform succeeds, setup also turns on automatic cookie
refresh ([`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled))
for the platforms you signed in to
([`cookies.active_platforms`](../configuration/cookies.md#cookiesactive_platforms)).
Everything about sign-in, including pasting a cookie file on a headless host,
is in [Cookies and sign-in](../guide/cookies.md).

### Step 2: Channels

Add the YouTube or Twitch channels Moombox should watch for live streams.
Select **Add Channel** and fill in:

| Field | What to enter |
|---|---|
| **Channel ID** | A YouTube channel ID (`UC…`), an `@handle`, or a channel URL. A Twitch channel URL or login name works too. URLs and handles are looked up when you save, and the display name is filled in if you left it empty. |
| **Display Name** | Optional; how the channel is shown. |
| **Platform** | YouTube or Twitch. Typing a YouTube or Twitch URL switches it for you. |
| **Filter Regex** | Optional. Only streams whose title matches are archived, for example `(?i)karaoke`. Leave empty to archive everything. |
| **Also archive uploads & premieres** | YouTube only. Off by default: only live streams are archived. |
| **Quality Preference** | `Best available` by default, or a specific resolution, or `Audio only`. |
| **Enabled** | Untick to save the channel without monitoring it yet. |

Each channel appears in the list with edit and remove buttons. Adding the
same channel twice is refused. Select **Finish Setup** when you are done; an
empty list is fine.

The terminal UI's channel editor offers every per-channel setting, including
the per-channel output directory, archive window, archive slots and the Twitch
options. The dashboard wizard keeps to the fields above; the rest are in
Settings → Channels afterwards. All of them are explained in
[Channels and monitoring](../guide/channels.md).

## Advanced Setup

Every field can be left empty to keep its default. The dashboard shows the
eight steps across the top; **Back** and **Next** move between them.

### 1. Network

| Field | Default | Notes |
|---|---|---|
| **Dashboard Port** | `774` | 1–65535. If the port is taken when Moombox starts, it tries the next ten ports and uses the first free one. |
| **Enable HTTPS** | off | Serves the dashboard over HTTPS with an automatically generated self-signed certificate. Your browser will warn about the certificate the first time. |
| **Network Access** | Localhost Only | Who may open the dashboard: **Localhost Only**, **LAN** (private addresses), or **External (dangerous)**. |
| **External Access Password** | — | Shown only for External, and required for it: 8 to 128 characters. Spaces at either end are kept as typed (the field warns you). |

See [`network.network_access`](../configuration/network.md#networknetwork_access)
and [Security](../reference/security.md) before choosing anything other than
Localhost.

### 2. Paths

| Field | Default |
|---|---|
| **Output Directory** — where finished archives go | `./output` |
| **Filename Template** — with a live preview below it | `${channel}/${start_date} ${title} [${id}]` |
| **Staging Directory** — segments of downloads in progress | `./staging` |
| **Database Path** | `./moombox.db` |
| **FFmpeg Path** — empty means FFmpeg from your `PATH` | *(system PATH)* |

Template variables are `${title}`, `${id}`, `${channel}`, `${start_date}` and
`${start_time}`. Paths may be absolute or relative (relative to the folder
Moombox is started from). A path wrapped in quotes or containing a `..`
segment is refused. The output and staging directories are created when you
finish; if one cannot be created, the wizard says which and why.

### 3. Logs

| Field | Default | Range |
|---|---|---|
| **Log Level** | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| **Log File Path** | `./moombox.log` | |
| **Max Log File Size (bytes)** | `10485760` (10 MB) | 1024 – 1073741824 |
| **Max Log Files** | `5` | 1 – 100 |

### 4. Monitors

| Field | Default | Range | Meaning |
|---|---|---|---|
| **Hide Finished After (days)** | `30` | 0 – 365 | Finished jobs older than this move to the Archived tab; 0 moves them at once. |
| **Archive Window (days)** | `3` | 1 – 3650 | How far back a channel's past videos are archived. Upcoming and live streams are always covered. |
| **Archive Slots** | `3` | 1 – 100 | Backlog downloads per channel at once. New content never waits for a slot. |
| **Feed Check Interval (minutes)** | `10` | 1 – 1440 | How often channel feeds are checked. Fractions such as `2.5` are accepted. |

### 5. Download

| Field | Default | Notes |
|---|---|---|
| **Max Resolution** | 4K (2160) | Caps the *shorter* edge, so 2160 means 4K in portrait or landscape. Presets from 480p to 8K, **Unbounded**, or **Custom…** for any number (0 = unbounded). |
| **Prefer 60fps** | on | At the same resolution, prefer 60 fps. Resolution always wins. |
| **Parallel Downloads** | `10` | Peak concurrent VOD downloads across all channels. Live streams always start immediately and never wait on this. |
| **Download Live Chat** | on | Saves the chat alongside the video, for replay in the player. |
| **YouTube Maximum Timeout (seconds)** | `600` | Under *Advanced retry settings*. How long to keep retrying a stalled YouTube live stream before finalising it, even if YouTube still says it is live. At least 30. |

### 6. Cookies

The same **Set up YouTube** / **Set up Twitch** sign-in as in Quick Setup,
plus **Cookie File (manual)** (default `./cookies.txt`): the Netscape-format
cookie file Moombox reads and keeps up to date. If you already have an
exported `cookies.txt`, put its path here.

### 7. Channels

The same channel list as in Quick Setup.

### 8. Integrations

**Install yt-dlp Plugin** (off by default) installs a yt-dlp plugin that lets
yt-dlp ask Moombox for proof-of-origin tokens. It is installed before the
restart. See [yt-dlp plugin](../guide/ytdlp-plugin.md).

Select **Finish Setup** to save.

## The terminal UI wizard

The terminal wizard asks the same questions, with a few differences in order
and keys.

| Screen | Keys |
|---|---|
| Mode choice | ++up++ / ++down++ to choose, ++enter++ to continue. |
| Cookie step | ++up++ / ++down++ between **YouTube**, **Twitch** and **Skip / Next**; ++enter++ to start. While a browser is open: ++enter++ when you have signed in, ++esc++ to cancel. ++esc++ on the list goes back. |
| Channel list | ++a++ add, ++enter++ edit, ++d++ delete, ++tab++ finish setup, ++esc++ back. |
| Channel editor | ++up++ / ++down++ (or ++tab++) between fields, ++left++ / ++right++ to cycle a choice, ++enter++ to save, ++esc++ to cancel. |
| Advanced form | Fill in each group as prompted. Press ++esc++ twice to abandon it and return to the mode choice. |

In the Advanced flow the terminal asks for the settings first (Network, Paths,
Logs, Monitors, Downloader, Cookies, Integrations), then runs the browser
sign-in step, then the channel list.

## What happens when you finish

1. The configuration is validated. A mistake is reported against the field
   (in the dashboard, as a message naming the setting) and nothing is saved.
2. `config.toml` is written. Where it goes is decided as described in
   [Command line → Configuration file](../reference/cli.md#configuration-file);
   with no `-config` flag it is `config.toml` in the working directory.
3. If you chose to, the yt-dlp plugin is installed.
4. Moombox restarts itself with the new configuration. The dashboard shows
   **Restarting Moombox...** and reconnects on its own; if the server is not
   back within two minutes it offers **Reload Page**.
5. If you changed the port or turned HTTPS on, the old address stops working.
   The dashboard shows **Dashboard address has changed** with the new address
   and redirects you there after a few seconds (or select
   **Open New Dashboard**).

## After setup

If FFmpeg cannot be found, the **FFmpeg Not Found** screen appears next (and
on any later start where it is still missing):

- **Install FFmpeg** — on Windows, installs it through Chocolatey or winget;
  on Linux, shows the command for your distribution. See
  [Installation → Install FFmpeg](installation.md#install-ffmpeg).
- **Custom FFmpeg path** — type the full path to `ffmpeg` and select
  **Check**. A working FFmpeg is saved to the configuration at once.
- **Skip for now** — continue to the dashboard. Recordings will fail at the
  muxing step until FFmpeg is available.
- **Close this tab** (dashboard) or **Quit Moombox** (terminal UI). Closing
  the tab does not stop Moombox; quitting from the terminal does.

Then the dashboard opens. A fresh install shows **Setup complete!** with
**Add Channels** and **Add Video** buttons. Good next steps:

- Add more channels: [Channels and monitoring](../guide/channels.md).
- Archive a single video or stream now: [Jobs](../guide/jobs.md).
- Get alerts in Discord: [Notifications](../guide/notifications.md).
- Learn the dashboard and the terminal UI:
  [Web dashboard](../guide/web-dashboard.md),
  [Terminal UI](../guide/terminal-ui.md). In the terminal UI, ++o++ then ++w++
  opens the dashboard in your browser.

## Setting up a headless server

On a machine without a desktop, the browser sign-in step cannot work (there
is no browser for you to sign in to), and the dashboard wizard refuses to
finish from another computer ("first-time setup must be from localhost").
Choose one of these:

=== "Wizard over SSH"

    Start Moombox from an SSH session. It runs the terminal UI in your
    terminal, wizard included. Skip the cookie step, finish, then quit with
    ++q++ ++q++ and start Moombox the way you will run it permanently (for
    example as a [systemd service](installation.md#running-in-the-background-on-linux)).

=== "SSH port forward"

    Forward the dashboard port and use the dashboard wizard from your own
    browser. To Moombox the connection arrives from the server itself, so the
    wizard is allowed to finish:

    ```bash
    ssh -L 774:localhost:774 you@server
    # then open http://localhost:774 on your own computer
    ```

    Remember to choose **LAN** network access (Advanced Setup, step 1) if you
    want to reach the dashboard without the tunnel afterwards.

=== "Write the configuration yourself"

    Create `config.toml` before the first start, for example by copying
    `config.example.toml` from the repository and editing it. When a
    configuration file exists, no wizard runs. The settings are documented
    under [Configuration](../configuration/index.md).

For signing in on a server afterwards, paste or upload a cookie file in
Settings → Cookies, or import one in the terminal UI with ++e++ ++i++. See
[Cookies and sign-in](../guide/cookies.md).

## Running the wizard again

The wizard runs only while no configuration file is found. To start over,
stop Moombox and move `config.toml` aside (keep a copy: it holds your channels
and any dashboard password). Your database, archives and cookies are left
alone. To sign in again without redoing setup, use Settings → Cookies in the
dashboard or ++e++ ++l++ (Cookie Login) in the terminal UI, which opens the
wizard's cookie step on its own.
