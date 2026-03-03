#!/bin/bash
# Update all reference repos and show what changed.
# Clones any missing repos automatically.
#
# Usage:
#   ./update-all.sh           # Update all, show new commits
#   ./update-all.sh --diff    # Also show file-level diffs (verbose)
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
        fi
        echo
    fi
done

if $CLONED_ANY; then
    echo "============================================"
    echo
fi

# Track whether anything changed across all repos
ANY_CHANGES=false

for name in "${!REPOS[@]}"; do
    dir="$SCRIPT_DIR/$name"
    [ -d "$dir/.git" ] || continue

    # Save current HEAD before pulling
    old_head=$(git -C "$dir" rev-parse HEAD 2>/dev/null)

    # Fetch and pull
    git -C "$dir" fetch --all --prune -q 2>/dev/null

    # Get default branch name
    default_branch=$(git -C "$dir" symbolic-ref refs/remotes/origin/HEAD 2>/dev/null | sed 's@^refs/remotes/origin/@@')
    if [ -z "$default_branch" ]; then
        default_branch=$(git -C "$dir" branch -r 2>/dev/null | grep -oP 'origin/\K(main|master)' | head -1)
    fi
    [ -z "$default_branch" ] && default_branch="main"

    git -C "$dir" pull -q 2>/dev/null

    new_head=$(git -C "$dir" rev-parse HEAD 2>/dev/null)

    # Skip repos with no changes
    if [ "$old_head" = "$new_head" ]; then
        echo "=== $name === (no changes)"
        echo
        continue
    fi

    ANY_CHANGES=true
    commit_count=$(git -C "$dir" rev-list --count "$old_head..$new_head" 2>/dev/null)
    echo "=== $name === ($commit_count new commits)"
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
                'yt_dlp/extractor/youtube.py' \
                'yt_dlp/extractor/youtube/' \
                2>/dev/null | head -20)
            [ -n "$yt_changes" ] && echo "[YouTube extractor]" && echo "$yt_changes" && echo

            # Twitch extractor
            tw_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/extractor/twitch.py' \
                'yt_dlp/extractor/twitch/' \
                2>/dev/null | head -20)
            [ -n "$tw_changes" ] && echo "[Twitch extractor]" && echo "$tw_changes" && echo

            # PO token / pot system
            pot_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/networking/_pot*' \
                'yt_dlp/pot/' \
                2>/dev/null | head -20)
            [ -n "$pot_changes" ] && echo "[PO token system]" && echo "$pot_changes" && echo

            # JS interpreter / cipher
            cipher_changes=$(git -C "$dir" diff --stat "$old_head..$new_head" -- \
                'yt_dlp/jsinterp*' \
                'yt_dlp/extractor/_extractors_gen.py' \
                2>/dev/null | head -20)
            [ -n "$cipher_changes" ] && echo "[JS interpreter / cipher]" && echo "$cipher_changes" && echo

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
                    'yt_dlp/extractor/youtube.py' \
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

if ! $ANY_CHANGES; then
    echo "All references are up to date."
fi
