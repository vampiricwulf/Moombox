#!/bin/bash
# Update all reference repos and show what changed.
# Clones any missing repos automatically.
#
# Usage:
#   ./update-all.sh           # Update all, show new commits
#   ./update-all.sh --diff    # Also show file-level diffs (verbose)
#
# Two ranges are reported per repo. "New commits" are those the pull just
# brought in, i.e. since this clone was last pulled. "Since last review" counts
# the commits after the repo's Last reviewed SHA in docs/spec/upstream-ports.md,
# which is the range a review must cover; it is printed whenever that SHA can
# be read from the ledger's table and exists in the clone.
#
# A clone, fetch or pull that fails is reported as FAILED, never as "no
# changes", and the script then exits 1.
#
# Repos and what we care about in each:
#   yt-dlp                    — YouTube/Twitch extractors, PO token, cipher, downloaders
#   BgUtils                   — BotGuard challenge/attestation, integrity tokens
#   bgutil-ytdlp-pot-provider — PO token provider protocol
#   ejs                       — yt-dlp external JS cipher/signature solving
#   moonarchive               — DASH/HLS segment download strategies
#   moombox                   — Original Python moombox
#   chatterino7               — Twitch IRC, emotes, badges

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SHOW_DIFF=false
LEDGER="$SCRIPT_DIR/../docs/spec/upstream-ports.md"
ANY_FAILED=false
[[ "$1" == "--diff" ]] && SHOW_DIFF=true

# Reference repos: name -> clone URL
declare -A REPOS=(
    [yt-dlp]="https://github.com/yt-dlp/yt-dlp.git"
    [BgUtils]="https://github.com/LuanRT/BgUtils.git"
    [bgutil-ytdlp-pot-provider]="https://github.com/Brainicism/bgutil-ytdlp-pot-provider.git"
    [ejs]="https://github.com/yt-dlp/ejs.git"
    [moonarchive]="https://github.com/nosoop/moonarchive.git"
    [moombox]="https://github.com/nosoop/moombox.git"
    [chatterino7]="https://github.com/SevenTV/chatterino7.git"
)

# Clone any missing repos
CLONED_ANY=false
for name in "${!REPOS[@]}"; do
    dir="$SCRIPT_DIR/$name"
    if [ ! -d "$dir/.git" ]; then
        echo "=== $name === (cloning...)"
        git clone --quiet "${REPOS[$name]}" "$dir" 2>/dev/null
        if [ $? -eq 0 ]; then
            echo "    Cloned from ${REPOS[$name]}"
            CLONED_ANY=true
        else
            echo "    FAILED to clone ${REPOS[$name]}"
            ANY_FAILED=true
        fi
        echo
    fi
done

if $CLONED_ANY; then
    echo "============================================"
    echo
fi

# ledger_sha <name>: the repo's Last reviewed SHA from the ledger's table, or
# nothing. The row is the one whose first cell starts with the repo's name.
ledger_sha() {
    [ -f "$LEDGER" ] || return 0
    awk -F'|' -v repo="$1" '
        { cell = $2; gsub(/^[ \t]+|[ \t]+$/, "", cell); split(cell, word, " ") }
        word[1] == repo && match($4, /[0-9a-f]{40}/) { print substr($4, RSTART, RLENGTH); exit }
    ' "$LEDGER"
}

# report_since_review <name> <dir> <default branch>: one line, never an error.
report_since_review() {
    local sha count
    sha=$(ledger_sha "$1")
    if [ -z "$sha" ]; then
        echo "    since last review: no last-reviewed SHA for $1 in docs/spec/upstream-ports.md"
        return 0
    fi
    if ! git -C "$2" cat-file -e "$sha^{commit}" 2>/dev/null; then
        echo "    since last review: last-reviewed SHA ${sha:0:9} not found in this clone"
        return 0
    fi
    count=$(git -C "$2" rev-list --count "$sha..origin/$3" 2>/dev/null)
    if [ -z "$count" ]; then
        echo "    since last review: origin/$3 not found"
        return 0
    fi
    echo "    since last review (${sha:0:9}..origin/$3): $count commits"
    if [ "$count" != 0 ]; then
        echo "    list them: git -C references/$1 log --oneline $sha..origin/$3"
    fi
}

# Track whether anything changed across all repos
ANY_CHANGES=false

