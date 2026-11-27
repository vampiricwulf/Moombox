#!/usr/bin/env bash
# Download the YouTube player bodies the ejs vector test reads
# (TestEjsPortVectors, internal/cipher/ejs_vectors_test.go): every player and
# variant internal/cipher/ejs_vectors.json names, or only the variants given.
#
#   fetch-ejs-vector-players.sh [variant...]     e.g.  main tv tv_es6 tcl
#
# The bodies go to internal/cipher/testdata/ejsvectors/player_<id>_<variant>.js
# (gitignored: they are YouTube's code, fetched, never committed), or to
# $EJS_VECTOR_PLAYERS_DIR, which the test reads from MOOMBOX_EJS_VECTOR_PLAYERS.
# A body already there is kept. A player YouTube no longer serves is reported
# and skipped; the test skips a body that is not there. Unlike
# fetch-cipher-fixture.sh there is no checksum: the expected answers in
# ejs_vectors.json are what checks a body, and a wrong one fails the test.
# Needs node (to read the JSON) and curl. Exits 1 when nothing could be
# fetched and nothing was there already.
set -uo pipefail

root=$(git rev-parse --show-toplevel) || exit 1
vectors=$root/internal/cipher/ejs_vectors.json
dest=${EJS_VECTOR_PLAYERS_DIR:-$root/internal/cipher/testdata/ejsvectors}
mkdir -p "$dest" || exit 1

# One "<id> <variant> <path>" line per body, filtered to the variants asked.
list=$(node -e '
const v = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
const want = new Set(process.argv.slice(2));
const seen = new Set();
for (const t of v.tests) for (const variant of t.variants) {
  const key = t.player + " " + variant;
  if ((want.size && !want.has(variant)) || seen.has(key)) continue;
  seen.add(key);
  console.log(key + " " + v.variants[variant]);
}' "$vectors" "$@") || exit 1

have=0 fetched=0 failed=0
while read -r id variant path; do
	[ -n "$id" ] || continue
	out=$dest/player_${id}_${variant}.js
	if [ -s "$out" ]; then
		have=$((have + 1))
		continue
	fi
	url=https://www.youtube.com/s/player/$id/$path
	if curl -fsSL --retry 2 --retry-delay 3 --connect-timeout 15 --max-time 60 -o "$out.download" -- "$url"; then
		mv -f -- "$out.download" "$out"
		fetched=$((fetched + 1))
	else
		rm -f -- "$out.download"
		echo "fetch-ejs-vector-players.sh: could not download $url" >&2
		failed=$((failed + 1))
	fi
done <<<"$list"

echo "fetch-ejs-vector-players.sh: $fetched fetched, $have already there, $failed failed, in $dest"
[ $((fetched + have)) -gt 0 ]
