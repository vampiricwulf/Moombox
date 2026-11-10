#!/usr/bin/env bash
# The checks to run before a branch is merged, from any checkout or worktree.
#
#   local-gate.sh [--quick | --full]   run the gates on the tree you are in
#   local-gate.sh --prune-gotmp        delete Go's leftovers from GOTMPDIR, run nothing
#
# --quick (the default) is what CI's lint job checks, and runs no tests: gofmt,
# `go mod tidy -diff`, go vet and go build for linux/amd64, windows/amd64 and
# linux/arm64, the modernc.org/libc pin, the Dockerfile's Go version,
# staticcheck for linux and windows (skipped when it is not installed),
# `test-shard.sh --check` and `bash -n` over .github/scripts. In a tree that lacks the embed blobs it first runs
# embed-standins.sh, so it works in a new worktree; see that script for what
# a stand-in must never be used for.
#
# --full adds the frontend node suite, the sidecar's `npm test` (skipped when
# bgutil-sidecar/node_modules is absent), `go test -count=1 ./...` and a race
# run over the packages with the most goroutines. It writes no stand-ins: the
# suite needs the real blobs (new-worktree.sh copies them), and it stops
# before the tests when one is an empty stand-in. Run one --full at
# a time on a machine; two full suites side by side time each other out.
#
# Each gate prints one PASS, FAIL or SKIP line; everything the commands print
# goes to the log named on the first line (local-gate.log in this worktree's
# git directory, or $LOCAL_GATE_LOG). Exits 1 if any gate failed.
#
# GOTMPDIR: where a virus scanner quarantines test binaries built under the
# user's temp directory (Windows Defender does), Go needs a temp directory of
# its own. When GOTMPDIR is unset and this is Git Bash, the script uses
# .superpowers/gotmp in the main checkout (gitignored, shared by every
# worktree) and passes it in the Windows spelling, with backslashes: tests
# that compare paths fail when the temp root is spelled with forward slashes.
# Set GOTMPDIR yourself to use another directory. Nothing in it is deleted
# unless you ask with --prune-gotmp, and only ask while no go command is
# running in any worktree, since they share it.
set -uo pipefail

usage() {
	sed -n '2,/^set -uo/{/^set -uo/d;s/^# \{0,1\}//;p;}' "$0" >&2
	exit 2
}

mode=quick
case ${1:-} in
'' | --quick) ;;
--full) mode=full ;;
--prune-gotmp) mode=prune ;;
*) usage ;;
esac
[ "$#" -le 1 ] || usage

root=$(git rev-parse --show-toplevel) || exit 1
cd "$root" || exit 1

if [ -z "${GOTMPDIR:-}" ] && command -v cygpath >/dev/null 2>&1; then
	# The main checkout is the parent of the git directory all worktrees share.
	main=$(cd "$(git rev-parse --git-common-dir)/.." && pwd)
	mkdir -p "$main/.superpowers/gotmp"
	GOTMPDIR=$(cygpath -w "$main/.superpowers/gotmp")
	export GOTMPDIR
fi
export NO_COLOR=1

if [ "$mode" = prune ]; then
	[ -n "${GOTMPDIR:-}" ] || {
		echo "local-gate.sh: GOTMPDIR is not set, so there is nothing of this script's to prune" >&2
		exit 2
	}
	dir=$GOTMPDIR
	if command -v cygpath >/dev/null 2>&1; then dir=$(cygpath -u "$dir"); fi
	# Only what the go command itself leaves at the top level: its build work
	# directories. A test's own temp directories are under TMP, not here.
	n=0
	for entry in "$dir"/go-build*; do
		[ -e "$entry" ] || continue
		rm -rf -- "$entry"
		n=$((n + 1))
	done
	echo "pruned $n entries from $GOTMPDIR"
	exit 0
fi

log=${LOCAL_GATE_LOG:-$(git rev-parse --absolute-git-dir)/local-gate.log}
: >"$log" || exit 1
echo "local-gate $mode: $(git rev-parse --abbrev-ref HEAD) at $(git rev-parse --short HEAD), log $log"
echo "GOTMPDIR=${GOTMPDIR:-<unset>}" >>"$log"

failed=0

# gate <name> <command> [args...]: run it, log its output, print one line.
gate() {
	local name=$1
	shift
	echo "== $name" >>"$log"
	if "$@" >>"$log" 2>&1; then
		echo "PASS  $name"
	else
		echo "FAIL  $name"
		failed=1
	fi
}

skip() { echo "SKIP  $1 ($2)"; }

# on <goos> <goarch> <command> [args...]: run it for that platform.
on() {
	local goos=$1 goarch=$2
	shift 2
	GOOS=$goos GOARCH=$goarch "$@"
}

# on_nocgo: the same without cgo, as CI cross-builds.
on_nocgo() {
	local goos=$1 goarch=$2
	shift 2
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch "$@"
}

