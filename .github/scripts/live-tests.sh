#!/usr/bin/env bash
# One command for Moombox's live tests (owner ruling RV-15): no special
# settings, and the owner's session used without anyone reading it.
#
#   live-tests.sh [options] [group...]     run the groups (default: youtube-auth)
#   live-tests.sh --list                   print the groups and their tests
#   live-tests.sh --scan <log>             count the cookie values a saved log holds
#
# Groups (each package runs alone with -count=1 -v, an explicit -run of the
# group's live tests, and a timeout):
#   youtube-auth  the YouTube tests that use the session (the cheapest cookie group)
#   youtube       every YouTube live test: anonymous extraction, login markers, session
#   twitch        Twitch: token validation; the playlist and playback-token probes
#                 need MOOMBOX_LIVE_TWITCH_CHANNEL / _OFFLINE_CHANNEL in the settings
#   cookies       every test that reads the cookie file (YouTube and Twitch)
#   bgutils       the sidecar's live PO-token mints and its death fallback
#   cipher        the sidecar cipher solver against the player fixtures
#   all           youtube twitch bgutils cipher
#   goja          opt-in, NOT in all: the goja BotGuard check, expected to FAIL
#                 while BotGuard rejects goja
#   pot-accept    opt-in, NOT in all: TestGojaPotAcceptance, which exists only in
#                 the research worktree; use with --worktree and a video
# Excluded for good: TestLiveFirefoxRefreshWritesTheProfile (it launches a
# browser and writes its profile) and TestLivePgroupSeesAndKillsARealGroup
# (a Linux process-group smoke test with no service behind it).
#
# Options:
#   --run <regexp>     narrow each group to its tests whose name matches
#   --worktree <dir>   run the group tests in another Moombox tree (the session
#                      probe always runs from this script's own tree)
#   --video <id|url>   the video for pot-accept (else MOOMBOX_POT_ACCEPT_VIDEO)
#   --timeout <dur>    per package (default 10m, or MOOMBOX_LIVE_TIMEOUT)
#   --no-probe         skip the session probe that runs first
#   --no-cookies       run as if there were no cookie source
#
# Settings: .superpowers/live-tests.env in the main checkout (gitignored), one
# KEY=value per line; only these keys are read, the file is never sourced:
#   MOOMBOX_LIVE_COOKIE_SOURCE            the Netscape cookie file (default
#                                         D:/Moombox/cookies.txt when it exists)
#   MOOMBOX_LIVE_TWITCH_CHANNEL           a Twitch login that is live right now
#   MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL   a Twitch login that is offline right now
#   MOOMBOX_LIVE_YT_EXPECT_PREMIUM        yes|no: what the account is known to have
#   MOOMBOX_POT_ACCEPT_VIDEO, MOOMBOX_POT_ACCEPT_CLIENTS, MOOMBOX_POT_ACCEPT_PLAYER_POT
#   MOOMBOX_LIVE_TIMEOUT                  the per-package timeout
#
# THE COOKIE SOURCE IS READ-ONLY AND NEVER DISPLAYED. Only the helper
# (tools/live-tests) opens it: once, read-only, to make a private copy in a
# fresh 0700 directory under .superpowers/live-tmp, printing only its size,
# age and row counts per platform. Each package gets its own copy of that copy
# (code under test may rewrite the file it is given), and every MOOMBOX_LIVE_*
# switch is set here. Everything the tests print goes through the helper's
# redactor, which masks each cookie value of 8+ characters and the session
# headers and parameters, before it reaches the console or the log in
# .superpowers/live-logs. A trap removes the copies on exit, failure, Ctrl+C
# and TERM, and the run ends by counting the cookie values left in its own log
# (the self-check), which must be 0.
#
# Test hooks (the runner's own tests use them): MOOMBOX_LIVE_SETTINGS (the
# settings file), MOOMBOX_LIVE_STATE_DIR (instead of the main checkout's
# .superpowers), MOOMBOX_LIVE_GO_TEST (the go command the tests run under).
set -uo pipefail

