# Cookies

The `[cookies]` section tells Moombox where its cookie file lives, how it
signs in through a browser, and which optional ways of reading cookies are
allowed. What cookies are for, and how to provide them step by step, is in
[Cookies and sign-in](../guide/cookies.md).

```toml
[cookies]
cookie_file = "./cookies.txt"          # /data/cookies.txt in Docker
auto_enabled = false
browser_profile_dir = "./browser-profile"
# browser_path = ""                    # empty or absent = auto-detect
# browser_type = ""
refresh_interval = 360                 # minutes (6 hours)
# active_platforms = ["youtube", "twitch"]   # absent = automatic
acquisition = "auto"
# dpapi_fallback = false               # Windows only
# dpapi_profile_dir = ""               # Windows only
# platforms = ["youtube"]              # written by Moombox
```

!!! info "When changes take effect"
    `cookie_file`, `browser_profile_dir` and `refresh_interval` need a
    **restart**, and so does the timer half of `auto_enabled`: both UIs mark
    them as such, the dashboard offers to restart after you save, and the TUI
    shows a restart banner. Every other key applies as soon as you save.

    If you edit `config.toml` by hand, Moombox reads it at the next start.
    An invalid value in a hand-edited file is replaced (with the default, or
    cleared, as each key says) and a warning is logged; the dashboard and the
    TUI refuse an invalid value when you save. Editing the file while
    Moombox runs is not picked up, and a later save from either UI writes the
    running settings back over your edit — stop Moombox first.

In the web dashboard these settings are under **Settings → Cookies**. In the
terminal UI, open Settings with ++grave++ and pick the **Cookies** section.

## Cookie file

### `cookies.cookie_file`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./cookies.txt` (`/data/cookies.txt` in the Docker image) | Any non-empty path without a `..` segment | Restart | Dashboard: Settings → Cookies → **Cookie File**. TUI: Settings → Cookies → **Cookie file**. Also **Cookie File (manual)** / **Cookie file (manual)** in the advanced first-run wizards. |

The Netscape-format `cookies.txt` Moombox reads its YouTube and Twitch logins
from, and writes back to. Every way of adding cookies — browser login, import,
profile read, the background refresh — merges into this one file. A relative
path is resolved against Moombox's working directory (see
[Paths and logs](paths-and-logs.md)).

Moombox keeps a small bookkeeping file beside it, named after it with
`.meta.json` appended (`cookies.txt.meta.json`).

Change it when you keep the cookie file somewhere else, for example on a
folder only your user can read.

- The file does not need to exist: Moombox works without cookies, and the
  first browser login or import creates it.
