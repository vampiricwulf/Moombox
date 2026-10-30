#!/usr/bin/env bash
# Which packages each CI test job runs (.github/workflows/ci.yml).
#
#   test-shard.sh <flavour>/<shard>   print the shard's import paths, one per line
#   test-shard.sh --check [ci.yml]    prove the layout below is sound (lint runs it)
#
# A flavour is one way of running the suite: linux (go test on ubuntu), race
# (go test -race on ubuntu) and windows (go test on windows). Each flavour runs
# a few NAMED shards from the table below, plus its own catch-all, rest: every
# package `go list ./...` reports for that flavour's GOOS that none of its
# named shards lists. A package added tomorrow therefore runs in rest without
# anyone touching this file, and moving a package out of rest is one edit here.
#
# Shards hold whole packages. The named ones exist because one package's tests
# take most of a job on their own (internal/worker is the longest everywhere);
# a flavour names only the shards that would otherwise push its rest past
# about two minutes on a 4-vCPU runner, so linux names one and windows four.
#
# --check fails unless, for every flavour: every package it names exists, no
# package is in two of its shards, no shard is empty, the shards together are
# `go list ./...` exactly once each, and ci.yml's test matrix runs exactly that
# flavour's shards, rest included (its `- { flavour: F, shard: S }` rows).
set -euo pipefail
export LC_ALL=C

# shard    packages, relative to the module root
shard_table='
worker     internal/worker
engine     internal/engine
cookies    internal/cookies
web        internal/bgutils/sidecar cmd/moombox internal/web/routes
'

# flavour  GOOS     the named shards it runs besides rest
flavour_table='
linux      linux    worker
race       linux    worker engine web
windows    windows  worker engine cookies web
'

die() {
	echo "test-shard.sh: $*" >&2
	exit 1
}

flavours() { awk 'NF { print $1 }' <<<"$flavour_table"; }

is_flavour() { grep -qxF -- "$1" <<<"$(flavours)"; }

flavour_goos() { awk -v f="$1" '$1 == f { print $2 }' <<<"$flavour_table"; }

flavour_shards() { awk -v f="$1" '$1 == f { for (i = 3; i <= NF; i++) print $i }' <<<"$flavour_table"; }

# shard_dirs <shard>: the shard's directories, or nothing for an unknown shard.
shard_dirs() { awk -v s="$1" '$1 == s { for (i = 2; i <= NF; i++) print $i }' <<<"$shard_table"; }

# all_packages <flavour>: `go list ./...` as that flavour's runner sees it.
all_packages() {
	GOOS=$(flavour_goos "$1") GOARCH=amd64 go list ./... | tr -d '\r'
}

# named_packages <shard>: the import paths of a named shard.
named_packages() {
	local mod dir
	mod=$(go list -m | tr -d '\r')
	for dir in $(shard_dirs "$1"); do
		echo "$mod/$dir"
	done
}

# shard_packages <flavour> <shard>: what the job for flavour/shard tests.
shard_packages() {
	local flavour=$1 shard=$2 s named
	if [ "$shard" != rest ]; then
		named_packages "$shard"
		return
	fi
	named=$(for s in $(flavour_shards "$flavour"); do named_packages "$s"; done)
	all_packages "$flavour" | grep -vxF -f <(printf '%s\n' "$named") || true
}

# validate <flavour> <shard>: dies unless the flavour exists and runs the shard.
validate() {
	is_flavour "$1" || die "unknown flavour '$1' (known: $(flavours | paste -sd ' ' -))"
	[ "$2" = rest ] && return
	grep -qxF -- "$2" <<<"$(flavour_shards "$1")" ||
		die "flavour '$1' does not run a shard '$2' (it runs: $(flavour_shards "$1" | paste -sd ' ' -) rest)"
	[ -n "$(shard_dirs "$2")" ] || die "shard '$2' has no packages in the shard table"
}

