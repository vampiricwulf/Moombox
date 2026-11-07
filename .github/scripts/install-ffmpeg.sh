#!/usr/bin/env bash
# Install FFmpeg on a GitHub runner (.github/workflows/ci.yml) — apt on
# Linux, choco on Windows — for the Go tests that run it
# (muxer_concatcopy_test.go and probe_params_test.go skip without it).
#
#   install-ffmpeg.sh          install now
#   install-ffmpeg.sh --start  start the install in the background and return
#   install-ffmpeg.sh --wait   wait for that install, print its log, and exit
#                              with its status
#
# The install takes 20-40 s and needs nothing the job's other setup steps
# make, so a test job starts it first and waits for it just before go test.
# A background process outlives the step that started it; the runner reaps
# what is left when the job ends.
#
# Every network command is retried (retry.sh), and apt's get a time limit per
# attempt too, since apt gives up only on total silence: a mirror that
# trickled bytes once hung a run for 22 minutes. apt downloads first and then
# installs with --no-download, so the part that cannot be cut off half-way
# never waits on a mirror; that offline install runs once, unretried. The
# tests need only the ffmpeg and ffprobe programs, so apt leaves out the
# recommended GPU and VA drivers, most of the ~94 MB a full install fetched;
# a download attempt gets 150 s, and a retry resumes what the last one
# fetched (apt keeps finished packages and partials in its cache). choco
# gets no per-attempt limit (cutting an install off half-way is worse than
# waiting), so a hung attempt is never retried: the waiting step's
# timeout-minutes is its bound, and the bound of the whole.
set -euo pipefail

here=$(dirname "$0")
temp=${RUNNER_TEMP:?RUNNER_TEMP is not set}
if command -v cygpath >/dev/null 2>&1; then
	temp=$(cygpath -u "$temp")
fi
state=$temp/ffmpeg-install

install() {
	case ${RUNNER_OS:-$(uname -s)} in
	Linux)
		# The .debs land in a runner-owned directory that ci.yml restores
		# from and saves to the Actions cache, so a run whose cache hit
		# fetches nothing (or only a newer package) from the Ubuntu mirror,
		# which stalls mid-package often enough to have failed whole runs.
		debs=${FFMPEG_APT_CACHE:-$HOME/.cache/apt-ffmpeg}
		mkdir -p "$debs/partial"
		echo "Cached FFmpeg packages: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"
		bash "$here/retry.sh" 3 90 sudo apt-get update -q &&
			bash "$here/retry.sh" 3 150 sudo apt-get install -y -q --no-install-recommends --download-only \
				-o Dir::Cache::archives="$debs" ffmpeg &&
			sudo apt-get install -y -q --no-install-recommends --no-download \
				-o Dir::Cache::archives="$debs" ffmpeg &&
			{
				# A cache restored from an older runner image (restore-keys)
				# holds versions apt no longer offers; drop them so the saved
				# cache stays the size of one install.
				sudo apt-get autoclean -q -o Dir::Cache::archives="$debs" || true
			} &&
			sudo chmod -R a+rX "$debs"
		;;
	Windows)
		bash "$here/retry.sh" 3 0 choco install ffmpeg -y --no-progress
		;;
	*)
		echo "install-ffmpeg.sh: no installer for ${RUNNER_OS:-$(uname -s)}" >&2
		return 1
		;;
	esac || return
	ffmpeg -version | head -1
}

case ${1:-} in
"")
	install
	;;
--start)
	rm -rf "$state"
	mkdir -p "$state"
	bash "$0" --run >"$state/log" 2>&1 </dev/null &
	echo "$!" >"$state/pid"
	echo "Installing FFmpeg in the background (log: $state/log)."
	;;
--run)
	# Always leave a status for --wait, whatever ends the install.
	trap 'status=$?; echo "$status" >"$state/status.tmp" && mv "$state/status.tmp" "$state/status"' EXIT
	start=$(date +%s)
	install
	echo "FFmpeg installed in $(($(date +%s) - start)) s."
	;;
--wait)
	[ -d "$state" ] || {
		echo "install-ffmpeg.sh: no background install was started (run --start first)" >&2
		exit 1
	}
	waited=0
	until [ -s "$state/status" ]; do
		if ! kill -0 "$(cat "$state/pid")" 2>/dev/null; then
			sleep 2 # it may have written its status on the way out
			[ -s "$state/status" ] && break
			cat "$state/log"
			echo "::warning::The background FFmpeg install ended without a status; installing in the foreground."
			install
			exit
		fi
		sleep 1
		waited=$((waited + 1))
	done
	cat "$state/log"
	status=$(cat "$state/status")
	echo "Waited ${waited} s for the background install; it exited $status."
	exit "$status"
	;;
*)
	echo "usage: install-ffmpeg.sh [--start | --wait]" >&2
	exit 2
	;;
esac
