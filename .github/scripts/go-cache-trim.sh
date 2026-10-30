#!/usr/bin/env bash
# Decide when a restored Go build cache — or staticcheck's, which copies its
# logic — trims itself in a CI job (.github/workflows/ci.yml).
#
#   go-cache-trim.sh hold <cache dir>...  right after the restore: no trim for
#                                         the rest of the job
#   go-cache-trim.sh due <cache dir>...   the next command to close the cache
#                                         trims it
#   go-cache-trim.sh now                  trim GOCACHE here (before a save)
#
# A go command that builds anything (go build, vet, test, run or install; not
# go list) trims GOCACHE as it exits whenever trim.txt says the last trim was
# over a day ago, and staticcheck does the same to its own cache: every entry
# not used for five days goes. actions/cache restores a cache with the mtimes
# and the trim.txt it was saved with, so in a cache saved a week ago every
# entry looks unused for five days, and the job's first build (`go install
# staticcheck` in lint, `go run ./tools/fetch-node` in a test job whose Node
# cache missed) deleted all that the later steps were about to reuse.
#
# hold writes the current time into trim.txt: neither go nor staticcheck then
# trims for 24 hours. Without a trim a cache would only grow, since each
# save starts from the one restored (the restore-keys fallback), so a job
# that saves trims once, after its last use of the cache: due writes 0 into
# trim.txt and the next command to close the cache trims it, and now does
# that for GOCACHE with a build that compiles nothing (go build -n). What the
# job used has a fresh mtime by then, so the saved cache keeps it and drops
# only what no step used and nothing used for five days before.
set -euo pipefail

usage() {
	echo "usage: go-cache-trim.sh hold|due <cache dir>... | now" >&2
	exit 2
}

# posix <path>: a Windows path (from `go env GOCACHE`, say) as git-bash's
# tools want it; any other path unchanged.
posix() {
	if command -v cygpath >/dev/null 2>&1; then
		cygpath -u "$1"
	else
		echo "$1"
	fi
}

# stamp <value> <dir>...: write <value> into each cache's trim.txt.
stamp() {
	local value=$1 dir
	shift
	[ "$#" -gt 0 ] || usage
	for dir; do
		dir=$(posix "$dir")
		mkdir -p "$dir"
		echo "$value" >"$dir/trim.txt"
	done
}

case ${1:-} in
hold)
	shift
	stamp "$(date +%s)" "$@"
	;;
due)
	shift
	stamp 0 "$@"
	;;
now)
	[ "$#" -eq 1 ] || usage
	gocache=$(posix "$(go env GOCACHE)")
	stamp 0 "$gocache"
	echo "GOCACHE before the trim: $(du -sh "$gocache" | cut -f1)"
	if ! out=$(go build -n errors 2>&1); then
		echo "$out" >&2
		exit 1
	fi
	echo "GOCACHE after the trim:  $(du -sh "$gocache" | cut -f1)"
	;;
*)
	usage
	;;
esac