usage() {
	sed -n '2,/^set -uo/{/^set -uo/d;s/^# \{0,1\}//;p;}' "$0" >&2
	exit 2
}

self_root=$(cd "$(dirname "$0")/../.." && pwd) || exit 1

# native <path>: the spelling a native Windows program reads; as is elsewhere.
native() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi
}

# ---- groups ---------------------------------------------------------------
# group_entries <group>: one "package<TAB>test,test,..." line per package.
group_entries() {
	case $1 in
	youtube-auth) printf 'internal/youtube\tTestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict\n' ;;
	youtube) printf 'internal/youtube\tTestLivePublicExtraction,TestLiveLoginMarkersPresent,TestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict\n' ;;
	twitch) printf 'internal/twitch\tTestLiveAuthenticatedTokenValidate,TestLiveTwitchMasterPlaylistParses,TestLivePlaybackTokenShape,TestLiveOfflineChannelPlaybackToken\n' ;;
	cookies)
		printf 'internal/youtube\tTestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict\n'
		printf 'internal/twitch\tTestLiveAuthenticatedTokenValidate,TestLivePlaybackTokenShape,TestLiveOfflineChannelPlaybackToken\n'
		;;
	bgutils)
		printf 'internal/bgutils\tTestSidecarFallsBackOnDeath\n'
		printf 'internal/bgutils/sidecar\tTestSidecarLivePoToken,TestSidecarLiveGvsMint\n'
		;;
	cipher) printf 'internal/cipher\tTestSidecarSolverLive,TestSidecarSolverGojaParity,TestSidecarSolverLiveTVRetry\n' ;;
	goja) printf 'internal/bgutils\tTestBotGuardLiveFingerprint\n' ;;
	pot-accept) printf 'internal/bgutils\tTestGojaPotAcceptance\n' ;;
	probe)
		printf 'internal/youtube\tTestLivePremiumVerdict\n'
		printf 'internal/twitch\tTestLiveAuthenticatedTokenValidate\n'
		;;
	*) return 1 ;;
	esac
}
known_groups="youtube-auth youtube twitch cookies bgutils cipher all goja pot-accept"

# ---- arguments ------------------------------------------------------------
groups=()
run_filter=""
tree=$self_root
video=""
timeout=""
probe=1
use_cookies=1
scan_log=""
while [ $# -gt 0 ]; do
	case $1 in
	--list)
		for g in $known_groups; do
			[ "$g" = all ] && { echo "all: youtube twitch bgutils cipher"; continue; }
			group_entries "$g" | while IFS=$'\t' read -r p t; do echo "$g: $p ${t//,/ }"; done
		done
		exit 0
		;;
	--scan) [ $# -ge 2 ] || usage; scan_log=$2; shift ;;
	--run) [ $# -ge 2 ] || usage; run_filter=$2; shift ;;
	--worktree) [ $# -ge 2 ] || usage; tree=$(cd "$2" && pwd) || exit 2; shift ;;
	--video) [ $# -ge 2 ] || usage; video=$2; shift ;;
	--timeout) [ $# -ge 2 ] || usage; timeout=$2; shift ;;
	--no-probe) probe=0 ;;
	--no-cookies) use_cookies=0 ;;
	-h | --help) usage ;;
	-*) usage ;;
	all) groups+=(youtube twitch bgutils cipher) ;;
	*)
		group_entries "$1" >/dev/null || { echo "unknown group: $1 (known: $known_groups)" >&2; exit 2; }
		groups+=("$1")
		;;
	esac
	shift