gofmt_clean() {
	local out
	out=$(gofmt -l ./cmd ./internal ./tools ./web) || return 1
	[ -z "$out" ] || {
		echo "gofmt would reformat:"
		echo "$out"
		return 1
	}
}

# The two pin checks are ci.yml's, word for word; the reasons are there.
libc_pin() {
	local ours want
	ours=$(go list -m -f '{{.Version}}' modernc.org/libc)
	want=$(sed -n 's#^[[:space:]]*modernc.org/libc \(v[^ ]*\).*#\1#p' "$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)")
	if [ "$ours" != "$want" ]; then
		echo "modernc.org/libc is $ours but modernc.org/sqlite pins $want"
		return 1
	fi
}

dockerfile_go() {
	local want got
	want=$(sed -n 's/^go \([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' go.mod)
	got=$(sed -n 's/^FROM .*golang:\([0-9][0-9]*\.[0-9][0-9]*\)[-.].*/\1/p' Dockerfile)
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		echo "go.mod requires Go \"$want\" but the Dockerfile's build stage is golang:\"$got\""
		return 1
	fi
}

scripts_parse() {
	local f status=0
	for f in .github/scripts/*.sh; do
		bash -n "$f" || status=1
	done
	return $status
}

# real_blobs: no file a go:embed directive names is an empty stand-in. (A
# missing one stops the build at once and says so itself.)
# A stand-in that --quick or embed-standins.sh left compiles, and then fails
# tests a long way into the suite.
real_blobs() {
	local file dir name status=0
	while read -r file; do
		dir=$(dirname "$file")
		while read -r name; do
			case $name in '' | *[*?[]*) continue ;; esac
			if [ -e "$dir/$name" ] && [ ! -s "$dir/$name" ]; then
				echo "empty stand-in: $dir/$name"
				status=1
			fi
		done <<<"$(sed -n 's|^//go:embed ||p' "$file" | tr ' ' '\n')"
	done <<<"$(git grep -l -e '^/[/]go:embed ' -- '*.go')"
	if [ "$status" -ne 0 ]; then
		echo "the full suite needs the real embed blobs: copy them from the main checkout (new-worktree.sh does), or run \`go run ./tools/fetch-node\` and \`node build.mjs\` in bgutil-sidecar/"
	fi
	return $status
}

node_frontend() (
	cd web/tests || exit 1
	node --test --test-timeout=120000 ./*.test.mjs
)

node_sidecar() (
	cd bgutil-sidecar || exit 1
	npm test
)

if [ "$mode" = quick ]; then
	bash .github/scripts/embed-standins.sh 2>&1 | tee -a "$log"
fi

gate "gofmt" gofmt_clean
gate "go mod tidy -diff" go mod tidy -diff
gate "go vet (linux/amd64)" on linux amd64 go vet ./...
gate "go vet (windows/amd64)" on windows amd64 go vet ./...
gate "go vet (linux/arm64)" on linux arm64 go vet ./...
gate "modernc.org/libc pin" libc_pin
gate "Dockerfile Go version" dockerfile_go
if command -v staticcheck >/dev/null 2>&1; then
	gate "staticcheck (linux/amd64)" on linux amd64 staticcheck ./...
	gate "staticcheck (windows/amd64)" on windows amd64 staticcheck ./...
else
	skip "staticcheck" "not installed: go install honnef.co/go/tools/cmd/staticcheck@<the version ci.yml pins>"
fi
gate "go build (linux/amd64)" on_nocgo linux amd64 go build ./...
gate "go build (windows/amd64)" on_nocgo windows amd64 go build ./...
gate "go build (linux/arm64)" on_nocgo linux arm64 go build ./...
gate "test shards cover every package" bash .github/scripts/test-shard.sh --check
gate "bash -n .github/scripts" scripts_parse

if [ "$mode" = full ]; then
	echo "== embed blobs are real" >>"$log"
	if ! real_blobs >>"$log" 2>&1; then
		echo "FAIL  embed blobs are real"
		real_blobs
		echo "FAILED: see $log"
		exit 1
	fi
	echo "PASS  embed blobs are real"
	gate "frontend node suite" node_frontend
	if [ -d bgutil-sidecar/node_modules ]; then
		gate "sidecar npm test" node_sidecar
	else
		skip "sidecar npm test" "no bgutil-sidecar/node_modules: npm ci && node build.mjs there"
	fi
	gate "go test ./..." go test -count=1 -timeout 900s ./...
	gate "go test -race (worker, web, tui, monitor, database, cmd)" go test -count=1 -race -timeout 900s \
		./internal/worker/ ./internal/web/... ./internal/tui/ ./internal/monitor/ ./internal/database/ ./cmd/moombox/
fi

if [ "$failed" -ne 0 ]; then
	echo "FAILED: see $log"
	exit 1
fi
echo "all gates passed"
