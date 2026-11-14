# Keyboard shortcuts

Every key in the [terminal UI](../guide/terminal-ui.md), then the
[web dashboard](../guide/web-dashboard.md) and [player](../guide/player.md)
shortcuts.

## Terminal UI basics

- **Chords** are a prefix key followed by an action key within 3 seconds: `A X` means press ++a++, then ++x++.
- **Confirm chords** need the action key a second time within 3 seconds: `A X X`. The second key shows a yellow `Press X to confirm …` prompt; any other key cancels.
- Letter keys are **case-insensitive** (`a` and `A` are the same), except `n` / `N` in a log search.
- Chords marked **batch** act on the rows ticked with ++space++ when any are ticked, and on the cursor row otherwise.
- Chords that need a job act on the selected (cursor) row. If that job does not qualify, the feedback line says why and nothing happens.
- Some chords only exist when the feature is available in the running process (noted below). `?` (Help) and `M` (Menu) always list exactly the chords available right now.
- While an overlay is open it owns every key, except ++ctrl+c++.

## Global keys

| Key | Action |
|---|---|
| ++ctrl+c++ | Quit immediately. Works everywhere, even inside overlays and text boxes. Stops all of Moombox. |
| `Q Q` | Quit. |
| ++tab++ / ++shift+tab++ | Move focus to the next / previous panel (Tasks → Details → Logs). |
| `M` | Open the action menu. |
| `` ` `` | Open Settings. |
| `?` | Open or close Help. |
| `F` | Tasks: cycle the status filter (all → active → issues → finished). Details: hide/show the description. Logs: cycle the log level (all → INFO+ → WARN+ → ERROR+). |
| ++esc++ | Clear the batch selection; otherwise clear an applied Tasks filter query; otherwise cancel an armed chord. |

## Action chords (`A`)

| Chord | Action | Applies to | Batch |
|---|---|---|---|
| `A A` | Add Video | — | |
| `A Z` | Import Archive (`.zip`) | — | |
| `A R` | Resume Job | YouTube jobs in `Error`, `Cancelled`, `COOKIES?`, or `Finished` with an incomplete tail, with staged files | yes |
| `A I I` | Reinitialize Job (confirm names what it deletes and any automatic action it cancels) | `Error`, `Cancelled`, `COOKIES?` | yes |
| `A M M` | Mux Job | `Error` / `Cancelled` with staged segments; `Finished` with an unmuxed part | |
| `A S S` | Recover Set-aside Recordings | Jobs not `Upcoming`, `Live`, `Downloading` or `Muxing` that have set-aside recordings | |
| `A X X` | Cancel Job | `Downloading`, `Live`, `Upcoming`, `Queued`, `Muxing`, `COOKIES?` | yes |
| `A D D` | Delete Job | `Finished`, `Error`, `Cancelled`, `COOKIES?` | yes |
| `A W` | Toggle Watched | `Finished` | yes |
| `A T` | Trim Video | `Finished` with an output file | |
| `A O` | Browse Orphaned Items | — | |
| `A K` | Manage Client Tokens | — | |
| `A C` | Import Chapters | Complete `Finished` jobs with an archive | |
| `A U U` | Restore Original Chapters | Jobs whose chapters were imported | |

## Request chords (`R`)

| Chord | Action | Notes |
|---|---|---|
| `R B` | Re-scan Feed History | Full-catalog backfill re-scan of every configured YouTube channel. |
| `R C` | Recheck Cookies | Re-validates the cookies on disk per platform. |
| `R F` | Refresh Cookies from Browser | Automated-browser refresh or browser-profile import, per [`cookies.acquisition`](../configuration/cookies.md#cookiesacquisition) and [`cookies.auto_enabled`](../configuration/cookies.md#cookiesauto_enabled). |
| `R V` | Check for Updates | |
| `R M` | Check Monitors Now | Runs every monitor's check now. |
| `R N` | View Release Notes | The pending update's notes, or the running version's. |
| `R U` | Apply Update | Only when an update is available. With active downloads, press `R U` again within 5 s to confirm. |
| `R S` | Verify Signature | Checks the binary's signature and release manifest. |
| `R P P` | Restart Program | |

## Open chords (`O`)

| Chord | Action | Applies to |
|---|---|---|
| `O F` | Open Folder (output folder, or staging while active) | `Finished` with an output file; `Upcoming`, `Live`, `Downloading`, `Muxing` |
| `O S` | Open Stream Page in a browser | Jobs with a stream URL |
| `O C` | Copy Stream URL (best effort: OSC 52, plus `clip.exe` on a local Windows console) | Jobs with a stream URL |
| `O W` | Open Web UI (`http(s)://localhost:<port>`) | — |
| `O L` | Open Job Log | Any job |
| `O G` | Open GitHub Page | — |

