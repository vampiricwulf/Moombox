#!/usr/bin/env bash
# Run a network command, giving up on one attempt after a time limit and
# trying again a bounded number of times (.github/workflows/ci.yml).
#
#   retry.sh <attempts> <seconds per attempt, 0 for no limit> <command> [args...]
#
# For installers and downloads that fail transiently: a package mirror that
# trickles bytes (apt only gives up on total silence, so one such connection
# hung a CI run for 22 minutes), or a proxy that resets one HTTP/2 stream
# (`go mod tidy` died on "stream error: ... INTERNAL_ERROR; received from
# peer" fetching one module). The per-attempt limit needs GNU timeout; where
# there is none (a Windows runner whose PATH finds System32's timeout.exe
# first), attempts run unbounded and only the step's timeout-minutes applies.
# Pauses 5s, then 10s, ... between attempts. Exits with the last attempt's
# status.
set -uo pipefail

[ "$#" -ge 3 ] || {
	echo "usage: retry.sh <attempts> <seconds per attempt, 0 for no limit> <command> [args...]" >&2
	exit 2
}
attempts=$1 limit=$2
shift 2

guard=()
if [ "$limit" -gt 0 ] && timeout --version >/dev/null 2>&1; then
	guard=(timeout --kill-after=10 "$limit")
fi

for ((attempt = 1; ; attempt++)); do
	"${guard[@]}" "$@"
	status=$?
	[ "$status" -eq 0 ] && exit 0
	if { [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; } && [ "${#guard[@]}" -gt 0 ]; then
		why="no result after ${limit}s"
	else
		why="exit status $status"
	fi
	if [ "$attempt" -ge "$attempts" ]; then
		echo "::error::$* failed on all $attempts attempts (last: $why)"
		exit "$status"
	fi
	echo "::warning::$* failed on attempt $attempt of $attempts ($why); retrying in $((attempt * 5))s"
	sleep $((attempt * 5))
done