done
[ ${#groups[@]} -gt 0 ] || groups=(youtube-auth)

# ---- where things live ----------------------------------------------------
if [ -n "${MOOMBOX_LIVE_STATE_DIR:-}" ]; then
	state=$MOOMBOX_LIVE_STATE_DIR
else
	main=$(cd "$(git -C "$self_root" rev-parse --git-common-dir)/.." && pwd) || exit 1
	state=$main/.superpowers
fi
settings=${MOOMBOX_LIVE_SETTINGS:-$state/live-tests.env}
mkdir -p "$state/live-tmp" "$state/live-logs" || exit 1
if [ -z "${GOTMPDIR:-}" ] && command -v cygpath >/dev/null 2>&1; then
	mkdir -p "$state/gotmp"
	GOTMPDIR=$(cygpath -w "$state/gotmp")
	export GOTMPDIR
fi
export NO_COLOR=1
go_test=${MOOMBOX_LIVE_GO_TEST:-go}

# ---- settings (a whitelist; never sourced) --------------------------------
cookie_source="" twitch_channel="" twitch_offline="" expect_premium=""
pot_video="" pot_clients="" pot_player_pot="" settings_timeout=""
if [ -f "$settings" ]; then
	while IFS= read -r line || [ -n "$line" ]; do
		line=${line%$'\r'}
		case $line in '' | '#'*) continue ;; esac
		key=${line%%=*}
		val=${line#*=}
		key=${key//[[:space:]]/}
		val=${val#\"}; val=${val%\"}; val=${val#\'}; val=${val%\'}
		case $key in
		MOOMBOX_LIVE_COOKIE_SOURCE) cookie_source=$val ;;
		MOOMBOX_LIVE_TWITCH_CHANNEL) twitch_channel=$val ;;
		MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL) twitch_offline=$val ;;
		MOOMBOX_LIVE_YT_EXPECT_PREMIUM) expect_premium=$val ;;
		MOOMBOX_POT_ACCEPT_VIDEO) pot_video=$val ;;
		MOOMBOX_POT_ACCEPT_CLIENTS) pot_clients=$val ;;
		MOOMBOX_POT_ACCEPT_PLAYER_POT) pot_player_pot=$val ;;
		MOOMBOX_LIVE_TIMEOUT) settings_timeout=$val ;;
		*) echo "live-tests: ignored unknown settings key: $key" >&2 ;;
		esac
	done <"$settings"
fi
# The default source applies to a real run only: under the test hooks
# (MOOMBOX_LIVE_STATE_DIR) the owner's file can never be picked up.
if [ -z "$cookie_source" ] && [ -z "${MOOMBOX_LIVE_STATE_DIR:-}" ] && [ -f D:/Moombox/cookies.txt ]; then
	cookie_source=D:/Moombox/cookies.txt
fi
[ "$use_cookies" = 1 ] || cookie_source=""
[ -n "$video" ] || video=$pot_video
[ -n "$timeout" ] || timeout=${settings_timeout:-10m}

# ---- the private run directory and its trap -------------------------------
# Sweep run directories a killed runner left behind (its pid is gone).
for d in "$state"/live-tmp/run.*; do
	[ -d "$d" ] || continue
	pid=$(cat "$d/pid" 2>/dev/null || true)
	if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
		rm -rf "$d"
	fi
done
run_dir=$(mktemp -d "$state/live-tmp/run.XXXXXX") || exit 1
chmod 700 "$run_dir"
echo $$ >"$run_dir/pid"

cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	if [ -n "${run_dir:-}" ] && [ -e "$run_dir" ]; then
		for _ in 1 2 3 4 5; do
			rm -rf "$run_dir" 2>/dev/null
			[ -e "$run_dir" ] || break
			sleep 1
		done
		if [ -e "$run_dir" ]; then
			echo "live-tests: LEFTOVER: could not remove $run_dir (a process still holds it); delete it by hand" >&2
			rc=1
		fi
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the helper -----------------------------------------------------------
helper=$run_dir/live-tests-helper
case $(uname -s) in MINGW* | MSYS* | CYGWIN*) helper=$helper.exe ;; esac
if ! (cd "$self_root" && go build -o "$(native "$helper")" ./tools/live-tests); then
	echo "live-tests: cannot build the helper (tools/live-tests)" >&2
	exit 1