`O F`, `O S`, `O W` and `O G` open on the machine running Moombox.

## Extras chords (`E`)

| Chord | Action |
|---|---|
| `E Y` | yt-dlp Plugin status and install |
| `E L` | Cookie Login (browser sign-in) |
| `E I` | Import Cookie File (Netscape `cookies.txt`) |
| `E T` | Statistics |

## Tasks panel

| Key | Action |
|---|---|
| ++up++ / ++down++ | Move the cursor. |
| ++page-up++ / ++page-down++ | Page up / down. |
| ++home++ / ++end++ | First / last row. |
| ++enter++ | On the `Archived (N)` divider: expand or collapse the archive. |
| ++space++ | Tick or untick the row for batch actions. |
| ++ctrl+a++ | Tick every row shown (the rows the current filter leaves). ++esc++ clears the ticks. |
| `/` | Open the filter query box. |

Inside the filter query box:

| Key | Action |
|---|---|
| typing | Filters the list live. |
| ++enter++ | Close the box, keep the query. |
| ++esc++ | Close the box and clear the query. |

Query syntax: plain text, `"quoted phrase"`, `status:` (a status name, or `active` / `issues` / `errors` / `finished`), `channel:"Name"`, `platform:youtube` / `platform:twitch`, `type:clip`, `-` to negate, `a|b` for either. See [Terminal UI → Filtering](../guide/terminal-ui.md#filtering).

## Details panel

| Key | Action |
|---|---|
| ++up++ / ++down++ | Scroll. |
| ++page-up++ / ++page-down++ | Page scroll. |
| ++ctrl+u++ / ++ctrl+d++ | Half-page scroll. |
| `F` | Hide / show the description. |

## Logs panel

| Key | Action |
|---|---|
| ++up++ / ++down++ | Scroll (scrolling up pauses auto-scroll). |
| ++page-up++ / ++page-down++ | Page scroll. |
| ++ctrl+u++ / ++ctrl+d++ | Half-page scroll. |
| ++end++ | Resume auto-scroll. |
| `F` | Cycle the minimum level. |
| `/` | Search: type, then ++enter++ (empty ++enter++ clears). ++esc++ while typing cancels the box. |
| `n` / `N` | Next / previous match (wraps). |
| ++esc++ | Clear an applied search. |
| `c` | Clear the log view (not the log file). |

## Mouse

| Action | Effect |
|---|---|
| Click a panel | Focus it. |
| Click a task row | Select it. Clicking the archive divider expands or collapses it. |
| Wheel | Scroll the panel under the pointer, 3 rows a notch. |

## Overlays

### Action menu (`M`)

| Key | Action |
|---|---|
| ++up++ / ++down++, ++page-up++ / ++page-down++, ++home++ / ++end++ | Move. |
| ++enter++ | Run the action, or open its job picker / confirm. |
| `M`, ++esc++ | Close. |
| Job picker: ++enter++ | Choose the job; ++enter++ again confirms. |
| Job picker: ++esc++ | Cancel the pending confirm, or go back to the list. |
| Confirm box: ++enter++ / ++esc++ | Confirm / cancel. |

Mouse clicks and the wheel only move the selection.

### Settings (`` ` ``)

| Key | Action |
|---|---|
| ++up++ / ++down++ | Previous / next field. |
| typing | Edit a text or number field. |
| ++left++ / ++right++ | Toggle, cycle a choice, or step presets. |
| ++shift+left++ / ++shift+right++ | Previous / next section. |
| ++tab++ | Next section (wraps). |
| ++shift+down++ | Go to the **Save & Return** / **Return** buttons. |
| Buttons: ++left++ / ++right++, ++enter++, ++up++ | Pick a button, activate it, back to the fields. |
| ++esc++ | Close (asks to save if there are unsaved changes). |
| Save prompt: `Y` / `N` / ++esc++ | Save and close / discard and close / keep editing. |
| Network section, on a non-text row: `` ` `` / `~` | Set or change / remove the dashboard password. |
| Paths → FFmpeg path row: ++ctrl+o++ | Open the FFmpeg install check. |
| Channels: `A` / ++enter++ / `D` | Add / edit / remove a channel. |
| Channel removal prompt: ++enter++ / `P` / any other key | Remove and keep its jobs / remove and delete its pending jobs (only when it has some) / cancel. Applied when you save. |
| Integrations: `A` / ++enter++ / `D` / `T` | Add / edit / delete / test a notification webhook. |
| Notification editor: ++space++ / `m` | Toggle an option or event / toggle an event's mention. |
| Restart Required box: ++enter++ / ++esc++ | Restart now / close without restarting. |

Settings also accepts mouse clicks.

### Help (`?`) and release notes (`R N`)

| Key | Action |
|---|---|
| ++up++ / ++down++, ++page-up++ / ++page-down++, ++ctrl+u++ / ++ctrl+d++ | Scroll. |
| `?`, ++esc++ | Close Help. |
| ++esc++, `Q` | Close release notes. |
| `U` | Release notes of a pending update: apply it. |
| `S` | Release notes of a pending update: skip that version. |

### Job log (`O L`)

| Key | Action |
|---|---|
| ++up++ / ++down++, ++page-up++ / ++page-down++, ++ctrl+u++ / ++ctrl+d++, wheel | Scroll. |
| ++end++ | Follow new lines. |
| `/` | Search; ++enter++ finds, ++esc++ cancels. |
| `n` / `N` | Next / previous match. |
| ++esc++ | Clear the search; with no search, close. |
| `Q` | Close. |

### Add Video (`A A`)

| Step | Keys |
|---|---|
| URL | Type the URL or ID; ++tab++ toggles Advanced Options; ++enter++ continues; ++esc++ cancels. |
| Video / audio format | `A` auto (best); `N` none (audio only / video only); `1`–`9` pick a row; ++up++ / ++down++ and ++enter++ pick the highlighted row; ++esc++ back. |
| Timestamps | ++tab++ switches start / end; ++enter++ continues; ++esc++ back. Formats: `HH:MM:SS`, `MM:SS` or seconds. |
| Confirmation | ++enter++ submits; ++esc++ back. |

### Import Archive (`A Z`)

| Step | Keys |
|---|---|
| File browser | ++up++ / ++down++ move; ++right++ / ++enter++ open a folder or select a file; ++left++ / ++backspace++ go up; ++esc++ cancels. |
| Metadata | ++tab++ switches title / channel; ++enter++ imports; ++esc++ back to the browser. |

### Trim Video (`A T`)

| Mode | Keys |
|---|---|
| Create: times | ++tab++ switches start / end; ++enter++ continues; `M` delete mode; ++esc++ cancel. |
| Create: confirm | ++enter++ creates the trim; `M` delete mode; ++esc++ back. |
| Create: encoding | ++esc++ closes and keeps encoding in the background. |
| Delete | ++up++ / ++down++ select (wraps); ++enter++ twice deletes; `M` or ++esc++ back to create mode. |

### Orphaned items (`A O`) and client tokens (`A K`)

| Key | Action |
|---|---|
| ++up++ / ++down++, ++page-up++ / ++page-down++, ++home++ / ++end++ | Move. |
| `D` `D` (within 3 s) | Orphans: delete the selected file or history entry. Tokens: revoke the selected token. |
| `A` `A` (within 3 s) | Orphans: delete every item in the current group. |
| `R` | Refresh. |
| ++esc++ | Close. |

### Import Chapters (`A C`)

| Key | Action |
|---|---|
| typing | Edit the pasted chapters or the offset. |
| ++tab++ / ++shift+tab++ | Switch between the paste and the offset. |
| ++ctrl+s++ | Save (asks to confirm). |
| Confirm: `y` / ++enter++ | Save. Any other key goes back. |
| ++esc++ | Cancel. While saving, close (the save keeps running). |

### Cookie Login (`E L`)

| State | Keys |
|---|---|
| Platform list | ++up++ / ++down++ choose YouTube, Twitch or Close; ++enter++ (or ++tab++) selects; ++esc++ closes. |
| Browser open | ++enter++ extracts the cookies once you have signed in; ++esc++ cancels. |
| Timed out | `R` tries again; `S` skips. |

### Import Cookie File (`E I`)

| Key | Action |
|---|---|
| typing | The path to `cookies.txt` (`~` and quotes accepted). |
| ++enter++ | Import. On the result: close. |
| ++esc++ | Close (an import already running carries on). |
| `Q` | Close the result. |

### yt-dlp Plugin (`E Y`) and Statistics (`E T`)

| Key | Action |
|---|---|
| `I` | yt-dlp Plugin: install or reinstall. |
| `R` | Refresh. |
| ++esc++, `Q` | Close. |

### FFmpeg check

Shown at startup when FFmpeg is missing, or from Settings with ++ctrl+o++.

| Key | Action |
|---|---|
| ++up++ / ++down++, ++enter++ | Choose Install FFmpeg, Custom FFmpeg path, Skip for now, or Quit Moombox. |
| ++esc++ | On the first screen: quit Moombox. Elsewhere: back. |
| Custom path: ++enter++ | Check the path. |
| Any key after success | Close. |

## Web dashboard

These work anywhere in the dashboard except while typing in a field, while a
dialog is open, or with ++ctrl++, ++alt++ or ++cmd++ held (those combinations
are left to the browser). The letter keys are lowercase: with Caps Lock on,
`a` and `f` do nothing.

| Key | Action |
|---|---|
| `a` | Add video (not on the Player tab). |
| `?` | Show the keyboard shortcuts dialog. |
| `1`–`8` | Switch tab: Tasks, Archived, Player, Imports, Orphaned, Stats, Logs, Settings. A Settings tab with unsaved changes asks first, as a click does. |
| `f` | Focus the filter box (Tasks and Archived tabs). |
| ++up++ / ++down++ | Move through the job list (Tasks tab; wraps). |
| ++enter++ | Open the highlighted job's details (Tasks tab). |
| ++esc++ | Clear the job selection; in a dialog, close it. |

In the filter box: ++enter++ applies the query, ++backspace++ in an empty box
removes the last filter chip, and ++esc++ closes the suggestions and leaves the
box.

## Web player

Active on the Player tab when a video is loaded, except while typing, while a
dialog or the resume prompt is open, or with ++ctrl++, ++alt++ or ++cmd++
held.

| Key | Action |
|---|---|
| ++space++ | Play / pause. On a focused button or checkbox, ++space++ activates that control instead. |
| ++left++ / ++right++ | Seek 5 seconds back / forward. |
| ++shift+left++ / ++shift+right++ | Seek 30 seconds. |
| ++up++ / ++down++ | Volume up / down by 10%. |
| `F` | Toggle fullscreen. |
| `M` | Toggle mute. |
| `C` | Toggle **Overlay chat**. |
| `S` | Toggle **Sidebar chat**. |

Unlike the dashboard keys, the player's letter shortcuts also work with Caps Lock or ++shift++ on.