- An empty value is refused by both UIs (the TUI says "Cookie file must not
  be empty"); in a hand-edited file it is replaced by `./cookies.txt`. A path
  containing a `..` segment, or one wrapped in quotes (what Windows Explorer's
  "Copy as path" gives you), is refused by both UIs.
- Moombox replaces the file by writing a new one and renaming it over the old
  one, and makes it readable only by your user. In Docker, do **not**
  bind-mount the file on its own or read-only — the rename then fails and the
  YouTube session slowly expires. Keep it inside the `/data` mount.

```toml
[cookies]
cookie_file = "D:/Moombox/private/cookies.txt"
```

## Browser login and refresh

### `cookies.auto_enabled`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true`, `false` | The refresh timer: restart. The automatic attempt after a lost login, and what the manual refresh does: as soon as you save. | Dashboard: Settings → Cookies → **Enable the headless-browser refresh timer**. TUI: Settings → Cookies → **Auto-cookie**. |

Turns on two automatic uses of a browser (the daily visit below is part of
the timer):

1. **The refresh timer.** Every
   [`refresh_interval`](#cookiesrefresh_interval), Moombox re-opens its own
   browser profile ([`browser_profile_dir`](#cookiesbrowser_profile_dir)) in
   the background, without a window, and re-extracts its cookies. A tick is
   skipped when no job is `Live` or `Downloading`, when a refresh already
   succeeded within the last half interval, and while the profile folder does
   not exist yet (it starts working once you complete a browser login, without
   a restart). On a host with no browser, or with
   [`acquisition`](#cookiesacquisition) = `profile`, a tick reads the profile
   instead, and only while `cookies.txt` is missing or holds no cookie rows.
   The timer also runs a pass **at least once a day**, with or without an
   active job, when nothing else has renewed the cookies for 24 hours, so an
   idle install still visits the sites daily. A daily visit that renewed
   nothing is tried again after 30 minutes, then less often, up to once a
   day; it never runs while Moombox is offline or in the first five minutes
   after start.
2. **Automatic attempts after a lost login.** When Moombox finds that a
   platform is no longer signed in, or a job stops in `COOKIES?`, it runs a
   refresh to try to repair it. A parked job is retried this way up to 3
   times; after that it stays parked, with an error saying the cookies are
   not the cause, until you resume it or a day has passed since the last
   retry. See [Cookies and sign-in](../guide/cookies.md#the-cookies-status).

With it off, nothing automatic launches a browser. Moombox flags the platform
for re-login and sends a "Cookie Re-Authentication Required" notification
instead.

What it does **not** control:

- the in-process check and rotation that runs every 30 minutes — that is
  always on;
- the browser login (**Set up YouTube** / `E L`), which works either way;
- the manual refresh (**Refresh cookies from browser profile**, shift+click on
  **Refresh cookies**, `R F`). With this switch off those import from the
  profile without launching a browser; with it on they launch the browser
  when one is available;
- the one-time profile import on first boot.

Turn it on for a desktop install with a browser, where you want Moombox to
keep its own profile signed in with no help. Leave it **off** in Docker: the
image has no browser, so there is nothing for it to launch.

```toml
[cookies]
auto_enabled = true
```

### `cookies.refresh_interval`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `360` (6 hours) | 10–10080 minutes (10 minutes to 7 days) | Restart | Dashboard: Settings → Cookies → **Refresh Interval**. TUI: Settings → Cookies → **Refresh interval**. |

How often the headless-browser refresh timer runs, in minutes. It only
matters when [`auto_enabled`](#cookiesauto_enabled) is on. It does not change
the in-process check, which always runs every 30 minutes. A tick runs only
while a job is recording; with none, the once-a-day visit described under
`auto_enabled` covers it, and this setting does not change how often that
visit runs.

Each timer pass starts a browser in the background, which costs a few seconds
of CPU and some memory, so there is little reason to go below an hour. YouTube
login cookies last a long time, which is why the upper limit is a week.

- Out of range in either UI: refused ("Cookie refresh interval must be
  10-10080 minutes" in the TUI). In a hand-edited file it is replaced by `360`.
- In `config.toml` you may also write a duration string: `"12h"`, `"90m"`,
  `"1d"`, `"1w"` (units `ms`, `s`, `m`, `h`, `d`, `w`). A string in any other
  form is a parse error, and `config.toml` does not load.

```toml
[cookies]
refresh_interval = "12h"
```

### `cookies.acquisition`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `"auto"` | `"auto"`, `"profile"` | Immediately | Dashboard: Settings → Cookies → **How refreshes get cookies** (**Automatic** / **Browser profile only**). TUI: Settings → Cookies → **Cookie source** (cycles `auto` / `profile`). |

How a cookie **refresh** gets its cookies. It does not affect the browser
login.

`auto`
:   Launch a browser against [`browser_profile_dir`](#cookiesbrowser_profile_dir)
    when one is available (and [`auto_enabled`](#cookiesauto_enabled) allows
    it); on a host without a browser, read the profile folder directly instead.
    A `browser_profile_dir` that lies inside an installed browser's own
    profile folders — your everyday browser — is refused, so the Moombox-only
    profile is the only one it will touch.

`profile`
:   Never launch a browser for a refresh. Always read
    `browser_profile_dir` directly, read-only, from a snapshot copy. This also
    lifts the refusal above, so it is the setting for reading **your real,
    signed-in Firefox profile** on a desktop. The timer, if
    `auto_enabled` is on, and the automatic repair attempt then import
    instead of launching; the timer's import only runs while `cookies.txt` is
    missing or holds no cookie rows, so it never overwrites cookies you
    already have.

Use `profile` when you want Moombox to follow the browser you actually use
rather than drive its own. Whatever it reads is exported into `cookies.txt`,
which is why it is opt-in. Only a Firefox-family profile (one containing
`cookies.sqlite`) can be read this way.

- Both UIs offer only the two values. In a hand-edited file anything other
  than `auto` or `profile` (lower case) is replaced by `auto` with a warning in
  the log. An empty or missing value means `auto`.

```toml
[cookies]
acquisition = "profile"
browser_profile_dir = "C:/Users/me/AppData/Roaming/Mozilla/Firefox/Profiles/abcd1234.default-release"
```

### `cookies.browser_profile_dir`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `./browser-profile` (resolves to `/data/browser-profile` in Docker) | A folder path without a `..` segment | Restart | Dashboard: Settings → Cookies → **Browser Profile Directory**. TUI: Settings → Cookies → **Browser profile dir**. |

The browser profile folder Moombox uses:

- For the **browser login** and the **browser refresh**, a Moombox-only
  profile kept separate from your everyday browser. Moombox creates the folder
  on the first browser login and restricts it to your user account (mode
  `0700` on Linux). It holds a full browser profile, including the session,
  so treat it as private.
- For the **profile read** on a host without a browser (Docker, headless
  servers) or with [`acquisition`](#cookiesacquisition) = `profile`: the
  Firefox-family profile folder to read — the folder containing `prefs.js`
  and `cookies.sqlite`, not the Firefox installation and not the folder that
  holds several profiles. Moombox never writes into a profile it only reads.

When `cookies.txt` is missing or holds no cookie rows, Moombox reads this
profile once, about 15 seconds after start, if a browser-free read is what it
would do (no browser on the host, or `acquisition = "profile"`) and the folder
contains a `cookies.sqlite`. This happens whatever
[`auto_enabled`](#cookiesauto_enabled) says.

- A relative path is resolved against the working directory; an empty value
  means `./browser-profile`. A path with a
  `..` segment, or one wrapped in quotes, is refused by both UIs.
- Moombox does not check at save time that the folder exists (in Docker it may
  be mounted later). A problem is reported when a refresh runs — for example
  "browser profile not found — run setup first", "browser profile path is not
  a directory", "cookies.sqlite not found in browser profile" or "browser
  profile directory could not be read" — and stays visible in
  Settings → Cookies as "Last cookie error".
- A folder inside a real installed browser's profiles is refused under
  `acquisition = "auto"` (logged at startup) and accepted under `"profile"`.

```toml
[cookies]
browser_profile_dir = "/data/browser-profile"
```

### `cookies.browser_path`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (auto-detect) | Empty, or an absolute path to an executable file (a `.exe` on Windows) | Immediately | Dashboard: Settings → Cookies → **Browser for cookie extraction** (pick a detected browser, or **Custom path…** → **Browser executable path**). TUI: Settings → Cookies → **Browser path**. |

Which browser the browser login and the browser refresh launch. Empty means
auto-detect: on Windows the system default browser first (unless it is Edge),
then LibreWolf, Zen Browser, Waterfox, Firefox, Vivaldi, Thorium, Brave,
Google Chrome, Opera and Microsoft Edge, in that order. On Linux the same
browsers are looked up on `PATH` (`firefox`, `google-chrome`, `chromium`, and
so on).

Set it when the browser you want is not detected (a portable or unusual
install) or when several are installed and you want a particular one. It must
be paired with [`browser_type`](#cookiesbrowser_type).

- Both UIs refuse a path that is not absolute, does not exist, is not a
  regular file, or is not executable, and refuse a path without a browser
  type ("browser_type required when browser_path is set" in the TUI).
- The dashboard additionally runs the browser with `--version` (up to 10
  seconds) when you save a custom path, and reports the result.
- In a hand-edited file, a path with no `browser_type` is ignored and
  auto-detect is used.
- Leading and trailing spaces are removed.

```toml
[cookies]
browser_path = "C:/Tools/ungoogled-chromium/chrome.exe"
browser_type = "chrome"
```

### `cookies.browser_type`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty | `firefox`, `waterfox`, `librewolf`, `zen`, `chrome`, `brave`, `edge`, `vivaldi`, `thorium`, `opera` | Immediately | Dashboard: Settings → Cookies → **Custom path…** → **Browser type** (**Firefox-family** saves `firefox`, **Chromium-family** saves `chrome`). TUI: Settings → Cookies → **Browser type** (free text). |

Tells Moombox how to drive the browser named in
[`browser_path`](#cookiesbrowser_path): Firefox-family browsers and
Chromium-family browsers are handled differently. Only the family matters for
launching, so `chrome` works for any Chromium-based browser. A type without a
path has no effect.

For the Windows [DPAPI fallback](#cookiesdpapi_fallback), a specific type such
as `brave` or `edge` (possible from the TUI) limits which real browser
profiles are considered; `chrome` considers all Chromium-family browsers.

- When a browser path is set, a type outside the list above is refused by
  both UIs. A type saved without a path is ignored. Leading and trailing
  spaces are removed.

```toml
[cookies]
browser_path = "/opt/librewolf/librewolf"
browser_type = "librewolf"
```

## Platforms

### `cookies.active_platforms`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| absent (automatic) | A list containing `"youtube"` and/or `"twitch"`, or `[]` | Immediately | Dashboard: Settings → Cookies → **Active Platforms** (**YouTube cookies**, **Twitch cookies**). TUI: Settings → Cookies → **YouTube cookies**, **Twitch cookies**. |

Which platforms' cookie indicators Moombox shows, in the dashboard's and the
TUI's status bars, and which **Set up** buttons the dashboard offers. It does
not change which cookies are used — every cookie in the file is used either
way.

When absent, Moombox decides by itself: platforms it has verified cookies for
([`platforms`](#cookiesplatforms)), otherwise the platforms of your enabled
channels. As soon as you flip either toggle, your choice is saved and wins
from then on, including `[]` (both off). The toggles show the automatic
answer until you change one, and saving without touching them keeps the
choice automatic.

Turn a platform off if you never use cookies for it and do not want its
indicator, or on to get the **Set up** button for a platform you have no
channels for yet.

```toml
[cookies]
active_platforms = ["youtube"]
```

### `cookies.platforms`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `[]` | Written by Moombox | — | Not shown in either UI |

**Bookkeeping written by Moombox — do not edit.** The platforms Moombox has
verified working cookies for. When both this list and `active_platforms` are
empty, Moombox fills it in at startup from the cookie file (from the platforms
its bookkeeping file recorded as verified, otherwise from the login cookies
the file contains). A browser login that verifies a platform adds it. Moombox
never removes entries.

It is used as the fallback for [`active_platforms`](#cookiesactive_platforms)
and, with [`auto_enabled`](#cookiesauto_enabled) on, to notice after a restart
that a platform which used to work has lost its login. A platform you removed
from it is seen that way from the next start.

```toml
[cookies]
platforms = ["twitch", "youtube"]
```

## Windows DPAPI fallback

### `cookies.dpapi_fallback`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| `false` | `true`, `false` | Immediately | Dashboard: Settings → Cookies → **DPAPI fallback (Windows only)**. TUI: Settings → Cookies → **DPAPI fallback (Windows)**. |

**Windows only.** When a browser refresh with a Chromium-family browser fails
(for example because that browser is already running), Moombox reads cookies
straight from your **real** Chrome, Edge, Brave, Vivaldi or Chromium profile
(including the Beta, Dev and Canary channels of Chrome and Edge) using
Windows' DPAPI, without launching anything.

It reads your actual signed-in session cookies and exports them to
`cookies.txt`, which is why it is off unless you turn it on.

- It runs only after a failed **browser** refresh — not for Firefox-family
  browsers, not for profile reads, and not for the browser login.
- It picks one profile, the one with the most usable YouTube and Twitch login
  cookies, and never combines profiles.
- Cookies protected by Chrome's app-bound encryption cannot be read; the log
  names that as the reason when it happens.
- On Linux and in Docker the setting has no effect.

```toml
[cookies]
dpapi_fallback = true
```

### `cookies.dpapi_profile_dir`

| Default | Allowed values | Takes effect | Where to change |
|---|---|---|---|
| empty (discover) | A Chromium-family **profile** folder path that does not start with `..` | Immediately | `config.toml` only |

**Windows only.** The one browser profile the
[DPAPI fallback](#cookiesdpapi_fallback) should read, instead of searching the
standard browser folders under `%LOCALAPPDATA%`. Set it for a portable
browser, a profile moved with `--user-data-dir`, or Opera, none of which the
search finds.

Name the **profile** folder, not the "User Data" folder above it: for example
`...\User Data\Default` or `...\User Data\Profile 1`. Moombox checks on every
use that it is a real folder (not a link), that a `Local State` file sits
beside it or inside it, and that it contains a `Cookies` or `Network\Cookies`
file. A folder that fails these checks is an error, and Moombox does not fall
back to searching.

- It has no effect unless `dpapi_fallback` is on. At startup Moombox logs a
  warning if it is set while the fallback is off, or if the folder is unusable.
- A path that climbs above its starting point (`..\something`) is cleared
  with a warning when `config.toml` is loaded.

```toml
[cookies]
dpapi_fallback = true
dpapi_profile_dir = "D:/PortableApps/Chromium/User Data/Default"
```
