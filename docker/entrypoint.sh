#!/bin/sh
# Moombox Docker entrypoint.
#
# Seeds /data/config.toml on first run, keeps /data/README-docker.txt (the
# operator guidance for the seeded settings) current, then execs the
# moombox binary so it receives signals directly (docker stop → SIGTERM →
# graceful shutdown).
#
# The seed sets network_access = "lan" — inside a container the dashboard
# is only reachable through the published port, and requests arriving over
# Docker's bridge network are never loopback from the container's point of
# view, so the binary's "localhost" default would make the dashboard
# unreachable (and the first-run web setup wizard, whose completion
# endpoint is loopback-gated, cannot be used either). Docker's port
# publishing is the exposure control: publish "127.0.0.1:774:774" to keep
# the dashboard host-only.
#
# The guidance is a file of its own because config.toml cannot keep it:
# Moombox saves config.toml by writing out its settings, comments dropped —
# on its first start, when it records the version that ran, and on every
# save after that (an update, any change from either UI). README-docker.txt
# is a file no save touches. It is written when missing and refreshed when
# this image's text differs from it, because the text describes the image:
# a copy frozen at the first install's text would keep an older image's
# advice forever. But only while it still holds, word for word, a text an
# entrypoint wrote — to it or beside it as README-docker.txt.new — which
# the checksums in .README-docker.txt.sha256 beside it record, one for
# every such text: a copy the operator edited is never overwritten. A text
# not given before goes to README-docker.txt.new beside it instead, and the
# log says so; a .new the operator moved over the copy is an entrypoint's
# text, so the next image refreshes it.
#
# An existing config.toml is never modified — change settings there or via
# the dashboard's Settings page as usual.
set -eu

CONFIG_PATH="${MOOMBOX_CONFIG:-/data/config.toml}"
GUIDE_PATH="$(dirname "$CONFIG_PATH")/README-docker.txt"
GUIDE_STAMP="$(dirname "$CONFIG_PATH")/.README-docker.txt.sha256"

# docker_guidance prints the text README-docker.txt holds.
docker_guidance() {
    cat <<'GUIDE'
Moombox in Docker
=================

This file is written by the Moombox image's entrypoint. It explains the
settings the entrypoint put in config.toml, beside it, when it created
that file on the first start, and how running Moombox in a container
differs from running it on a desktop.

config.toml itself keeps no comments. Moombox saves it by writing out
every setting with its value, which drops anything else in the file,
and it saves it often: on its first start, when it records the version
that ran; after each update; and whenever a setting or a channel is
changed from the dashboard or the TUI. This file is never saved over. A
newer image refreshes it only while it still holds an image's text word
for word. Once you edit it, your copy is kept, and an image whose text
is new puts it in README-docker.txt.new beside it instead, once: move
that over your copy to take it (later images then refresh the file
again), or delete it to keep your own.

Every setting, with its default and an explanation:
https://github.com/vampiricwulf/Moombox/blob/main/config.example.toml


[network]
---------

network_access = "lan"
  "lan" is the Docker default. The container is only reachable through
  published ports, and those connections arrive over the bridge network
  (not loopback), so the desktop default "localhost" would leave the
  dashboard unreachable. Restrict exposure with the port publish (e.g.
  "127.0.0.1:774:774"), or set "external" plus a dashboard password to
  allow non-private clients. For internet exposure, read "Remote access"
  in the README first (trusted_proxies, TLS, and the Tailscale/VPN
  option).

port = 774
  Must match the container side of the port publish ("774:774") and the
  docker-compose healthcheck: change all three together.

Behind a TLS-terminating reverse proxy (nginx, Caddy, Traefik)? See
trust_forwarded_proto in config.example.toml.


[paths]
-------

database_path, log_file_path, output_directory, staging_directory
  All under /data, the one volume: ./data on the host with the shipped
  docker-compose.yml.


[cookies]
---------

cookie_file = "/data/cookies.txt"
  Netscape cookie file for members-only content. Put it at
  ./data/cookies.txt on the host: the /data volume already exposes it.
  Do NOT bind-mount the file on its own. Moombox rewrites it (temp file
  plus rename) to keep the rotated YouTube session alive, and a rename
  cannot replace a single-file bind mount. See docker-compose.yml.

Alternative to exporting cookies.txt by hand: bind-mount a Firefox
profile directory (the one holding prefs.js and cookies.sqlite, not its
parent) at /data/browser-profile. browser_profile_dir needs no entry: it
defaults to ./browser-profile, which resolves to /data/browser-profile
here. This image ships no browser, so Moombox reads the profile directly
instead of launching one, and never writes into it. Copy the profile
with Firefox closed, and keep cookies.sqlite-wal next to cookies.sqlite:
recent cookies live in that sidecar, and a copy without it reads as
empty.

auto_enabled is left OFF here on purpose, and should stay off. All it
does is start a slow headless-browser refresh timer and allow one
automatic browser attempt when auth fails, and there is no browser in
this image to run either. It does NOT gate the profile import.

FIRST BOOT IS AUTOMATIC. If there is no cookies.txt yet, Moombox imports
the mounted profile once, shortly after start, on its own: there is
nothing on disk to lose, so mount a profile and start.

After that the profile is deliberately NOT re-read on a timer: nothing
inside the container changes it, so a timer would re-read identical
bytes over credentials that may still be working. YOU are what changes
it, so you trigger the read: refresh the profile on the host (Firefox
closed, .sqlite-wal included), then press "Refresh cookies from browser
profile" on the dashboard's Settings page, or shift+click the header's
"Refresh cookies", or press R F in the TUI. All three import straight
from the profile with no browser, whatever auto_enabled says and
whatever cookies.txt already holds. On a phone or tablet use the
Settings button: shift+click needs a keyboard.

NO PROFILE, OR THE PROFILE ITSELF IS STALE? Paste it instead. The
dashboard's Settings -> Cookies panel accepts a Netscape cookies.txt by
paste or upload: it MERGES into the file above (a YouTube-only export
leaves Twitch alone), reloads it immediately and reports whether it
authenticates. No browser, no shell, no access to the data volume,
which makes it the re-auth path for this image, and the only one that
works from a phone. Export from a private window and close it: browsing
on in the source profile invalidates it.

The in-process refresh that keeps the imported YouTube session alive
between imports runs regardless; it needs only cookie_file above.


[updates]
---------

auto_check_updates = false
  The in-app updater would swap the binary INSIDE the container, and
  that is silently reverted whenever the container is recreated from its
  image. Update by pulling a new image instead:
    docker compose pull && docker compose up -d
  Set it to true if you still want new-release notifications in the
  dashboard (the manual "Check for updates" button works either way).


[memory], [bgutils], [connectivity]
-----------------------------------

  Seeded empty, so their settings keep the built-in defaults (see
  config.example.toml). Moombox would otherwise add them itself on its
  first start.
GUIDE
}