fi

# ---- the private copy -----------------------------------------------------
pristine=""
rows_youtube=0 rows_twitch=0
cookie_line="Cookie source: none (cookie tests skip)"
if [ -n "$cookie_source" ]; then
	if [ ! -f "$cookie_source" ]; then
		cookie_line="Cookie source: missing (cookie tests skip)"
	else
		pristine=$run_dir/source-copy.txt
		stats=$("$helper" copy "$(native "$cookie_source")" "$(native "$pristine")") || {
			echo "live-tests: could not copy the cookie source" >&2
			exit 1
		}
		for kv in $stats; do
			case $kv in
			size=*) size=${kv#size=} ;;
			age_s=*) age_s=${kv#age_s=} ;;
			rows_youtube=*) rows_youtube=${kv#rows_youtube=} ;;
			rows_twitch=*) rows_twitch=${kv#rows_twitch=} ;;
			rows_other=*) rows_other=${kv#rows_other=} ;;
			esac
		done
		cookie_line="Cookie source: present, ${size} bytes, $((age_s / 60)) min old; rows: youtube=${rows_youtube} twitch=${rows_twitch} other=${rows_other}"
	fi
fi

if [ -n "$scan_log" ]; then
	[ -n "$pristine" ] || { echo "live-tests: --scan needs a cookie source" >&2; exit 2; }
	n=$("$helper" scan --cookies "$(native "$pristine")" "$(native "$scan_log")")
	echo "cookie values found in $scan_log: $n"
	[ "$n" = 0 ]
	exit
fi

log=$state/live-logs/live-$(date +%Y%m%d-%H%M%S).log
: >"$log"
copy_n=0

# run_pkg <group> <tree> <package> <comma-separated tests>
run_pkg() {
	local group=$1 dir=$2 pkg=$3 tests=$4 names=() t pattern work="" rc rrc
	local -a envs=(
		-u MOOMBOX_LIVE_BROWSER_REFRESH -u MOOMBOX_LIVE_BROWSER_PATH -u MOOMBOX_LIVE_PGROUP
		-u MOOMBOX_LIVE_GOJA_TEST -u MOOMBOX_LIVE_YT_COOKIES -u MOOMBOX_LIVE_TWITCH_COOKIES
		-u MOOMBOX_LIVE_TWITCH_PROBE -u MOOMBOX_POT_ACCEPT -u MOOMBOX_POT_ACCEPT_COOKIES
		-u MOOMBOX_LIVE_TWITCH_CHANNEL -u MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL
		MOOMBOX_LIVE_YT_TEST=1 MOOMBOX_LIVE_BG_TEST=1 MOOMBOX_LIVE_CIPHER_TEST=1 MOOMBOX_LIVE_TWITCH_TEST=1
	)
	IFS=, read -r -a names <<<"$tests"
	local keep=()
	for t in "${names[@]}"; do
		if [ -z "$run_filter" ] || [[ $t =~ $run_filter ]]; then keep+=("$t"); fi
	done
	[ ${#keep[@]} -gt 0 ] || return 0
	pattern="^($(IFS='|'; echo "${keep[*]}"))\$"

	if [ -n "$pristine" ]; then
		copy_n=$((copy_n + 1))
		work=$run_dir/work-$copy_n.txt
		"$helper" copy "$(native "$pristine")" "$(native "$work")" >/dev/null || return 1
		# pot-accept takes its copy through its own switch only (below).
		if [ "$group" != pot-accept ]; then
			[ "$rows_youtube" -gt 0 ] && envs+=(MOOMBOX_LIVE_YT_COOKIES="$(native "$work")")
			[ "$rows_twitch" -gt 0 ] && envs+=(MOOMBOX_LIVE_TWITCH_COOKIES="$(native "$work")")
		fi
	fi
	[ -n "$expect_premium" ] && envs+=(MOOMBOX_LIVE_YT_EXPECT_PREMIUM="$expect_premium")
	[ -n "$twitch_channel" ] && envs+=(MOOMBOX_LIVE_TWITCH_CHANNEL="$twitch_channel")
	[ -n "$twitch_offline" ] && envs+=(MOOMBOX_LIVE_TWITCH_PROBE=1 MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL="$twitch_offline")
	[ "$group" = goja ] && envs+=(MOOMBOX_LIVE_GOJA_TEST=1)
	if [ "$group" = pot-accept ]; then
		envs+=(MOOMBOX_POT_ACCEPT=1 MOOMBOX_POT_ACCEPT_VIDEO="$video")
		[ -n "$work" ] && [ "$rows_youtube" -gt 0 ] && envs+=(MOOMBOX_POT_ACCEPT_COOKIES="$(native "$work")")
		[ -n "$pot_clients" ] && envs+=(MOOMBOX_POT_ACCEPT_CLIENTS="$pot_clients")
		[ -n "$pot_player_pot" ] && envs+=(MOOMBOX_POT_ACCEPT_PLAYER_POT="$pot_player_pot")
	fi

	local redact
	if [ -n "$pristine" ]; then
		redact=(--cookies "$(native "$pristine")")
	else
		# No cookie file: still mask the session shapes. An empty file is
		# readable, so the redactor starts.
		: >"$run_dir/empty.txt"
		redact=(--cookies "$(native "$run_dir/empty.txt")")
	fi
	[ -n "$work" ] && redact+=(--cookies "$(native "$work")")

	echo "### live-tests begin group=$group pkg=$pkg" | tee -a "$log"
	(cd "$dir" && env "${envs[@]}" "$go_test" test -count=1 -v -timeout "$timeout" -run "$pattern" "./$pkg" </dev/null) 2>&1 |
		"$helper" redact "${redact[@]}" | tee -a "$log"
	rc=${PIPESTATUS[0]} rrc=${PIPESTATUS[1]}
	echo "### live-tests end group=$group pkg=$pkg exit=$rc" | tee -a "$log"
	if [ "$rrc" != 0 ]; then
		echo "live-tests: the redactor failed; stopping" >&2
		exit 1
	fi
}

echo "live-tests: log $log"
echo "$cookie_line" | tee -a "$log"
if [ "$probe" = 1 ]; then
	while IFS=$'\t' read -r p t; do run_pkg probe "$self_root" "$p" "$t"; done < <(group_entries probe)
fi
for g in "${groups[@]}"; do
	if [ "$g" = pot-accept ] && [ -z "$video" ]; then
		echo "### live-tests skip group=pot-accept reason=no video: pass --video or set MOOMBOX_POT_ACCEPT_VIDEO" | tee -a "$log"
		continue
	fi
	while IFS=$'\t' read -r p t; do run_pkg "$g" "$tree" "$p" "$t"; done < <(group_entries "$g")
done

echo
echo "==== summary ===="
echo "$cookie_line"
summary=$("$helper" summary "$(native "$log")")
echo "$summary"
status=0
case $summary in *"RESULT: FAIL"*) status=1 ;; esac

# Self-check: every copy this run made, the rewritten ones included.
copies=()
for f in "$run_dir"/source-copy.txt "$run_dir"/work-*.txt; do
	[ -f "$f" ] && copies+=(--cookies "$(native "$f")")
done
if [ ${#copies[@]} -gt 0 ]; then
	n=$("$helper" scan "${copies[@]}" "$(native "$log")")
	echo "Self-check: $n cookie values in the saved log"
	[ "$n" = 0 ] || status=1
fi
{
	echo "==== summary ===="
	echo "$summary"
} >>"$log"
echo "Log: $log"
exit $status