print_shard() {
	local flavour shard pkgs
	[[ $1 == */* ]] || die "usage: test-shard.sh <flavour>/<shard>, e.g. race/worker (or --check)"
	flavour=${1%%/*} shard=${1#*/}
	validate "$flavour" "$shard"
	pkgs=$(shard_packages "$flavour" "$shard")
	[ -n "$pkgs" ] || die "shard $1 is empty"
	printf '%s\n' "$pkgs"
}

# list_lines <text>: the non-empty lines, indented, for a failure report.
list_lines() { grep . <<<"$1" | sed 's/^/    /'; }

check() {
	local workflow=${1:-.github/workflows/ci.yml} failed=0
	local flavour shard all pkgs listed dupes missing extra want got matrix p
	[ -f "$workflow" ] || die "no workflow file at $workflow"
	# The test matrix rows the workflow runs, as flavour/shard.
	matrix=$(sed -n 's|^[[:space:]]*-[[:space:]]*{[[:space:]]*flavour:[[:space:]]*\([a-z0-9-]*\),[[:space:]]*shard:[[:space:]]*\([a-z0-9-]*\)[[:space:]]*}.*|\1/\2|p' "$workflow")
	dupes=$(sort <<<"$matrix" | uniq -d)
	if [ -n "$dupes" ]; then
		echo "FAIL: $workflow runs these matrix rows more than once:"
		list_lines "$dupes"
		failed=1
	fi
	for flavour in $(flavours); do
		all=$(all_packages "$flavour")
		listed=""
		for shard in $(flavour_shards "$flavour"); do
			if [ "$shard" = rest ] || [ -z "$(shard_dirs "$shard")" ]; then
				echo "FAIL: $flavour names a shard '$shard', which the shard table does not define (rest is implicit)"
				failed=1
				continue
			fi
			for p in $(named_packages "$shard"); do
				if ! grep -qxF -- "$p" <<<"$all"; then
					echo "FAIL: $flavour/$shard names $p, which go list ./... (GOOS=$(flavour_goos "$flavour")) does not report"
					failed=1
				fi
			done
		done
		for shard in $(flavour_shards "$flavour") rest; do
			pkgs=$(shard_packages "$flavour" "$shard")
			if [ -z "$pkgs" ]; then
				echo "FAIL: $flavour/$shard is empty"
				failed=1
			fi
			echo "$flavour/$shard: $(grep -c . <<<"$pkgs" || true) package(s)"
			listed+=$pkgs$'\n'
		done
		dupes=$(grep . <<<"$listed" | sort | uniq -d || true)
		missing=$(comm -23 <(grep . <<<"$all" | sort -u) <(grep . <<<"$listed" | sort -u))
		extra=$(comm -13 <(grep . <<<"$all" | sort -u) <(grep . <<<"$listed" | sort -u))
		if [ -n "$dupes" ]; then
			echo "FAIL: $flavour runs these packages in more than one shard:"
			list_lines "$dupes"
			failed=1
		fi
		if [ -n "$missing" ]; then
			echo "FAIL: $flavour runs these packages in no shard:"
			list_lines "$missing"
			failed=1
		fi
		if [ -n "$extra" ]; then
			echo "FAIL: $flavour's shards name packages go list ./... does not report:"
			list_lines "$extra"
			failed=1
		fi
		if [ -z "$dupes$missing$extra" ]; then
			echo "$flavour: its shards hold all $(grep -c . <<<"$all") packages of go list ./... (GOOS=$(flavour_goos "$flavour")), each exactly once"
		fi
		want=$({ flavour_shards "$flavour"; echo rest; } | sed "s|^|$flavour/|" | sort)
		got=$(grep "^$flavour/" <<<"$matrix" | sort || true)
		if [ "$want" != "$got" ]; then
			echo "FAIL: $workflow's test matrix runs [$(paste -sd ' ' - <<<"$got")] for $flavour; this script defines [$(paste -sd ' ' - <<<"$want")]"
			failed=1
		fi
	done
	extra=$(grep . <<<"$matrix" | grep -vE "^($(flavours | paste -sd '|' -))/" || true)
	if [ -n "$extra" ]; then
		echo "FAIL: $workflow's test matrix runs flavours this script does not define:"
		list_lines "$extra"
		failed=1
	fi
	[ "$failed" -eq 0 ] || exit 1
	echo "OK: every flavour's shards partition go list ./..., and $workflow runs each shard exactly once"
}

case "${1:-}" in
--check) check "${2:-}" ;;
-h | --help) sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//' ;;
"") die "usage: test-shard.sh <flavour>/<shard> | --check [ci.yml]" ;;
*) print_shard "$1" ;;
esac
