# Command line

Moombox is normally started with no arguments. This page lists everything the
command line accepts, the environment variables Moombox reads, how it finds
its configuration, and how it starts, restarts and stops.

The examples use `moombox`; on Windows the program is `Moombox.exe`, and the
Linux release files are `moombox-linux-amd64` / `moombox-linux-arm64` unless
you renamed them.

## Synopsis

```text
moombox [flags]                                  run the archiver
moombox [-config path] add [--] <video_id_or_url>  queue a video and exit
moombox -version                                 print the version and exit
moombox -help                                    print usage and exit
```

Flags can be written with one or two dashes (`-no-tui` or `--no-tui`), and a
value can follow a space or an `=` (`-config my.toml`, `--config=my.toml`).
Flags go before `add`; `-config` may also follow it.

## Flags

| Flag | Effect |
|---|---|
| `-config <path>` | Use this configuration file and nothing else. See [Configuration file](#configuration-file). |
| `-log-level <level>` | Log at `DEBUG`, `INFO`, `WARN` or `ERROR` for this run only (case-insensitive). The setting in `config.toml` is not changed. An unknown value falls back to `INFO`. |
| `-no-tui` | Run without the terminal UI: web dashboard only. |
| `-headless` | Same as `-no-tui`. |
| `-version` | Print `moombox <version> (<commit>)` and exit. |
| `-h`, `-help` | Print the usage text and exit. |

`-version` and `-help` are answered at once, without starting anything, so
they work while Moombox is running.

The commit is the short Git revision of the build. A release shows the tagged
commit, a build from a modified working tree adds `-dirty`, and a build
without Git information shows `unknown`.

??? note "More about `-log-level`"
    The override applies to the logger only. Saving an unrelated setting
    keeps it; choosing a log level explicitly in Settings (dashboard or
    terminal UI) replaces it for the rest of the run. The level you normally
    want is [`logs.log_level`](../configuration/paths-and-logs.md#logslog_level).

## Terminal UI or headless

Moombox shows the terminal UI only when **both** its input and its output are
an interactive terminal, and nothing asked for headless mode. It runs headless
(web dashboard only, log lines printed to the console) when:

- `-no-tui` or `-headless` is given;
- `MOOMBOX_NO_TUI` is `1`, `true` or `yes` (any case);
- input or output is not a terminal: output piped to a file or another
  program, a systemd service, a Task Scheduler task without a console, a
  container started without a TTY.

Headless and terminal-UI mode differ in two failure cases:

| Situation | Terminal UI | Headless |
|---|---|---|
| The web dashboard cannot bind its port (and none of the next ten ports is free) | Logs a warning and keeps running without the dashboard. | Prints the error and exits with code 3. |
| A startup error (bad configuration, unusable database) | Prints the error and waits for ++enter++ so the window does not vanish, then exits with code 3. | Exits with code 3 at once. |

In both modes, Moombox tries to open the dashboard in your default browser
when it starts (`explorer.exe` on Windows, `xdg-open` on Linux). On a machine
without a desktop this simply does nothing.

## Configuration file

**With `-config <path>`,** that file is the only candidate:

- If it exists, it is loaded.
- If it does not exist, Moombox starts on defaults and runs the
  [setup wizard](../getting-started/first-run.md); the wizard creates the file
  at exactly that path.

**Without `-config`,** Moombox uses the first of these that exists:

1. `config.toml` in the current working directory;
2. `config/config.toml` under the current working directory;
3. `~/.config/moombox/config.toml` in your home folder (on Windows,
   `%USERPROFILE%\.config\moombox\config.toml`).

If none exists, Moombox runs the setup wizard, which writes `config.toml` in
the current working directory. Every later save goes back to the file that was
loaded, so a configuration found in `config/` stays there.

The startup log names the file it used (`Configuration loaded path=…`), or
says it is running on defaults and where a save will create one.

!!! note
    The search does not consult `XDG_CONFIG_HOME`; it always uses
    `~/.config/moombox`.

**Relative paths** inside the configuration (`./moombox.db`, `./output`, and
so on) are resolved against the working directory Moombox was started from,
not against the configuration file's folder. If you start Moombox from
different directories, use absolute paths or always start it from the same
place (`WorkingDirectory=` in a systemd unit).

Moombox reads `config.toml` once, at startup. Changes made in the dashboard or
the terminal UI are saved to it immediately, and the file is rewritten
without comments each time. Edit it by hand only while Moombox is stopped. A
file that cannot be parsed stops startup with
`Startup error: load config: failed to parse config …` and exit code 3. All
settings are documented under [Configuration](../configuration/index.md).

## `moombox add`

```bash
moombox add "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
moombox -config /srv/moombox/config.toml add https://www.twitch.tv/somechannel
moombox add -- -AbCdEfGhIj
```

Queues one target in Moombox's database and exits. It does not start
Moombox; the running instance picks the job up by itself, with no restart.

### What it accepts

| Input | What is queued |
|---|---|
| A YouTube video URL (`watch?v=`, `youtu.be/`, `/live/`, `/shorts/`, `/embed/`, `/v/`) or a bare 11-character video ID | One YouTube job, titled *Manual Add* until Moombox reads the video. |
| A Twitch channel URL or login name | A job that waits for that channel to go live and records the broadcast. |
| A Twitch VOD URL (`twitch.tv/videos/<id>` and the other VOD forms) | One VOD job. |
| A Twitch clip URL (`clips.twitch.tv/<slug>`, `twitch.tv/<channel>/clip/<slug>`) | One clip job. |
| A Twitch channel's video list (`twitch.tv/<channel>/videos`, with its `?filter=` and `?sort=`) or a collection | One `Queued` job per VOD in the listing, admitted by the backlog scheduler. |

A channel's clip list (`twitch.tv/<channel>/clips`) is refused:
`Twitch clip lists are not supported yet.` A YouTube video ID that starts with
`-` is accepted as it is, or after `--`.

!!! tip
    A bare word is tried as a YouTube video ID first. A Twitch login that is
    exactly 11 letters, digits, `_` or `-` would be read as a YouTube ID, so
    give Twitch channels as full `https://www.twitch.tv/<login>` URLs.

Twitch listings are read signed out, so a subscriber-only VOD is listed as
anyone sees it; the job itself downloads with Moombox's own sign-in. A Twitch
live-channel job takes the channel's quality preference when the channel is in
your configuration, otherwise `best`.

Output and results:

- `Added <id> to queue.`, `Added Twitch VOD <id> to queue.`, or
  `Queued N Twitch VOD(s); M already queued.` — success. For a single video,
  channel, VOD or clip a **Job Added** notification is sent to your webhooks,
  as when you add from the dashboard.
- `Job already exists for video: <id>` — nothing to do (exit code 0).
- Anything else is an error on standard error, with exit code 1.

### Which database it writes to

`add` must write to the database the Moombox instance uses, so:

- It needs a configuration file. With no `-config` it searches the same
  places Moombox does; if it finds nothing it refuses, rather than create a
  stray database, and names the configuration a running instance loaded.
- **While Moombox is running as the same user,** `add` uses the database that
  instance has open, from whatever directory you run it in. If the
  configuration you pass resolves to a different database, it refuses and
  names both.
- **While Moombox is stopped,** a relative `database_path` (the default
  `./moombox.db`) is resolved against the current directory, so run `add` from
  Moombox's own folder, or set an absolute
  [`paths.database_path`](../configuration/paths-and-logs.md#pathsdatabase_path).
- If the database was created by a different version of Moombox (for example
  right after an update, before the new version has started), `add` refuses
  and asks you to start Moombox first so it can upgrade the database.

In Docker, run it inside the container:
`docker compose exec moombox moombox -config /data/config.toml add <url>`.

## Environment variables

| Variable | Effect |
|---|---|
| `MOOMBOX_NO_TUI` | `1`, `true` or `yes` (any case) runs headless, as `-no-tui` does. The Docker image sets it. |
| `MOOMBOX_PPROF` | `1` starts Go's profiling endpoints on `localhost:6060`. See [Profiling](#profiling). |
| `MOOMBOX_CONFIG` | Docker image only: the configuration path the entrypoint seeds and passes as `-config` (default `/data/config.toml`). The binary itself does not read it. |
| `TZ` | Time zone for file names built from `${start_date}` / `${start_time}` and for log timestamps. Mostly useful in Docker, where the default is UTC. |
| `GOMEMLIMIT` | Go's memory limit, honoured at startup. [`memory.go_soft_limit_mb`](../configuration/disk-memory-connectivity.md#memorygo_soft_limit_mb) overrides it while set; clearing that setting restores this value. |
| `HOME` (Linux), `LOCALAPPDATA` (Windows) | Where the single-instance lock lives (`~/.local/share/moombox`, `%LOCALAPPDATA%\moombox`). With `HOME` unset on Linux, `/tmp` is used. |
| `XDG_CACHE_HOME` (Linux), `LOCALAPPDATA` (Windows) | Where the BotGuard helper's runtime is unpacked (`…/Moombox/sidecar`). |
| `XDG_CONFIG_HOME` (Linux) | Used only to locate yt-dlp's plugin folder for the [yt-dlp plugin](../guide/ytdlp-plugin.md). |

Variables beginning with `_MOOMBOX_` (such as `_MOOMBOX_CHILD`) are how the
launcher talks to the program it starts. Do not set them yourself.

## The launcher

Every normal start of Moombox runs two processes from the same file:

- the **launcher**, which you started. It takes the single-instance lock,
  starts the program as a child in the same console, and waits;
- the **program**, which does all the work.

When the program asks for a restart (after the setup wizard, an update, a
setting that needs one, or **Restart Moombox** / ++r++ ++p++), it exits and the
launcher starts it again, picking up a new binary if an update placed one.
The console stays attached throughout, so the terminal UI comes back cleanly
and no chain of processes builds up.

The launcher also supervises the program:

- **Crash recovery.** If the program dies abnormally after running for at
  least 60 seconds, the launcher restarts it after 1 s, then 2, 4, 8 and
  16 s for consecutive crashes. After five crashes in a row without a
  healthy 60-second run it gives up:
  `moombox crashed N times in a row (last exit code X) — giving up; check the logs`.
  Each recovery logs the previous exit code and any crash output, and sends a
  **Recovered From Crash** notification.
- **Fail fast.** A fresh start that dies within 60 seconds is not restarted:
  something about the setup is wrong and retrying would only repeat it.
- **Startup errors** (exit code 3) are never restarted.
- **Update rollback.** The first start after an update is watched; see
  [Updating → If the new version fails to start](../getting-started/updating.md#if-the-new-version-fails-to-start).
- **Tied lifetimes.** If the launcher is killed outright, the program and its
  FFmpeg children go with it (a Windows job object; on Linux the kernel's
  parent-death signal), so no recorder is left running unsupervised.

`add`, `-version` and `-help` run in a single process and skip the launcher.

## One instance per user

Only one Moombox can run per user account at a time. A second start prints:

```text
another moombox instance is already running, or the last one is still shutting down (lock held on /home/you/.local/share/moombox/moombox.lock)
If you believe this is in error (the previous instance crashed), wait a few seconds and try again.
```

and exits with code 1. The lock is held by the operating system and released
when the process ends, so a crash never leaves a stale lock behind.

- On **Linux** it is a lock on `~/.local/share/moombox/moombox.lock`.
- On **Windows** it is a per-session mutex plus a lock on
  `%LOCALAPPDATA%\moombox\moombox.lock`, so an interactive copy and a
  scheduled-task copy for the same user also exclude each other.

The lock is per **user**, not per installation: two separate Moombox folders
cannot run at the same time under the same account. Different user accounts
can each run their own (on different ports).

## Signals and shutdown

Moombox stops gracefully on:

=== "Linux"

    - ++ctrl+c++ / `SIGINT`;
    - `SIGTERM` (`kill <pid>`, `systemctl stop`, `docker stop`);
    - `SIGHUP` (the terminal closing). If Moombox was started with hangups
      ignored, for example under `nohup`, it keeps running when the shell
      exits.

    Send the signal to either process; the launcher passes it on to the
    program.

=== "Windows"

    - ++ctrl+c++ or ++ctrl+break++ in the console;
    - closing the console window, logging off, or shutting down.

    Windows gives a closing console only a short time before it ends the
    process, so prefer quitting from the terminal UI or with ++ctrl+c++.

In the terminal UI, ++q++ ++q++ quits (the second ++q++ within three
seconds), and ++ctrl+c++ quits immediately from anywhere. Closing the
dashboard tab never stops Moombox.

A graceful shutdown stops the monitors, cancels running trims, gives active
downloads up to 10 seconds to save their state, cancels any mux still running
(it is redone from staging on the next start), flushes pending
notifications, stops the cookie services and the BotGuard helper, closes the
web server and checkpoints the database. If it takes longer than about
15 seconds in total, Moombox forces the exit. Allow at least that much in
service managers (`TimeoutStopSec=`, Docker's `stop_grace_period`).

A stop that arrives while a restart is pending wins: Moombox stops instead of
restarting.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Normal exit (quit, or a stop request). Also used when the shutdown had to be forced. |
| `1` | Another instance is running, the program could not be started, or `add` failed. |
| `2` | Unknown command or invalid flag. |
| `3` | Startup error: the configuration, database or web port is unusable. Not restarted by the launcher; fix the cause. |
| `129`, `130`, `143` | Stopped by `SIGHUP`, `SIGINT` or `SIGTERM` before the program was running (Linux). |

When the program itself exits, the launcher exits with the program's code;
after giving up on repeated crashes, with the last crash's code. The internal
restart code (42) never reaches you.

## Profiling

For memory or CPU investigations, start Moombox with `MOOMBOX_PPROF=1`. The
program then serves Go's standard profiling endpoints on
`http://localhost:6060/debug/pprof/` (loopback only, no authentication).
Without the variable nothing is started.

=== "Linux"

    ```bash
    MOOMBOX_PPROF=1 ./moombox
    # in another terminal
    go tool pprof http://localhost:6060/debug/pprof/heap
    curl "http://localhost:6060/debug/pprof/goroutine?debug=2"
    ```

=== "Windows (PowerShell)"

    ```powershell
    $env:MOOMBOX_PPROF = "1"
    .\Moombox.exe
    # in another terminal
    go tool pprof http://localhost:6060/debug/pprof/heap
    ```

Moombox also writes a memory line to the log every two minutes at `DEBUG`
level (`[Memory] Sys: … Heap: …`, with the BotGuard helper's memory when it is
running).

## See also

- [Installation](../getting-started/installation.md) — running as a systemd
  service.
- [Docker](../getting-started/docker.md) — the container's entrypoint and
  variables.
- [Keyboard shortcuts](shortcuts.md) — every terminal UI chord.
- [API](api.md) — automating Moombox over HTTP.
