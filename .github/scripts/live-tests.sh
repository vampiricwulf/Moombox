#!/usr/bin/env bash
# One command for Moombox's live tests (owner ruling RV-15): no special
# settings, and the owner's session used without anyone reading it.
#
#   live-tests.sh [options] [group...]     run the groups (default: all)
#   live-tests.sh --list                   print the groups and their tests
#   live-tests.sh --scan <log>             count the secrets a saved log holds (-v: where)
#   live-tests.sh --sweep                  remove what a killed run left; run nothing
#
# Groups (each package runs alone with -count=1 -v, an explicit -run of the
# group's live tests, and a timeout; a test two groups share runs once):
#   youtube-auth  the YouTube tests that use the session (the quick check)
#   youtube       every YouTube live test: anonymous extraction, login markers,
#                 the session, the membership tab, a format URL served
#   twitch        Twitch: token validation, the master playlist and the playback
#                 token of a channel live now (picked from Twitch's top streams
#                 unless one is set); the offline-channel probe needs
#                 MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL
#   cookies       every test that reads the cookie file (YouTube and Twitch;
#                 the Twitch channel is picked as for twitch)
#   bgutils       the sidecar's live PO-token mints and its death fallback
#   cipher        the sidecar cipher solver against the player fixtures
#                 (skipped when the tree has none), and today's player (main
#                 and tv bodies) against sidecar and goja, which needs none
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
#   --run <regexp>           narrow each group to its tests whose name matches
#   --worktree <dir>         run the group tests in another Moombox tree (the
#                            session probe always runs from this script's tree)
#   --video <id|url>         the video for pot-accept (else MOOMBOX_POT_ACCEPT_VIDEO)
#   --twitch-channel <login> the live channel for the Twitch channel tests
#   --timeout <dur>          per package (default 10m, or MOOMBOX_LIVE_TIMEOUT)
#   --no-probe               skip the session probe that runs first
#   --no-cookies             run as if there were no cookie source
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
# fresh run directory under .superpowers/live-tmp that only this user can
# read (icacls on Windows, 0700 elsewhere), printing only its size, age and
# row counts per platform. Each package that reads cookies gets its own copy
# of that copy (code under test may rewrite the file it is given); every
# other package gets none. Everything a test creates lands in the run
# directory too (TMP, TEMP, TMPDIR and GOTMPDIR point there). Everything the
# tests print goes through the helper's redactor (tools/live-tests/redact.go
# says what it masks) before it reaches the console or the log in
# .superpowers/live-logs. The run directory is removed on exit, failure,
# Ctrl+C and TERM, after the test processes are ended by their exact PIDs.
# When the runner's own bash is ended with no chance to trap, or partway
# through its cleanup (taskkill /F, SIGKILL, a signal and then a hard kill),
# a watchdog it started does the same once it sees the runner gone. A
# directory a kill of the whole tree left is removed by the next run, or by
# --sweep (local-gate.sh and new-worktree.sh call it), once its run is gone or past
# its deadline. The run ends with a second, independent reading of its own
# log (the self-check), which must find 0; a log it finds anything in is
# redacted again in place, and deleted if that does not clear it. A log a
# killed run left without a summary is checked the same way by the next run.
#
# Test hooks (the runner's own tests use them; MOOMBOX_LIVE_SETTINGS and
# MOOMBOX_LIVE_GO_TEST are honoured only with MOOMBOX_LIVE_STATE_DIR set):
# MOOMBOX_LIVE_STATE_DIR (instead of the main checkout's .superpowers),
# MOOMBOX_LIVE_SETTINGS (the settings file), MOOMBOX_LIVE_GO_TEST (the go
# command the tests run under), MOOMBOX_LIVE_TWITCH_PICK (the channel the
# Twitch pick returns, instead of asking Twitch), MOOMBOX_LIVE_TRACE (a file
# the runner appends its trap, kill and watchdog steps to) and
# MOOMBOX_LIVE_KILL_PAUSE (seconds the cleanup pauses after ending the first
# process of its list, so a test can hard-kill it there); both honoured only
# with MOOMBOX_LIVE_STATE_DIR set.
set -uo pipefail

usage() {
	sed -n '2,/^set -uo/{/^set -uo/d;s/^# \{0,1\}//;p;}' "$0" >&2
	exit 2
}

