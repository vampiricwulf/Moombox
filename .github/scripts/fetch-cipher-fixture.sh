#!/usr/bin/env bash
# Download the YouTube player the real-player cipher tests read
# (internal/cipher/testdata/player_cb017549.js), checked against a pinned
# SHA-256.
#
#   fetch-cipher-fixture.sh
#
# internal/cipher/testdata is gitignored: the file is YouTube's code, so it is
# fetched, never committed. The tests that read it skip without it, unless
# MOOMBOX_REQUIRE_CIPHER_FIXTURE=1 is set, which makes them fail instead. CI
# sets that only after this script succeeded (.github/workflows/ci.yml, the js
# job and the windows test shard that runs internal/bgutils/sidecar), so a
# YouTube outage or a retired player URL makes them skip with a warning
# rather than turn the run red.
#
# Does nothing when the file is already there with the pinned hash. Downloads
# to a temporary file and moves it into place only once the hash matches, so
# a failed or altered download leaves nothing behind. Exits 0 when the file
# is in place, 1 otherwise. CIPHER_FIXTURE_URL overrides the URL (to try the
# failure paths).
set -uo pipefail

url=${CIPHER_FIXTURE_URL:-https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js}
want=f1e42bbe7f76194b2d42325b8ab2e7308e2e91fdb2804f1f6abe781ed1490dc8

root=$(git rev-parse --show-toplevel) || exit 1
dest=$root/internal/cipher/testdata/player_cb017549.js

sha256() { sha256sum "$1" | awk '{ sub(/^\\/, "", $1); print $1 }'; }

if [ -f "$dest" ] && [ "$(sha256 "$dest")" = "$want" ]; then
	echo "fetch-cipher-fixture.sh: $dest is already in place"
	exit 0
fi

mkdir -p "$(dirname "$dest")" || exit 1
tmp=$dest.download
trap 'rm -f -- "$tmp"' EXIT

if ! curl -fsSL --retry 2 --retry-all-errors --retry-delay 5 --connect-timeout 15 --max-time 50 -o "$tmp" -- "$url"; then
	echo "fetch-cipher-fixture.sh: could not download $url" >&2
	exit 1
fi
got=$(sha256 "$tmp")
if [ "$got" != "$want" ]; then
	echo "fetch-cipher-fixture.sh: $url has SHA-256 $got, not the pinned $want" >&2
	exit 1
fi
mv -f -- "$tmp" "$dest" || exit 1
echo "fetch-cipher-fixture.sh: downloaded $dest"
