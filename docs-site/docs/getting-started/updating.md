# Updating

Moombox can update itself from GitHub releases. It checks for a new version,
shows you the release notes, and when you say so downloads the new binary,
verifies it, swaps it in and restarts. If the new version fails to start, it
puts the old one back on its own.

YouTube and Twitch change their sites often, and fixes for that arrive as
releases, so keeping Moombox current matters more than it does for most
programs.

!!! note "Docker"
    In a container, update by pulling a new image; the in-app updater cannot
    install there. See [Docker → Updating the container](docker.md#updating-the-container).

## How you find out about a new release

With [`updates.auto_check_updates`](../configuration/updates-bgutils-twitch.md#updatesauto_check_updates)
on (the default outside Docker), Moombox asks GitHub for the latest release
about five seconds after it starts and then every 24 hours. Turning the
setting on while Moombox runs triggers a check within a minute; turning it off
stops the schedule without a restart. Only stable releases are offered;
pre-releases are never picked up by the check.

When a newer release exists:

- **Dashboard:** the version in the header changes to `vX.Y.Z ⬆`. Click it to
  open the **Update Available** dialog.
- **Terminal UI:** the feedback line says
  `Update available: vX.Y.Z — R N for notes, R U to install`, and the Details
  panel shows `⬆ Update!` beside the version.
- **Webhooks:** an **Update Available** notification (event
  `update_available`) is sent once per release while Moombox keeps running;
  after a restart the next automatic check announces it again.
  See [Notifications](../guide/notifications.md).

To check by hand at any time: **Check Now** under Settings → Updates in the
dashboard, or ++r++ ++v++ (Check for Updates) in the terminal UI. Manual
checks are rate-limited to protect your GitHub API allowance; if GitHub itself
refuses ("GitHub API rate limit exceeded"), try again later.

When no update is pending, clicking the header version once shows a hint, and
clicking again within three seconds opens the GitHub page. ++o++ ++g++ does
the same in the terminal UI.

## Reading the release notes

- **Dashboard:** the **Update Available** dialog shows the notes of the
  pending release. **View Release Notes** under Settings → Updates shows them
  too, or, when no update is pending, the notes of the version you are
  running.
- **Terminal UI:** ++r++ ++n++ opens the notes of the pending release, or of
  the running version when there is none. While a pending release's notes are
  open, press ++u++ to install it or ++s++ to skip it; ++up++ / ++down++
  scroll, ++esc++ or ++q++ closes.

## Installing an update

**Dashboard:** select **Update Now** in the **Update Available** dialog.
**Terminal UI:** press ++r++ ++u++ (Apply Update), or ++u++ inside the
release-notes view.

If downloads are active, you are asked to confirm first, because the restart
interrupts them. In the dashboard it is a confirmation dialog; in the terminal
UI, press ++r++ ++u++ again within five seconds. Recordings resume after the
restart, but live segments that pass during the gap (typically under half a
minute) can be lost, Twitch especially. Updating between streams is safest.

!!! warning "Updates install only from the Moombox machine"
    Replacing the program is accepted only from a browser running on the
    same computer as Moombox (connecting to `localhost`, `127.0.0.1` or `::1`)
    or from the terminal UI. A dashboard opened from another device still sees
    the dialog and can read the notes or skip the release, but **Update Now**
    is disabled with an explanation. This holds whatever
    [`network.network_access`](../configuration/network.md#networknetwork_access)
    is set to.

### What happens during an update

1. Moombox downloads the signed release manifest (`moombox-manifest.json` and
   its signature) and checks it. The manifest must be signed by the Moombox
   release key, name exactly the release being installed, name a version
   newer than the running one, and list a binary for your platform.
2. It downloads the new binary next to the running one, as `<binary>.new`, and
   its `.sig` signature file.
3. It verifies the binary's Ed25519 signature against the key built into
   Moombox, and checks that the file's SHA-256 matches the manifest's entry
   for your platform. Any failure deletes the download and stops; nothing is
   replaced.
4. The running binary is kept as `<binary>.old`, and the new one takes its
   name. On Linux the swap is a single rename, so the file name always holds
   a working binary. On Windows there is a split second in which the name is
   empty while the running program is moved aside, and during the restart the
   launcher renames `<binary>.old` to `<binary>~` (the file it is still
   running from).
5. Moombox shuts down gracefully (saving in-progress downloads) and its
   launcher starts the new binary. The dashboard shows
   **Update applied. Restarting...** and reconnects.
6. Once the new version has started successfully, it deletes the leftover
   files (`<binary>.old`, and any `.new` or `.failed`). On Windows,
   `<binary>~` is removed once the launcher that runs from it has exited. On
   the first start of the new version an
   **Update Applied** notification reports the old and new version and
   whether the dashboard came back.

A release that has no signed manifest is shown to you but cannot be
installed; it is refused at step 1. Releases from 2.9.0 onward all carry one.

Because the update writes files next to the binary, the folder it lives in
must be writable by the user running Moombox.

### If the new version fails to start

The launcher watches the first start after an update.

- **The new version crashes or exits with an error within two minutes:** the
  launcher rolls back automatically. The failed binary is kept as
  `<binary>.failed`, the previous binary is restored to the normal name and
  started, and a marker file `<binary>.update-failed` explains what happened.
  The failed version is marked as skipped, so automatic checks stop offering
  it; a manual check still shows it if you want to retry deliberately. On the
  next start a **Previous Update Failed** notification is sent and the
  details are logged. Delete the marker file once you have read it.
- **The new version reports a startup error** (a configuration it cannot
  load, a port it cannot bind, a database it refuses): that is your
  environment, not the release, so there is no rollback. Moombox stops and
  prints where the previous binary still is (`<binary>.old` on Linux,
  `<binary>~` on Windows). Fix the cause and start Moombox again.
- **The rollback itself fails:** the launcher leaves the previous binary where
  it is, writes the recovery steps into `<binary>.update-failed` and prints
  them. Recovery is one rename: put the previous binary (the path the
  marker names) back under the normal name.

A `<binary>.update-broken` marker means both placing the new binary and
restoring the old name failed during the swap. Moombox refuses further
updates until it is resolved; check which binary is in place and delete the
marker.

!!! note "Database downgrades"
    A new release may upgrade the database format. An older binary then
    refuses to open the database ("database schema vN is newer than this
    binary supports") and points at the newer binary, which a rollback keeps
    as `<binary>.failed`. If you replace binaries by hand, keep the newest one
    you have run.

### The launcher keeps running the old code

Moombox runs as two processes: a small launcher, which holds the console and
restarts the program after updates and setting changes, and the program
itself. An update replaces the program; the launcher process keeps running
the version it started with until you fully stop and start Moombox. Everything
works meanwhile, and the log says so at INFO level. Improvements to the
launcher itself take effect after the next full restart.

## Skipping a release

Select **Skip this version** in the dashboard dialog, or press ++s++ in the
terminal UI's release-notes view. The skipped release is no longer shown by
automatic checks and sends no further notifications; the next, different
release is announced normally. A manual check still finds the skipped release.
Skipping does not turn update checks off; that is the
[`updates.auto_check_updates`](../configuration/updates-bgutils-twitch.md#updatesauto_check_updates)
toggle (Settings → Updates → **Automatically check for updates**, or the
**Auto-check updates** row in the terminal UI's Settings).

## Verifying the installed binary

**Verify Signature** under Settings → Updates (or ++r++ ++s++ in the terminal
UI) downloads the signature and manifest of the release you are running and
checks your binary against them. The result is one of:

- *Signature and release manifest valid* — the binary is exactly the
  published release.
- *Signature valid — this release publishes no signed manifest, so only the
  signature was checked* — for releases older than 2.9.0.
- an error naming what did not match. A binary you built yourself will not
  verify.

## Updating by hand

You can also download the new binary from the releases page yourself, stop
Moombox, replace the file and start it again. On the next start Moombox
notices the version changed and sends the same **Update Applied**
notification. Your configuration, database and archives are untouched by an
update, whichever way it is done.

## Restarting without updating

Some settings only take effect after a restart. **Restart Moombox** under
Settings → System in the dashboard, or ++r++ ++p++ (Restart Program, confirm
with a third key press within three seconds) in the terminal UI, restarts the
program through the launcher. In-progress downloads are saved on shutdown and
resume on startup.