self_root=$(cd "$(dirname "$0")/../.." && pwd) || exit 1
on_windows=0
case $(uname -s) in MINGW* | MSYS* | CYGWIN*) on_windows=1 ;; esac

# native <path>: the spelling a native Windows program reads; as is elsewhere.
native() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi
}
# native_bs <path>: the backslash spelling (GOTMPDIR, TMP and TEMP want it).
native_bs() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}

# private_dir <dir>: only this user may open it. Fails when that cannot be
# made so.
private_dir() {
	if [ "$on_windows" = 1 ]; then
		MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' icacls "$(native_bs "$1")" /inheritance:r \
			/grant:r "${USERNAME:?USERNAME is not set}:(OI)(CI)F" >/dev/null || return 1
	else
		chmod 700 "$1" || return 1
	fi
}

# ---- groups ---------------------------------------------------------------
# group_entries <group>: one "package<TAB>test,test,..." line per package.
group_entries() {
	case $1 in
	youtube-auth) printf 'internal/youtube\tTestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict,TestLiveMembershipTabParses\n' ;;
	youtube) printf 'internal/youtube\tTestLivePublicExtraction,TestLiveLoginMarkersPresent,TestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict,TestLiveMembershipTabParses,TestLiveFormatURLServes\n' ;;
	twitch) printf 'internal/twitch\tTestLiveAuthenticatedTokenValidate,TestLiveTwitchMasterPlaylistParses,TestLivePlaybackTokenShape,TestLiveOfflineChannelPlaybackToken\n' ;;
	cookies)
		printf 'internal/youtube\tTestLiveAuthenticatedAccountProbe,TestLivePremiumVerdict,TestLiveMembershipTabParses,TestLiveFormatURLServes\n'
		printf 'internal/twitch\tTestLiveAuthenticatedTokenValidate,TestLivePlaybackTokenShape,TestLiveOfflineChannelPlaybackToken\n'
		;;
	bgutils)
		printf 'internal/bgutils\tTestSidecarFallsBackOnDeath\n'
		printf 'internal/bgutils/sidecar\tTestSidecarLivePoToken,TestSidecarLiveGvsMint\n'
		;;
	cipher)
		printf 'internal/cipher\tTestSidecarSolverLive,TestSidecarSolverGojaParity,TestSidecarSolverLiveTVRetry\n'
		printf 'internal/youtube\tTestLiveCurrentPlayerSolves\n'
		;;
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
scan_verbose=""
sweep_only=0
channel_opt=""
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
	-v) scan_verbose=-v ;;
	--sweep) sweep_only=1 ;;
	--run) [ $# -ge 2 ] || usage; run_filter=$2; shift ;;
	--worktree) [ $# -ge 2 ] || usage; tree=$(cd "$2" && pwd) || exit 2; shift ;;
	--video) [ $# -ge 2 ] || usage; video=$2; shift ;;
	--twitch-channel) [ $# -ge 2 ] || usage; channel_opt=$2; shift ;;
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
[ ${#groups[@]} -gt 0 ] || groups=(youtube twitch bgutils cipher)

# ---- where things live ----------------------------------------------------
hooks=0
if [ -n "${MOOMBOX_LIVE_STATE_DIR:-}" ]; then
	state=$MOOMBOX_LIVE_STATE_DIR
	hooks=1
else
	# git names the main checkout's common dir relative to $self_root
	# (".git"), so it is resolved from there, not from the caller's cwd.
	main=$(cd "$self_root" && cd "$(git rev-parse --git-common-dir)/.." && pwd) || exit 1
	state=$main/.superpowers
fi
settings=$state/live-tests.env
go_test=go
if [ "$hooks" = 1 ]; then
	settings=${MOOMBOX_LIVE_SETTINGS:-$settings}
	go_test=${MOOMBOX_LIVE_GO_TEST:-go}
fi
mkdir -p "$state/live-tmp" "$state/live-logs" || exit 1

# ---- the sweep: what a killed run left ------------------------------------
# A run directory (and the lock) holds "owner": the runner's pid and the
# epoch second past which no run of it can still be going. It is stale when
# that pid is gone or that moment has passed (pids are reused, deadlines are
# not). The rm is checked.
stale() {
	local pid deadline
	read -r pid deadline <"$1/owner" 2>/dev/null || return 0
	[ -n "${deadline:-}" ] && [ "$(date +%s)" -gt "$deadline" ] && return 0
	[ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null && return 1
	return 0
}
sweep() {
	local d bad=0
	for d in "$state"/live-tmp/run.* "$state"/live-tmp/lock; do
		[ -d "$d" ] || continue
		stale "$d" || continue
		rm -rf "$d" 2>/dev/null
		if [ -e "$d" ]; then
			echo "live-tests: LEFTOVER: could not remove $d (a process still holds it); delete it by hand" >&2
			bad=1
		fi
	done
	return $bad
}
if [ "$sweep_only" = 1 ]; then
	sweep
	exit
fi

# One run at a time: the bgutils and cipher tests depend on timing, and two
# runs would share nothing safely.
if ! mkdir "$state/live-tmp/lock" 2>/dev/null; then
	if stale "$state/live-tmp/lock"; then
		rm -rf "$state/live-tmp/lock"
		mkdir "$state/live-tmp/lock" 2>/dev/null || { echo "live-tests: another run holds $state/live-tmp/lock" >&2; exit 1; }
	else
		echo "live-tests: another live run is going (pid $(cut -d' ' -f1 "$state/live-tmp/lock/owner" 2>/dev/null)); wait for it" >&2
		exit 1
	fi
fi
lock=$state/live-tmp/lock
echo "$$ $(($(date +%s) + 600))" >"$lock/owner"
sweep || true

if [ -z "${GOTMPDIR:-}" ] && command -v cygpath >/dev/null 2>&1; then
	mkdir -p "$state/gotmp"
	GOTMPDIR=$(cygpath -w "$state/gotmp")
	export GOTMPDIR
fi
export NO_COLOR=1

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
# The default source applies to a real run only: under the test hooks the
# owner's file can never be picked up.
if [ -z "$cookie_source" ] && [ "$hooks" = 0 ] && [ -f D:/Moombox/cookies.txt ]; then
	cookie_source=D:/Moombox/cookies.txt
fi
[ "$use_cookies" = 1 ] || cookie_source=""
[ -n "$video" ] || video=$pot_video
[ -n "$channel_opt" ] && twitch_channel=$channel_opt
[ -n "$timeout" ] || timeout=${settings_timeout:-10m}

# duration_s <go duration>: seconds, for the deadline (h, m, s; else a day).
duration_s() {
	local d=$1 total=0 n
	while [[ $d =~ ^([0-9]+)(h|m|s)(.*)$ ]]; do
		n=${BASH_REMATCH[1]}
		case ${BASH_REMATCH[2]} in h) total=$((total + n * 3600)) ;; m) total=$((total + n * 60)) ;; s) total=$((total + n)) ;; esac
		d=${BASH_REMATCH[3]}
	done
	[ -z "$d" ] && [ "$total" -gt 0 ] && echo "$total" || echo 86400
}
n_pkgs=2
for g in "${groups[@]}"; do n_pkgs=$((n_pkgs + $(group_entries "$g" | wc -l))); done
deadline=$(($(date +%s) + n_pkgs * $(duration_s "$timeout") + 1800))
echo "$$ $deadline" >"$lock/owner"

# ---- the private run directory and its trap -------------------------------
run_dir=$(mktemp -d "$state/live-tmp/run.XXXXXX") || exit 1
owner_line="$$ $deadline"
echo "$owner_line" >"$run_dir/owner"
bg_pid=""
watch_pid=""

# trace <text>: one line in the runner's trace (a test hook: the runner's
# tests print it when they fail). Never a cookie value or a path to one.
trace_file=""
[ "$hooks" = 1 ] && trace_file=${MOOMBOX_LIVE_TRACE:-}
kill_pause=""
[ "$hooks" = 1 ] && kill_pause=${MOOMBOX_LIVE_KILL_PAUSE:-}
trace() {
	[ -n "$trace_file" ] && printf '%s\n' "$*" >>"$trace_file" 2>/dev/null
	return 0
}
trace "runner pid=$$ winpid=$(cat "/proc/$$/winpid" 2>/dev/null || echo -) bash=$BASH_VERSION $(uname -sr 2>/dev/null)"

# taskkill, by its full path when PATH lacks it; empty when there is none.
taskkill_cmd=""
if [ "$on_windows" = 1 ]; then
	taskkill_cmd=$(command -v taskkill 2>/dev/null)
	if [ -z "$taskkill_cmd" ]; then
		for c in "${SYSTEMROOT:-${SystemRoot:-C:/Windows}}/System32/taskkill.exe" /c/Windows/System32/taskkill.exe; do
			[ -f "$c" ] && { taskkill_cmd=$c; break; }
		done
	fi
	trace "taskkill=${taskkill_cmd:-none}"
fi

# kill_tree <pid>: end a background job and everything it started, by exact
# PIDs (never by image name). Native Windows programs do not see bash's
# signals, so on Windows the job's Windows process tree is ended.
descendants() {
	local c
	for c in $(ps -ef 2>/dev/null | awk -v p="$1" '$3 == p { print $2 }'); do
		echo "$c"
		descendants "$c"
	done
}
kill_tree() {
	local p all list=""
	# Every pid is found before any is killed (a killed parent's children
	# are re-parented and could no longer be found), then they are killed
	# parents first, so no step of the job runs on after its child dies.
	# The list goes to $run_dir/kill-pids first: a runner ended partway
	# through this leaves the watchdog the rest of it, the job's root
	# perhaps already dead.
	all="$1 $(descendants "$1")"
	for p in $all; do
		list+="$p $(cat "/proc/$p/winpid" 2>/dev/null)"$'\n'
	done
	[ -n "${run_dir:-}" ] && [ -d "$run_dir" ] && printf '%s' "$list" >"$run_dir/kill-pids" 2>/dev/null
	trace "kill_tree $1: pids" $all
	end_listed "$list"
}
# end_listed <lines of "pid winpid">: ends each listed process that is still
# alive, in order. One a taskkill /T of its parent already ended is skipped.
end_listed() {
	local p w o rc
	local now
	while read -r p w; do
		[ -n "$p" ] && kill -0 "$p" 2>/dev/null || continue
		# The Windows pid as it is now (an exec since the listing changes it),
		# else the listed one.
		now=$(cat "/proc/$p/winpid" 2>/dev/null) && [ -n "$now" ] && w=$now
		# A native Windows program (go.exe, the test binary, node) ends with
		# its own Windows children only through taskkill /T. The arguments
		# are kept from MSYS path conversion (/T is not a path). Without
		# taskkill, the runtime's own kill ends that one Windows process.
		if [ -n "$w" ] && [ -n "$taskkill_cmd" ]; then
			o=$(MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' "$taskkill_cmd" /T /F /PID "$w" 2>&1)
			rc=$?
			trace "taskkill /T /F /PID $w (pid $p): exit $rc: $(printf '%s' "$o" | tr '\r\n' '  ')"
		elif [ -n "$w" ] && [ -x /usr/bin/kill ]; then
			/usr/bin/kill -f -W -KILL "$w" >/dev/null 2>&1
			trace "kill -f -W -KILL $w (pid $p): exit $?"
		fi
		kill -KILL "$p" 2>/dev/null
		if [ -n "$kill_pause" ]; then
			trace "kill paused"
			sleep "$kill_pause" </dev/null >/dev/null 2>&1
			kill_pause=""
		fi
	done <<<"$1"
}

# remove_run_dir: retried, since a process just ended may still hold a file.
remove_run_dir() {
	[ -n "${run_dir:-}" ] && [ -e "$run_dir" ] || return 0
	for _ in 1 2 3 4 5; do
		rm -rf "$run_dir" 2>/dev/null
		[ -e "$run_dir" ] || return 0
		sleep 1
	done
	return 1
}

cleanup() {
	local rc=$?
	trap - EXIT INT TERM HUP
	trace "cleanup rc=$rc job=${bg_pid:-none}"
	if [ -n "$bg_pid" ] && kill -0 "$bg_pid" 2>/dev/null; then
		kill_tree "$bg_pid"
		wait "$bg_pid" 2>/dev/null
	fi
	if ! remove_run_dir; then
		echo "live-tests: LEFTOVER: could not remove $run_dir (a process still holds it); delete it by hand" >&2
		rc=1
	fi
	rm -rf "$lock"
	trace "cleanup done"
	# The watchdog is ended last: a runner killed during cleanup (a signal
	# and then a hard kill, as a CI cancel or docker stop does) leaves it to
	# finish the job. It acts only once the runner is gone, so the two never
	# run at once.
	if [ -n "$watch_pid" ]; then
		kill -KILL "$watch_pid" 2>/dev/null
		wait "$watch_pid" 2>/dev/null
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'trace "trap INT"; exit 130' INT
trap 'trace "trap TERM"; exit 143' TERM
trap 'trace "trap HUP"; exit 129' HUP

# The watchdog: a runner ended with no chance to run its trap (taskkill /F
# of its bash, SIGKILL, a Windows runtime that ends a process instead of
# signalling it) would leave its test running and its cookie copy on disk
# until the next run's sweep. While the runner lives the watchdog only
# polls; once it is gone, the watchdog ends what a cleanup cut short had
# listed and the current job's tree, removes the run directory and removes
# the lock if it is still this run's. Its stdio is /dev/null (it holds no
# caller's pipe open) and its cwd is /. cleanup ends it last, once nothing
# is left to do, so it never acts on a run that ended normally.
watchdog() {
	local job="" listed=""
	kill_pause=""
	trap - EXIT TERM HUP
	cd / 2>/dev/null
	# Gone: no such pid, a zombie (Linux: its parent has not reaped it), or
	# past the run's deadline.
	while kill -0 "$1" 2>/dev/null && [ "$(date +%s)" -le "$deadline" ]; do
		case $(cat "/proc/$1/stat" 2>/dev/null) in *") Z "*) break ;; esac
		sleep 1
	done
	trace "watchdog: runner gone (or past its deadline)"
	read -r job <"$run_dir/job" 2>/dev/null
	listed=$(cat "$run_dir/kill-pids" 2>/dev/null)
	if [ -n "$listed" ]; then
		trace "watchdog: ending what cleanup listed"
		end_listed "$listed"
	fi
	[ -n "$job" ] && kill_tree "$job"
	remove_run_dir || trace "watchdog: could not remove the run directory"
	[ "$(cat "$lock/owner" 2>/dev/null)" = "$owner_line" ] && rm -rf "$lock"
	trace "watchdog: done"
}
watchdog "$$" </dev/null >/dev/null 2>&1 &
watch_pid=$!

if ! private_dir "$run_dir" || ! private_dir "$state/live-logs"; then
	echo "live-tests: cannot make $run_dir and the log directory private to this user; stopping before any copy" >&2
	exit 1
fi
mkdir -p "$run_dir/tmp" || exit 1

# ---- the helper -----------------------------------------------------------
helper=$run_dir/live-tests-helper
[ "$on_windows" = 1 ] && helper=$helper.exe
if ! (cd "$self_root" && go build -o "$(native "$helper")" ./tools/live-tests); then
	echo "live-tests: cannot build the helper (tools/live-tests)" >&2
	exit 1
fi

# ---- the private copy -----------------------------------------------------
pristine=""
size="?" age_s=0 rows_youtube=0 rows_twitch=0 rows_other=0
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
# The redactor and the scan always get a cookie file: with no source, an
# empty one, so the names and session shapes are still masked.
redact_base=$pristine
if [ -z "$redact_base" ]; then
	redact_base=$run_dir/empty.txt
	: >"$redact_base"
fi

# check_log <log> <cookie file>...: the self-check. A log the second reading
# finds anything in is redacted again in place, and deleted if that does not
# clear it. Prints one line; returns 1 when anything was found.
check_log() {
	local l=$1 n n2 args=()
	shift
	for f in "$@"; do args+=(--cookies "$(native "$f")"); done
	n=$("$helper" scan "${args[@]}" "$(native "$l")" | head -1)
	if [ "$n" = 0 ]; then
		echo "Self-check: 0 secrets in $(basename "$l")"
		return 0
	fi
	"$helper" scan -v "${args[@]}" "$(native "$l")" | tail -n +2 | sed 's/^.*: line /  line /' >&2
	if "$helper" redact "${args[@]}" <"$l" >"$l.redo" && mv -f "$l.redo" "$l"; then
		n2=$("$helper" scan "${args[@]}" "$(native "$l")" | head -1)
	else
		n2=failed
	fi
	if [ "$n2" = 0 ]; then
		echo "Self-check: $n secrets were in $(basename "$l"); it has been redacted again and now holds none"
	else
		rm -f "$l" "$l.redo"
		echo "Self-check: $n secrets were in $(basename "$l"); a second redaction left $n2, so the log was DELETED"
	fi
	return 1
}

if [ -n "$scan_log" ]; then
	[ -n "$pristine" ] || { echo "live-tests: --scan needs a cookie source" >&2; exit 2; }
	n=$("$helper" scan $scan_verbose --cookies "$(native "$pristine")" "$(native "$scan_log")")
	echo "secrets found in $scan_log (by the cookie file as it is now): ${n%%$'\n'*}"
	[ -n "$scan_verbose" ] && printf '%s\n' "$n" | tail -n +2
	[ "${n%%$'\n'*}" = 0 ]
	exit
fi

# A log a killed run left (no summary) is checked now, with today's copy.
for l in "$state"/live-logs/*.log; do
	[ -f "$l" ] || continue
	grep -q '^==== summary' "$l" && continue
	echo "live-tests: checking $(basename "$l"), which a stopped run left without a summary"
	check_log "$l" "$redact_base" || true
	[ -f "$l" ] && printf '==== summary ====\n(a stopped run; checked by a later run)\n' >>"$l"
done

log=$(mktemp "$state/live-logs/live-$(date +%Y%m%d-%H%M%S)-XXXX") || exit 1
mv "$log" "$log.log" && log=$log.log
copy_n=0
declare -A ran=()

for v in HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
	if [ -n "${!v:-}" ]; then
		echo "live-tests: WARNING: $v is set; the signed-in requests go through that proxy" >&2
	fi
done

# run_pkg <group> <tree> <package> <comma-separated tests>
run_pkg() {
	local group=$1 dir=$2 pkg=$3 tests=$4 names=() t pattern work="" rc rrc tmp_bs
	tmp_bs=$(native_bs "$run_dir/tmp")
	local -a envs=(
		-u MOOMBOX_LIVE_BROWSER_REFRESH -u MOOMBOX_LIVE_BROWSER_PATH -u MOOMBOX_LIVE_PGROUP
		-u MOOMBOX_LIVE_GOJA_TEST -u MOOMBOX_LIVE_YT_COOKIES -u MOOMBOX_LIVE_TWITCH_COOKIES
		-u MOOMBOX_LIVE_TWITCH_PROBE -u MOOMBOX_POT_ACCEPT -u MOOMBOX_POT_ACCEPT_COOKIES
		-u MOOMBOX_LIVE_TWITCH_CHANNEL -u MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL
		-u GODEBUG -u GOFLAGS -u NODE_OPTIONS GOTRACEBACK=single
		GOTMPDIR="$tmp_bs" TMP="$tmp_bs" TEMP="$tmp_bs" TMPDIR="$tmp_bs"
		MOOMBOX_LIVE_COOKIES_PRIVATE=1
		MOOMBOX_LIVE_YT_TEST=1 MOOMBOX_LIVE_BG_TEST=1 MOOMBOX_LIVE_CIPHER_TEST=1 MOOMBOX_LIVE_TWITCH_TEST=1
	)
	IFS=, read -r -a names <<<"$tests"
	local keep=()
	for t in "${names[@]}"; do
		[ -z "${ran["$dir $pkg $t"]:-}" ] || continue
		if [ -z "$run_filter" ] || [[ $t =~ $run_filter ]]; then keep+=("$t"); fi
	done
	[ ${#keep[@]} -gt 0 ] || return 0
	for t in "${keep[@]}"; do ran["$dir $pkg $t"]=1; done
	pattern="^($(IFS='|'; echo "${keep[*]}"))\$"

	# Only the package that reads a platform's cookies gets a copy.
	local want=""
	case $pkg:$group in
	*:pot-accept) [ "$rows_youtube" -gt 0 ] && want=pot ;;
	internal/youtube:*) [ "$rows_youtube" -gt 0 ] && want=yt ;;
	internal/twitch:*) [ "$rows_twitch" -gt 0 ] && want=twitch ;;
	esac
	if [ -n "$pristine" ] && [ -n "$want" ]; then
		copy_n=$((copy_n + 1))
		work=$run_dir/work-$copy_n.txt
		"$helper" copy "$(native "$pristine")" "$(native "$work")" >/dev/null || return 1
		case $want in
		yt) envs+=(MOOMBOX_LIVE_YT_COOKIES="$(native "$work")") ;;
		twitch) envs+=(MOOMBOX_LIVE_TWITCH_COOKIES="$(native "$work")") ;;
		pot) envs+=(MOOMBOX_POT_ACCEPT_COOKIES="$(native "$work")") ;;
		esac
	fi
	[ -n "$expect_premium" ] && envs+=(MOOMBOX_LIVE_YT_EXPECT_PREMIUM="$expect_premium")
	[ -n "$twitch_channel" ] && envs+=(MOOMBOX_LIVE_TWITCH_CHANNEL="$twitch_channel")
	[ -n "$twitch_offline" ] && envs+=(MOOMBOX_LIVE_TWITCH_PROBE=1 MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL="$twitch_offline")
	[ "$group" = goja ] && envs+=(MOOMBOX_LIVE_GOJA_TEST=1)
	if [ "$group" = pot-accept ]; then
		envs+=(MOOMBOX_POT_ACCEPT=1 MOOMBOX_POT_ACCEPT_VIDEO="$video")
		[ -n "$pot_clients" ] && envs+=(MOOMBOX_POT_ACCEPT_CLIENTS="$pot_clients")
		[ -n "$pot_player_pot" ] && envs+=(MOOMBOX_POT_ACCEPT_PLAYER_POT="$pot_player_pot")
	fi

	local redact=(--cookies "$(native "$redact_base")")
	[ -n "$work" ] && redact+=(--cookies "$(native "$work")")

	echo "### live-tests begin group=$group pkg=$pkg" | tee -a "$log"
	# In the background, so Ctrl+C and TERM reach the trap at once rather
	# than when go test ends (bash runs a trap only between commands, and
	# the wait builtin is one it interrupts); the exit codes come back
	# through a file, and the job's pid goes to one the watchdog reads.
	rm -f "$run_dir/status"
	{
		(cd "$dir" && env "${envs[@]}" "$go_test" test -count=1 -v -timeout "$timeout" -run "$pattern" "./$pkg" </dev/null) 2>&1 |
			"$helper" redact "${redact[@]}" | tee -a "$log"
		echo "${PIPESTATUS[0]} ${PIPESTATUS[1]}" >"$run_dir/status"
	} </dev/null &
	bg_pid=$!
	echo "$bg_pid" >"$run_dir/job"
	trace "job $group $pkg: pid $bg_pid"
	wait "$bg_pid"
	bg_pid=""
	: >"$run_dir/job"
	read -r rc rrc <"$run_dir/status" 2>/dev/null || { rc=1 rrc=1; }
	echo "### live-tests end group=$group pkg=$pkg exit=$rc" | tee -a "$log"
	if [ "$rrc" != 0 ]; then
		echo "live-tests: the redactor failed; stopping" >&2
		exit 1
	fi
}

# The Twitch channel tests (in twitch and cookies) need a channel that is
# live now: asked of Twitch when none is set (an anonymous query; the login is not a secret).
needs_twitch_channel=0
for g in "${groups[@]}"; do
	case $g in twitch | cookies) needs_twitch_channel=1 ;; esac
done
if [ "$needs_twitch_channel" = 1 ] && [ -z "$twitch_channel" ]; then
	if [ "$hooks" = 1 ] && [ -n "${MOOMBOX_LIVE_TWITCH_PICK:-}" ]; then
		twitch_channel=$MOOMBOX_LIVE_TWITCH_PICK
	else
		twitch_channel=$("$helper" pick-twitch-live 2>/dev/null) || twitch_channel=""
	fi
	[ -n "$twitch_channel" ] && echo "Twitch channel: $twitch_channel (live now, from Twitch's top streams)" | tee -a "$log"
fi

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
	while IFS=$'\t' read -r p t; do
		# Only the fixture tests need the fixture: today's player
		# (internal/youtube TestLiveCurrentPlayerSolves) runs without it.
		if [ "$g" = cipher ] && [ "$p" = internal/cipher ] && [ ! -f "$tree/internal/cipher/testdata/player_cb017549.js" ]; then
			echo "### live-tests skip group=cipher reason=internal/cipher: no player fixture in that tree: run bash .github/scripts/fetch-cipher-fixture.sh" | tee -a "$log"
			continue
		fi
		run_pkg "$g" "$tree" "$p" "$t"
	done < <(group_entries "$g")
done

echo
echo "==== summary ===="
echo "$cookie_line"
summary=$("$helper" summary "$(native "$log")")
echo "$summary"
status=0
case $summary in *"RESULT: FAIL"*) status=1 ;; esac
{
	echo "==== summary ===="
	echo "$summary"
} >>"$log"

# Self-check: every copy this run made, the rewritten ones included.
copies=("$redact_base")
for f in "$run_dir"/work-*.txt; do
	[ -f "$f" ] && copies+=("$f")
done
check_log "$log" "${copies[@]}" || status=1
[ -f "$log" ] && echo "Log: $log"
exit $status