for name in "${!REPOS[@]}"; do
    dir="$SCRIPT_DIR/$name"
    [ -d "$dir/.git" ] || continue

    # Save current HEAD before pulling
    old_head=$(git -C "$dir" rev-parse HEAD 2>/dev/null)

    # Get default branch name, from what the last fetch left, so a failed
    # fetch can still report against it
    default_branch=$(git -C "$dir" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@')
    if [ -z "$default_branch" ]; then
        default_branch=$(git -C "$dir" branch -r 2>/dev/null | grep -oP 'origin/\K(main|master)' | head -1)
    fi
    [ -z "$default_branch" ] && default_branch="main"

    # Fetch and pull
    if ! fetch_err=$(git -C "$dir" fetch --all --prune -q 2>&1); then
        echo "=== $name === FETCH FAILED"
        echo "    $(echo "$fetch_err" | head -1)"
        report_since_review "$name" "$dir" "$default_branch"
        echo
        ANY_FAILED=true
        continue
    fi

    if ! pull_err=$(git -C "$dir" pull -q 2>&1); then
        echo "=== $name === PULL FAILED (diverged or dirty)"
        echo "    $(echo "$pull_err" | head -1)"
        report_since_review "$name" "$dir" "$default_branch"
        echo
        ANY_FAILED=true
        continue
    fi

    new_head=$(git -C "$dir" rev-parse HEAD 2>/dev/null)

    # Skip repos with no changes
    if [ "$old_head" = "$new_head" ]; then
        echo "=== $name === (no changes since the last pull)"
        report_since_review "$name" "$dir" "$default_branch"
        echo
        continue
    fi

    ANY_CHANGES=true
    commit_count=$(git -C "$dir" rev-list --count "$old_head..$new_head" 2>/dev/null)
    echo "=== $name === ($commit_count new commits)"
    report_since_review "$name" "$dir" "$default_branch"
    echo

    # Show commit log
    echo "--- Commits ---"
    git -C "$dir" log --oneline --no-merges "$old_head..$new_head" 2>/dev/null
    echo

    # Show which files changed (summary)
    echo "--- Files changed ---"
    git -C "$dir" diff --stat "$old_head..$new_head" 2>/dev/null
    echo

    # Highlight files relevant to Moombox per repo
    case "$name" in
        yt-dlp)
            echo "--- Moombox-relevant changes ---"
            # YouTube extractor
            yt_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/extractor/youtube/' \
                2>/dev/null | head -20)
            [ -n "$yt_changes" ] && echo "[YouTube extractor]" && echo "$yt_changes" && echo

            # Twitch extractor
            tw_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/extractor/twitch.py' \
                2>/dev/null | head -20)
            [ -n "$tw_changes" ] && echo "[Twitch extractor]" && echo "$tw_changes" && echo

            # PO token / pot system
            pot_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/extractor/youtube/pot' \
                2>/dev/null | head -20)
            [ -n "$pot_changes" ] && echo "[PO token system]" && echo "$pot_changes" && echo

            # JS interpreter / cipher (jsc is the JS challenge code)
            cipher_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/jsinterp*' \
                'yt_dlp/extractor/youtube/jsc' \
                2>/dev/null | head -20)
            [ -n "$cipher_changes" ] && echo "[JS interpreter / cipher]" && echo "$cipher_changes" && echo

            # Networking (request handlers, impersonation)
            net_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/networking' \
                2>/dev/null | head -20)
            [ -n "$net_changes" ] && echo "[Networking]" && echo "$net_changes" && echo

            # Downloaders
            dl_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/downloader/' \
                2>/dev/null | head -20)
            [ -n "$dl_changes" ] && echo "[Downloaders]" && echo "$dl_changes" && echo

            # Cookies
            cookie_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/cookies.py' \
                2>/dev/null | head -20)
            [ -n "$cookie_changes" ] && echo "[Cookies]" && echo "$cookie_changes" && echo

            # If --diff, show actual diffs for the relevant files
            if $SHOW_DIFF; then
                echo "--- YouTube extractor diff ---"
                git -C "$dir" diff "$old_head..$new_head" -- \
                    'yt_dlp/extractor/youtube/' \
                    2>/dev/null | head -500
                echo
            fi
            ;;

        BgUtils)
            echo "--- Moombox-relevant changes ---"
            bg_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'src/' 'lib/' \
                2>/dev/null | head -20)
            [ -n "$bg_changes" ] && echo "[BotGuard core]" && echo "$bg_changes" && echo
            ;;

        bgutil-ytdlp-pot-provider)
            echo "--- Moombox-relevant changes ---"
            echo "[PO token provider — all changes are relevant]"
            echo
            ;;

        ejs)
            echo "--- Moombox-relevant changes ---"
            echo "[External JS cipher — all changes are relevant]"
            echo
            ;;

        moonarchive)
            echo "--- Moombox-relevant changes ---"
            ma_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                '*.py' \
                2>/dev/null | head -20)
            [ -n "$ma_changes" ] && echo "[Download/segment logic]" && echo "$ma_changes" && echo
            ;;

        chatterino7)
            echo "--- Moombox-relevant changes ---"
            irc_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'src/providers/twitch/' \
                'src/common/' \
                2>/dev/null | head -20)
            [ -n "$irc_changes" ] && echo "[Twitch IRC/emotes/badges]" && echo "$irc_changes" && echo
            ;;
    esac

    echo "============================================"
    echo
done

if $ANY_FAILED; then
    echo "Some references FAILED to update (see above); what is shown for them may be stale."
    exit 1
fi

if ! $ANY_CHANGES; then
    echo "Nothing new was pulled."
fi
