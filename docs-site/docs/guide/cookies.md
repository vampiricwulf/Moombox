# Cookies and sign-in

Moombox records most streams without an account. Some content needs one, and
Moombox gets an account the same way yt-dlp does: from your browser's login
cookies, kept in a Netscape-format `cookies.txt` file. This page explains why
you might need cookies, every way to give them to Moombox, how Moombox keeps
them alive, and what the cookie badges and the `COOKIES?` status mean.

The settings mentioned here are documented key by key in
[Cookies settings](../configuration/cookies.md).

## When you need cookies

| You want to record | Needs |
|---|---|
| Public YouTube and Twitch livestreams and VODs | Nothing |
| YouTube members-only streams and videos | YouTube cookies from an account that holds the membership |
| Age-restricted YouTube content | YouTube cookies from an age-verified account. Without them the job ends in `Error` (not `COOKIES?`), with no failure notification, and does not resume on its own: add the cookies, then reinitialize the job |
| Members-only streams found by [membership discovery](../configuration/monitors.md#monitorsmembership_discovery) | YouTube cookies |
| Twitch subscriber-only VODs and streams | Twitch cookies from an account with access |

Without Twitch cookies Moombox simply works anonymously on Twitch, which is
normal and needs no attention. Without YouTube cookies, anything that needs an
account stops in the `COOKIES?` status (see [below](#the-cookies-status)) —
except age-restricted content, which ends in `Error` as described in the table.

What counts as a login:

- **YouTube** — a `SAPISID` cookie (or one of its `__Secure-1PAPISID` /
  `__Secure-3PAPISID` variants) **and** a non-empty `LOGIN_INFO` cookie, from
  `youtube.com` / `google.com`.
- **Twitch** — the `auth-token` cookie from `twitch.tv`.

When Moombox reads cookies out of a browser itself (the browser login, a
refresh, a profile read), it keeps only YouTube, Google and Twitch cookies; it
does not collect the rest of your browser's cookies.

## One file, both platforms

Everything ends up in one file, [`cookies.cookie_file`](../configuration/cookies.md#cookiescookie_file)
(`./cookies.txt` by default, `/data/cookies.txt` in Docker). It holds YouTube
and Twitch rows side by side. Every way of adding cookies **merges** into this
file rather than replacing it: a YouTube-only import leaves your Twitch session
alone, and vice versa.

Next to it Moombox keeps a small bookkeeping file, `cookies.txt.meta.json`
(the cookie file's name plus `.meta.json`), recording when cookies were last
refreshed and which platforms were verified. You do not need to edit it.

## Ways to provide cookies

There are four, and they suit different setups:

| Method | Works on | Needs a browser on the Moombox host | Works from a remote dashboard |
|---|---|---|---|
| [Browser login](#browser-login) | Windows, Linux desktop | Yes | No — run it on the host |
| [Import a cookies.txt](#import-a-cookiestxt) | Everywhere, including Docker | No | Yes |
| [Read a Firefox profile](#read-a-firefox-profile-docker-and-headless-hosts) | Everywhere, including Docker | No | Yes (the trigger) |
| [DPAPI fallback](#dpapi-fallback-windows-only) | Windows only | A Chromium-family browser | Not applicable (automatic) |

### Browser login

The browser login opens a real browser window on the Moombox host, using a
**separate, Moombox-only browser profile** stored in
[`cookies.browser_profile_dir`](../configuration/cookies.md#cookiesbrowser_profile_dir)
(`./browser-profile` by default). It never touches your everyday browser
profile. You sign in once in that window; Moombox then reads the cookies out
of it and closes the window.

Supported browsers, in the order Moombox looks for them:
LibreWolf, Zen Browser, Waterfox, Firefox, Vivaldi, Thorium, Brave,
Google Chrome, Opera, Microsoft Edge. On Windows the system's default browser
is tried first (unless it is Edge). On Linux Moombox looks for the browser
programs on `PATH` (for example `firefox`, `google-chrome`, `chromium`). You
can pick a specific browser or point at any executable with
[`cookies.browser_path`](../configuration/cookies.md#cookiesbrowser_path) and
[`cookies.browser_type`](../configuration/cookies.md#cookiesbrowser_type).

=== "Web dashboard"

    1. Open **Settings → Cookies**.
    2. Under **Active Platforms**, make sure **YouTube cookies** and/or
       **Twitch cookies** is switched on. The **Set up YouTube** and
       **Set up Twitch** buttons only appear for platforms switched on here.
    3. Click **Set up YouTube** (or **Set up Twitch**). A browser window opens
       on the host at the Google (or Twitch) sign-in page, and the dashboard
       shows the **Setup: YouTube** dialog.
    4. Sign in in that browser window.
    5. Click **I'm Logged In**. Moombox shows "Extracting cookies..." with a
       60-second countdown, closes the browser, verifies the login and shows
       the result.

    The first-run wizard offers the same **Set up YouTube** / **Set up Twitch**
    buttons on its Cookies step.

=== "Terminal UI"

    1. Press ++e++ then ++l++ (**Cookie Login**). If the status bar is
       showing a re-login alert, the platform it is about is preselected.
    2. Choose **YouTube** or **Twitch** and press ++enter++. A browser window
       opens on the host.
    3. Sign in, then press ++enter++ to extract cookies. The TUI waits up to
       5 minutes ("Waiting for YouTube login... (300s remaining)"); ++esc++
       cancels.
    4. If the wait runs out you get **R** (Try Again) and **S** (Skip).
       Choose **Close** when you are done.

    The TUI's first-run wizard has the same step.

**What the result means**

| Message | Meaning |
|---|---|
| "YouTube cookies configured" (green) | Saved and verified. |
| "YouTube cookies saved, but Moombox could not establish whether they work — nothing has been concluded about them" (yellow) | Saved and in use, but the check could not reach the site (a network blip, a rate limit). Usually fine; recheck later. |
| "No login detected. Try again." | The browser held no login. Make sure you finished signing in before clicking **I'm Logged In**. Your existing `cookies.txt` is left untouched. |
| "Cookies were saved, but Moombox could not establish whether they authenticate — they cannot form a signed-in request. Try signing in again." | Cookies were found but are incomplete. Sign in again. |

!!! warning "Run it on the host"
    The browser opens **on the machine Moombox runs on**, so the dashboard only
    allows the browser login when you open the dashboard on that machine
    (`localhost` / `127.0.0.1`). From any other device the **Set up** buttons
    say "Browser login opens a window on the host, so run this on the host.
    From here, use Import cookies.txt instead." Remote desktop (RDP, VNC) to
    the host counts as being on the host.

!!! note "Linux and Docker"
    On Linux the browser login needs a desktop session and an installed
    browser, because a visible window opens. A headless server cannot use it.
    The Docker image ships **no browser at all**, so the browser login is not
    available there — use [Import a cookies.txt](#import-a-cookiestxt) or
    [Read a Firefox profile](#read-a-firefox-profile-docker-and-headless-hosts).

Other things to know:

- Only one browser login can run at a time, and it cannot start while a cookie
  refresh or import is running ("please try again shortly").
- If you reload the dashboard or open a second tab while a login is running,
  the dialog reopens there so you can finish or cancel it. Closing the tab
  releases the login so it does not block later attempts.
- **Cancel** (or ++esc++) closes the browser window and abandons the attempt.
- A Chromium-family browser refuses to start on a profile that another browser
  is still using. If you see "browser profile in use", close the browser named
  in the message (it may be on another machine, if the profile folder is
  shared), or delete the lock file it names if nothing is using the profile.

### Import a cookies.txt

Export cookies from any browser that is signed in, using a "cookies.txt"
browser extension or yt-dlp's `--cookies-from-browser`, in **Netscape**
format. Then give the file to Moombox. This works from anywhere — a phone, a
remote dashboard, a container — and needs no shell or access to the data
folder.

=== "Web dashboard"

    1. Open **Settings → Cookies** and scroll to **Paste or upload a
       cookies.txt**.
    2. Paste the file's contents into **cookies.txt contents**, or leave that
       box empty and choose the file with the file picker. If both are filled,
       the pasted text wins.
    3. Click **Import cookies**.

    Clicking a **YT: Re-login** / **TW: Re-login** warning in the status bar
    from a remote device jumps straight to this panel.

=== "Terminal UI"

    1. Press ++e++ then ++i++ (**Import Cookie File**).
    2. Type the path to the file (`~` is expanded) and press ++enter++.
    3. The result is shown per platform. ++esc++ closes the dialog; if the
       import is still running it keeps going and the result appears on the
       feedback line ("Cookie import: YouTube imported (authenticates); Twitch
       unchanged (...)").

    The TUI only ever handles the **path**; the file's contents are never
    shown or logged.

**How an import is checked.** Moombox merges the new rows into `cookies.txt`,
reloads it immediately and checks each platform the file contained:

| Outcome for a platform | What happened |
|---|---|
| imported | The new cookies are now in use. |
| unchanged | The file had no rows for this platform, so its existing cookies were kept as they were. |
| rolled back | The new cookies did not authenticate, so Moombox **put the previous ones back**. You see "YouTube cookies were not accepted — they did not authenticate, so Moombox kept the working ones it already had". If the previous cookies no longer work either, the message says so and asks for a fresh export. |
| rejected | The new cookies did not authenticate and there was nothing working to restore. They stay on disk as the best guess for the next attempt. |

An import is refused, and nothing is written, when:

- the text is not a Netscape cookie file at all (usually a JSON export from an
  extension) — re-export in Netscape / cookies.txt format;
- the file holds only comments ("that cookie file has no cookie rows");
- no row is a YouTube or Twitch login cookie ("sign in to the site first, then
  export again from the window that is signed in") — the most common mistake;
- the file is larger than 512 KiB (a real export is a few kilobytes);
- a cookie refresh is running at that moment (try again shortly);
- the existing `cookies.txt` cannot be read (a permission or mount problem).
  Moombox will not overwrite a file it could not read, because it may hold
  working credentials. Fix the permission or mount; do not replace the file.

Rows with an empty value are dropped, and Windows line endings are handled.

!!! tip "Export from a private window"
    Sign in to YouTube in a private/incognito window, export, then **close
    the window without browsing further**. If you keep using the same browser
    session after exporting, YouTube rotates the session and the exported
    copy can stop working.

!!! tip "Scripting an import"
    The same import is available at `POST /api/cookies/import`, with the file
    as a `text/plain` body or as a multipart upload in a part named
    `cookies`. See [API](../reference/api.md).

### Read a Firefox profile (Docker and headless hosts)

On a host with no browser — the Docker image, or a headless server — Moombox
can read cookies straight out of a **Firefox-family profile folder** you copy
or mount into [`cookies.browser_profile_dir`](../configuration/cookies.md#cookiesbrowser_profile_dir).
It never launches a browser and never writes into the profile; it copies the
cookie database to a private temporary folder and reads the copy.

- Point it at the **profile folder itself** (the one containing `prefs.js` and
  `cookies.sqlite`), not at the Firefox installation folder or the folder
  holding several profiles.
- Copy the profile **with Firefox closed**, and keep `cookies.sqlite-wal` next
  to `cookies.sqlite`. Recent cookies live in that `-wal` file, and a copy
  without it reads as empty ("no YouTube/Twitch cookies found in browser
  profile").
- In Docker, mount it at `/data/browser-profile`; no setting is needed because
  the default `./browser-profile` resolves there. See
  [Docker](../getting-started/docker.md).

**First boot is automatic.** When `cookies.txt` is missing or holds no cookie
rows, Moombox imports the profile once, about 15 seconds after start. An
install that already has cookies is never overwritten this way.

**After that, you trigger the read.** Once you have cookies, the profile is
not re-read on a timer, because nothing inside Moombox changes it. When you
have refreshed the profile
(signed in again on the source machine, copied it over again), use any of:

- **Settings → Cookies → Refresh cookies from browser profile** (works on
  touch devices),
- shift+click the **Refresh cookies** button (the circular arrow) in the
  dashboard's status bar,
- ++r++ then ++f++ in the TUI (**Refresh Cookies from Browser**).

These import straight from the profile whatever
[`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled) says
and whatever `cookies.txt` already holds. The result is checked and rolled
back per platform exactly like an [import](#import-a-cookiestxt).

Reading a profile only refreshes what the profile holds. It cannot renew a
YouTube session on its own; that is the job of the
[in-process refresh](#how-moombox-keeps-cookies-alive).

!!! warning "Your real browser profile"
    By default Moombox refuses to read a `browser_profile_dir` that sits inside
    an installed browser's own profile folders — your everyday browser — and
    says "reading this browser profile needs cookies.acquisition = "profile"".
    This is deliberate: everything it reads is exported into `cookies.txt`.
    To read your real signed-in Firefox profile on a desktop, set
    [`cookies.acquisition`](../configuration/cookies.md#cookiesacquisition) to
    `profile` (**How refreshes get cookies → Browser profile only**). Moombox
    then never launches a browser for a refresh and only reads the profile,
    read-only.

### DPAPI fallback (Windows only)

When [`cookies.dpapi_fallback`](../configuration/cookies.md#cookiesdpapi_fallback)
is on and a browser refresh with a Chromium-family browser fails (typically
because that browser is busy), Moombox reads the cookies directly from your
**real** Chrome/Edge/Brave/Vivaldi/Chromium profile using Windows' own
decryption (DPAPI). It is off by default because it reads your actual signed-in session.

- It picks exactly **one** profile — the one with the most usable YouTube and
  Twitch login cookies — and never mixes cookies from two profiles. If you are
  signed in to YouTube in one profile and Twitch in another, only one of them
  is used.
- Newer Chrome versions protect cookies with app-bound encryption that DPAPI
  cannot open; the log then names that as the reason nothing could be read.
- Discovery covers the standard browser folders under `%LOCALAPPDATA%`. A
  portable browser, a relocated profile, or Opera needs
  [`cookies.dpapi_profile_dir`](../configuration/cookies.md#cookiesdpapi_profile_dir).
- It does nothing for Firefox-family browsers, which do not use DPAPI.

!!! note "Linux and Docker"
    DPAPI exists only on Windows. On Linux and in Docker the setting has no
    effect.

## How Moombox keeps cookies alive

Two separate mechanisms run. Only the first is always on.

**The in-process refresh (always on, every 30 minutes).** Moombox checks each
platform's login against YouTube and Twitch at startup and then every 30
minutes, and writes back the cookie values YouTube rotates, so a YouTube
session stays alive for as long as YouTube allows. This needs no browser and
no setting. It is not configurable. Press **Refresh cookies** in the status
bar (plain click) or ++r++ then ++c++ in the TUI (**Recheck Cookies**) to run
it now; the answer reads, for example, "Cookies: YouTube OK, Twitch — could
not establish".

**The headless-browser refresh timer (optional).** With
[`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled) on,
Moombox also re-opens the Moombox-only browser profile in the background every
[`cookies.refresh_interval`](../configuration/cookies.md#cookiesrefresh_interval)
(6 hours by default) and re-extracts its cookies. A tick is skipped when no
job is `Live` or `Downloading`, when a refresh already succeeded within the
last half interval, and when the browser profile folder does not exist yet. The same
switch allows **one automatic browser attempt** when Moombox finds a platform
has lost its login.

Every refresh is checked per platform before it counts: if newly extracted
cookies do not authenticate and the previous ones did, the previous ones are
put back.

## Which platforms are shown

The YouTube and Twitch indicators (and the dashboard's **Set up** buttons)
appear only for **active** platforms. Moombox decides this as follows:

1. If you switched the **Active Platforms** toggles yourself
   ([`cookies.active_platforms`](../configuration/cookies.md#cookiesactive_platforms)),
   that choice wins — including "both off".
2. Otherwise, platforms whose cookies Moombox has verified before
   ([`cookies.platforms`](../configuration/cookies.md#cookiesplatforms)).
3. Otherwise, the platforms of your enabled channels.

## Cookie indicators

=== "Web dashboard"

    The status bar shows **YT** and **TW** indicators. Hover for the full
    meaning; each tooltip starts with the platform name ("YouTube: …",
    "Twitch: …"):

    | Colour | Tooltip | Meaning |
    |---|---|---|
    | Green | "YouTube: Authenticated" | Working. |
    | Yellow | "YouTube: No cookies" | No YouTube login. |
    | Grey | "Twitch: Anonymous" | No Twitch login — normal. |
    | Yellow | "Cookies saved — Moombox could not establish whether they work" | The last check could not reach the site. |
    | Red | "Not authenticated" (with the reason, when known) | The site refused the cookies. |
    | Red | "A download stopped for want of usable credentials" | A job of this platform is parked in `COOKIES?`. |
    | Red | "cookies.txt could not be read (error)" | The file exists but cannot be read — a permission or mount problem. |
    | Red | "Re-login required" | Moombox concluded you must sign in again. |

    A **YT: Re-login** / **TW: Re-login** warning also appears beside the
    indicators. Click it: on the host with a browser available it starts the
    browser login; anywhere else it opens the import panel.

    The circular-arrow **Refresh cookies** button runs the in-process check
    on click, and the browser refresh / profile import on shift+click.

=== "Terminal UI"

    The status bar shows `YT` and `TW`:

    | Display | Meaning |
    |---|---|
    | green `YT` / `TW` | Working. |
    | yellow `YT` | No YouTube cookies. |
    | dim `TW` | Twitch anonymous — normal. |
    | `YT: Unknown` (`YT` when narrow) | The last check could not reach the site. |
    | red `YT` / `TW` | Cookies refused, or a job parked in `COOKIES?`. |
    | `YT: cookies.txt unreadable` (`YT?` when narrow) | The file cannot be read. |
    | red `YT: Re-login` (`YT!` when narrow) | Sign in again. When the bar is wide enough it adds `(E L)`, the chord that fixes it. |

    The same labels apply to `TW`. When the bar is very narrow, the first
    four (the states that need no action) are hidden.

The order of importance is the same in both: re-login, then unreadable file,
then a parked job or refused cookies, then "could not establish", then none,
then OK.

When a platform loses its login, Moombox also sends a notification (see
[Notifications](notifications.md)). With `auto_enabled` off it is titled
"Cookie Re-Authentication Required" and tells you nothing will try to repair
it automatically.

## The `COOKIES?` status

A job stops in `COOKIES?` instead of `Error` when signing in is the fix:
YouTube said "sign in" or "members only", Twitch refused the login, or a
Twitch stream or VOD is subscriber-only. Moombox then:

1. tries one automatic cookie refresh, if
   [`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled) is
   on;
2. otherwise waits. The job's message names the cookie file to replace.

**Parked jobs resume on their own** once that platform's login works again —
after a browser login, an import, a profile refresh or a successful automatic
refresh. Backlog VODs go back to `Queued`; other jobs go back to `Upcoming`.
Queued VODs are re-admitted a few at a time under the
channel's archive slots. You get a notification such as "Resumed 3 job(s)
waiting on YouTube cookies — backlog re-queues and drains at archive_slots per
channel".

!!! note "Members-only with the wrong account"
    If YouTube refused a members-only video **while your cookies were signed
    in**, the account simply does not hold that membership — often the wrong
    one of several Google accounts in the exporting browser. These jobs are
    **not** resumed when the same login is refreshed; they resume only when
    cookies for a **different** account arrive. Export from the account that
    is a member.

You can also act on a `COOKIES?` job by hand — Resume (YouTube jobs with
staged data), Reinitialize, Cancel or Delete; see [Jobs](jobs.md).

## How cookie files are protected

- Cookie values are never written to the log, shown in either UI, or included
  in error messages. The TUI import handles only the file path.
- `cookies.txt` is written to a temporary file, flushed to disk and renamed
  into place, so a crash cannot leave half a file. The file is made readable
  only by your user (`0600` on Linux).
- The browser profile folder is restricted to your user account when the
  browser login creates it (`0700` on Linux). On Windows the cookie file's
  folder is restricted to your user account too. On Linux the cookie folder
  is tightened to `0700` only when it is a folder of its own — not when it
  also holds the output, staging, database or log (such as Docker's `/data`).
- The browser login (start, finish, cancel) is only accepted from the host
  itself, so nobody on your network can open a browser window on your screen.
- The DPAPI fallback and reading your real browser profile are both off until
  you turn them on.

!!! warning "Docker: do not mount `cookies.txt` on its own"
    Moombox replaces `cookies.txt` by renaming a new file over it. A
    single-file bind mount (or a `:ro` mount) cannot be replaced that way, so
    the write-back fails, only a warning is logged, and the session slowly
    ages out. Keep the file inside the mounted `/data` folder instead.

## Troubleshooting

| Symptom | What to do |
|---|---|
| "a supported browser (...) is required" | Install one of the supported browsers on the host, or set a [custom browser path](../configuration/cookies.md#cookiesbrowser_path). In Docker, import a file instead. |
| "browser profile not found — run setup first" | The Moombox profile folder does not exist yet. Run the browser login once, or mount a Firefox profile there. |
| "browser profile path is not a directory" | `browser_profile_dir` points at a file — often a `cookies.txt` mounted in the wrong place. |
| "cookies.sqlite not found in browser profile" | You mounted the Firefox installation or the parent profiles folder. Point at the folder with `prefs.js`. |
| "cookies.sqlite is locked by another process" | Close Firefox (recent versions lock the database while running), then retry. |
| "browser profile directory could not be read" | Permissions: in Docker, the container's user cannot read the mounted folder. |
| "existing cookies.txt could not be read" | Fix the file's permissions or mount. Moombox left it untouched on purpose. |
| YouTube works, then stops after you browse | You kept using the browser session you exported from. Re-export from a private window and close it. |
| Members-only video stays in `COOKIES?` after refreshing | The account is not a member; see [the note above](#the-cookies-status). |

More general help is in [Troubleshooting](../help/troubleshooting.md).
