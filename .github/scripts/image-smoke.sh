#!/usr/bin/env bash
# Start the Moombox Docker image once and check what it did
# (.github/workflows/docker-publish.yml, before the multi-arch build).
#
#   image-smoke.sh <image> <commit> [<version>]
#
# Runs `<image> --version` with an empty directory mounted at /data. That
# goes through the image's entrypoint (docker/entrypoint.sh), which seeds
# /data/config.toml and writes /data/README-docker.txt before it execs
# `moombox -config /data/config.toml --version`; the binary answers
# --version before the launcher, the config or the database are touched. So
# one short run proves that the image starts, that the entrypoint runs to
# its exec on an empty volume, that the binary in the image is the one for
# this version and commit, and that both seeded files were written.
#
# <version> is what the image was built with (the VERSION build arg). Empty
# or absent means it was built without one, and the binary then reports the
# version cmd/moombox/main.go declares.
#
# The container runs as the calling user, so the files on the mount are the
# caller's and the check also covers the image's "works under a user:
# override" promise. The data directory is left in place for inspection.
set -euo pipefail

if [ "$#" -lt 2 ]; then
	echo "usage: image-smoke.sh <image> <commit> [<version>]" >&2
	exit 2
fi
image=$1
commit=$2
version=${3:-}

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

if [ -z "$version" ]; then
	version=$(sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' cmd/moombox/main.go | head -n 1)
	if [ -z "$version" ]; then
		echo "image-smoke.sh: could not read the version from cmd/moombox/main.go" >&2
		exit 1
	fi
fi

data=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/moombox-smoke.XXXXXX")

out=$(docker run --rm --user "$(id -u):$(id -g)" -v "$data:/data" "$image" --version)
echo "$out"

# The entrypoint's own lines come first; the binary's answer is the last.
got=$(printf '%s\n' "$out" | tail -n 1)
want="moombox $version ($commit)"
failed=0
if [ "$got" != "$want" ]; then
	echo "image-smoke.sh: $image --version ended with: $got" >&2
	echo "image-smoke.sh: expected:                    $want" >&2
	failed=1
fi

for name in config.toml README-docker.txt; do
	if [ ! -s "$data/$name" ]; then
		echo "image-smoke.sh: the entrypoint did not write /data/$name (looked in $data)" >&2
		failed=1
	fi
done

if [ "$failed" -ne 0 ]; then
	exit 1
fi
echo "image-smoke.sh: $image starts, reports $want and seeded /data"
