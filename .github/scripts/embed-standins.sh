#!/usr/bin/env bash
# Put an empty file where each missing go:embed build output should be, so
# every package loads (.github/workflows/ci.yml's lint job; local-gate.sh
# --quick in a worktree that has no blobs).
#
#   embed-standins.sh
#
# vet, staticcheck and the builds load every package, and a package does not
# load without the files its go:embed directives name. They read code, never
# the embedded bytes, so an empty file replaces each named file the checkout
# lacks that git ignores (a build output): the sidecar tarball and the Node
# binaries, found from the directives rather than listed here, so a blob added
# for another platform is covered too. A file that exists is never touched,
# and a tracked file missing from a commit gets no stand-in, so vet and the
# builds still fail on it.
#
# Stand-ins are for compiling, vetting and tests that never start the sidecar.
# A binary built over them has an empty sidecar and fails only when it runs,
# and internal/bgutils/embed's own tests fail on them. Replace them with the
# real blobs (`go run ./tools/fetch-node`, then `node build.mjs` in
# bgutil-sidecar/, or copy the files from another checkout) before building
# anything to run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# The pattern is spelled /[/] because Git Bash rewrites an argument holding
# "//" before a Windows git sees it; the expression is the same.

made=$(
	git grep -l -e '^/[/]go:embed ' -- '*.go' | while read -r file; do
		dir=$(dirname "$file")
		sed -n 's|^//go:embed ||p' "$file" | tr ' ' '\n' | while read -r name; do
			case $name in '' | *[*?[]*) continue ;; esac
			if [ ! -e "$dir/$name" ] && git check-ignore -q "$dir/$name"; then
				: >"$dir/$name"
				echo "empty stand-in: $dir/$name"
			fi
		done
	done
)

if [ -n "$made" ]; then
	echo "$made"
	echo "embed-standins.sh: these are empty files. Do not build a binary to run from this tree until the real blobs replace them." >&2
fi
