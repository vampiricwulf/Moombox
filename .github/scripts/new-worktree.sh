#!/usr/bin/env bash
# Make a worktree that can build and run the tests straight away.
#
#   new-worktree.sh <branch> [<start-point>]
#
# Creates branch <branch> from <start-point> (default: main) in
# .worktrees/<branch, with each / turned into -> under the main checkout, then
# gives it what git does not carry, all copied from the main checkout:
#
#   - the embed blobs in internal/bgutils/embed/ (the sidecar tarball and the
#     Node binaries), without which most packages do not compile;
#   - the player fixtures in internal/cipher/testdata/, which the cipher tests
#     read;
#   - web/tests/node_modules, by `npm ci` there, so the DOM suites run.
#
# It ends with `go build ./...` as proof. Run it from anywhere inside the
# repository. It deletes nothing and refuses a branch or directory that
# exists. When the main checkout has no blobs to copy, or one it copies is an
# empty stand-in, it says so: fetch them
# there first (CLAUDE.md, "BotGuard sidecar embed prerequisites"), or run
# embed-standins.sh in the worktree for compile-only work.
#
# The sidecar's own `npm test` needs `npm ci && node build.mjs` in the
# worktree's bgutil-sidecar/; this script leaves that to whoever needs it.
set -euo pipefail

case ${1:-} in
-h | --help)
	sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p;}' "$0" >&2
	exit 2
	;;
-*)
	echo "new-worktree.sh: a branch name cannot start with -" >&2
	exit 2
	;;
esac

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || {
	echo "usage: new-worktree.sh <branch> [<start-point>]" >&2
	exit 2
}
branch=$1
start=${2:-main}

# The main checkout is the parent of the git directory all worktrees share.
main=$(cd "$(git rev-parse --git-common-dir)/.." && pwd)
wt=$main/.worktrees/${branch//\//-}

[ ! -e "$wt" ] || {
	echo "new-worktree.sh: $wt already exists" >&2
	exit 1
}

cd "$main"
git worktree add -b "$branch" "$wt" "$start"

copied=0
while read -r f; do
	[ -n "$f" ] || continue
	mkdir -p "$wt/$(dirname "$f")"
	cp -- "$f" "$wt/$f"
	case $f in
	internal/bgutils/embed/*)
		[ -s "$f" ] || echo "new-worktree.sh: $f is an empty stand-in in the main checkout; replace it there and here before building anything to run" >&2
		;;
	esac
	copied=$((copied + 1))
done <<<"$(git ls-files --others --ignored --exclude-standard -- internal/bgutils/embed/ internal/cipher/testdata/)"
echo "copied $copied ignored files (embed blobs, cipher fixtures)"

if [ -z "$(git ls-files --others --ignored --exclude-standard -- internal/bgutils/embed/)" ]; then
	echo "new-worktree.sh: the main checkout has no embed blobs to copy; the build below will fail until the worktree has them or stand-ins" >&2
fi

if command -v npm >/dev/null 2>&1; then
	(cd "$wt/web/tests" && npm ci --silent --no-audit --no-fund)
else
	echo "new-worktree.sh: npm not found, so the frontend DOM suites will skip" >&2
fi

if [ -z "${GOTMPDIR:-}" ] && command -v cygpath >/dev/null 2>&1; then
	# See local-gate.sh for why Go gets its own temp directory, spelled the
	# Windows way.
	mkdir -p "$main/.superpowers/gotmp"
	GOTMPDIR=$(cygpath -w "$main/.superpowers/gotmp")
	export GOTMPDIR
fi

cd "$wt"
go build ./...
echo "READY $wt"