# sha256_of prints the SHA-256 of its standard input.
sha256_of() {
    sha256sum | cut -d ' ' -f 1
}

# known_guidance reports whether the text with checksum $1 is one an
# entrypoint wrote, to README-docker.txt or beside it as .new.
known_guidance() {
    grep -qxF "$1" "$GUIDE_STAMP" 2>/dev/null
}

# remember_guidance records the checksum $1 among those texts, once.
remember_guidance() {
    known_guidance "$1" || printf '%s\n' "$1" >> "$GUIDE_STAMP"
}

# write_guidance keeps README-docker.txt current without overwriting an
# operator's edits. Every step checks for itself: it runs on the left of
# an || below, where set -e does not apply, and a failure must only be
# reported — the guidance never stops the container from starting.
write_guidance() {
    want=$(docker_guidance | sha256_of) || return 1
    [ -n "$want" ] || return 1 # no sha256sum: nothing to tell edits apart by
    if [ -e "$GUIDE_PATH" ] && [ ! -f "$GUIDE_PATH" ]; then
        return 1 # a directory, say: not a file this may replace
    fi
    if [ -f "$GUIDE_PATH" ]; then
        have=$(sha256_of < "$GUIDE_PATH") || return 1
        if [ "$have" = "$want" ]; then
            # Current already. Record it as this image's text, which it is
            # whoever put it there, so the next image may refresh it.
            remember_guidance "$want" || return 1
            rm -f "$GUIDE_PATH.new"
            return 0
        fi
        if ! known_guidance "$have"; then
            # Not a text an entrypoint wrote: the operator's. Keep it, and
            # say where this image's text is — once per text. A text
            # already given is not given again: a .new the operator
            # deleted stays deleted, and an edit of this image's own text
            # has nothing new to put beside it.
            if known_guidance "$want"; then
                return 0
            fi
            docker_guidance > "$GUIDE_PATH.new" || return 1
            remember_guidance "$want" || return 1
            echo "[moombox-entrypoint] $GUIDE_PATH has local edits, so it was kept; this image's guidance is in $GUIDE_PATH.new"
            return 0
        fi
        echo "[moombox-entrypoint] refreshing $GUIDE_PATH with this image's guidance"
    else
        echo "[moombox-entrypoint] writing $GUIDE_PATH (what the seeded settings mean)"
    fi
    # Temp file + rename: a start killed mid-write leaves the old text or
    # none, never half of one.
    docker_guidance > "$GUIDE_PATH.tmp" || return 1
    mv -f "$GUIDE_PATH.tmp" "$GUIDE_PATH" || return 1
    remember_guidance "$want" || return 1
    rm -f "$GUIDE_PATH.new"
}

if [ ! -f "$CONFIG_PATH" ]; then
    echo "[moombox-entrypoint] no config found — seeding $CONFIG_PATH (network_access = \"lan\")"
    cat > "$CONFIG_PATH" <<'CONFIG'
# Generated by the Moombox Docker entrypoint on first run.
# README-docker.txt, beside this file, explains each setting below. Read
# it there: Moombox rewrites this file without comments on its first
# start, and on every save after that.
# Only the settings a container needs are listed; the rest keep the
# built-in defaults. Full reference:
# https://github.com/vampiricwulf/Moombox/blob/main/config.example.toml

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

# Empty on purpose: the built-in defaults apply. Present so that
# Moombox has no section to add on its first start.
[memory]

[bgutils]

[connectivity]
CONFIG
fi

if ! write_guidance; then
    rm -f "$GUIDE_PATH.tmp" 2>/dev/null || true
    echo "[moombox-entrypoint] warning: could not write $GUIDE_PATH; starting without it" >&2
fi

# `moombox add <url>` takes the same -config as the daemon, so the job lands
# in the database the daemon is using.
exec moombox -config "$CONFIG_PATH" "$@"
