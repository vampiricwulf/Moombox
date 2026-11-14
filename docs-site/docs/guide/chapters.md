# Chapters

Chapters split a recording into named sections — songs in a karaoke stream,
games in a variety stream, topics in a talk. Moombox writes chapters into the
archive when it muxes, and lets you replace them with a list pasted from a
viewer's timestamp comment.

## Where chapters come from

When Moombox muxes a single-file archive, it embeds the chapters it can find:

| Recording | Chapters embedded |
|---|---|
| YouTube VOD | The video's chapter markers, or failing that the timestamps in its description. |
| Live YouTube capture | The same, but only when the whole broadcast was recorded from its start; a capture that joined late gets none. |
| Twitch VOD | The VOD's moments (Twitch's chapter list). |
| Live Twitch capture | The archive VOD's moments when that VOD exists at the end of the capture; otherwise one chapter per category the stream was seen in, timed from the stream's start. |

A recording split into several parts gets no embedded chapters, because each
part is muxed while the broadcast is still running. Instead Moombox builds the
same list when the recording finishes and saves it beside the parts in a
`.chapters.json` file, which the Moombox player shows (other players do not
see it). You can also import chapters for a split recording (see below).

The [player](player.md) shows a recording's chapters as markers under
the video and as a clickable list. Other players (VLC, mpv, MPC-HC and so on)
read the embedded chapters too.

## Importing chapters from a comment

Viewers often post a timestamp list under a stream: a setlist, a game list, a
"chapters" comment. You can paste one in as the recording's chapters.

### Which recordings allow it

Only a **complete `Finished`** job:

- Not while the job is active (`Upcoming`, `Live`, `Downloading`, `Muxing`)
  — "The job is *Status*; chapters can be imported once it has finished".
- Not an `Error` or `Cancelled` job, and not a `Finished` job with an
  incomplete tail: those can still be resumed, and resuming rewrites the
  archive the import would be rewriting. The refusal reads "Chapters can be
  imported only into a finished, complete archive".

YouTube, Twitch and imported recordings all qualify.

### Step by step

=== "Web dashboard"

    1. Open the recording in the **Player** tab (or click **Play** in its job
       details).
    2. Click **Import chapters** under the video. (The button is hidden for a
       recording that does not qualify.)
    3. Paste the comment into **Timestamp comment**.
    4. Check the preview table (**Start**, **Title**) and adjust **Offset** if
       needed (see [The offset](#the-offset)). The preview updates as you type.
    5. Optionally open **Current chapters (N) — replaced on save** to compare
       with what the archive has now.
    6. Click **Save**. It is disabled until the preview has at least one
       chapter.

    The dialog closes and a toast says **Chapters imported**; the player
    reloads the chapter strip and list straight away. If you were playing
    the recording, playback carries on from the same position.

=== "Terminal UI"

    1. Select a finished job in the Tasks panel and press `A C` (**Import
       Chapters**). The chord is offered only for jobs that qualify;
       otherwise the menu says "no finished jobs with an archive".
    2. Paste the comment into the text area.
    3. Press ++tab++ to move to the **Offset** field and adjust it if needed;
       ++tab++ again goes back to the paste.
    4. Read the preview: **Preview: N chapters (offset …)**, the rows, and
       **Current: M chapters — replaced on save** with the current list.
    5. Press ++ctrl+s++. The dialog asks **Save N chapters, replacing the
       current M?** — press ++y++ or ++enter++ to save, any other key to go
       back.

    While saving the dialog shows "Saving — rewriting the archive with the
    chapters…". ++esc++ closes it; the save keeps running. The feedback line
    then reads "Imported N chapters: *title*".

    ++esc++ in the editing step cancels without saving.

### How a comment is read

Every line that carries a timestamp becomes one chapter. Everything else —
headings, separators, decorations, blank lines — is ignored.

A line is read in one of two ways:

1. **Timestamp first**: `12:34 First song`, `1:02:03 - Ending`,
   `0:06: six`. Leading decoration is stripped first: spaces (including the
   full-width space), tree marks such as `├ ┣ ┗ ┃ │ └ ╰`, and bullets
   `- – — • ・ * ▶ ►`. The title is the rest of the line, with the separator
   after the timestamp removed (space, `-`, `–`, `—`, `|`, `:`, `：`, `・`).
2. **Timestamp last**: `Opening talk 5:25`. Only when the line has exactly
   one timestamp and it ends the line; the title is what comes before it.

A line with a timestamp in the middle, or with two timestamps that do not
start the line, is ignored.

Accepted timestamp shapes are `M:SS`, `MM:SS` (minutes up to 99), `H:MM:SS`
and `HH:MM:SS`. Full-width digits and colons (`１２：３４`, as typed with a
Japanese IME) are accepted. A run that is not one of those shapes, such as
`1:2:3`, is not a timestamp, and a line led by one is ignored.

Then:

- Chapters are sorted by time, so an out-of-order comment is fine.
- Nesting is flattened: an indented sub-entry is a chapter like any other.
- If the same time appears twice, the first line's title is kept.
- A line with a timestamp but no title gets **Chapter N**.
- Emote shortcodes, emoji and brackets in titles are kept exactly as pasted.

!!! example
    ```text
    ✨ Setlist ✨
    0:00 Waiting room
    ┣ 3:12 Opening talk
    ┣ 12:40 First song
    ┃ ┗ 16:05 (encore)
    1:02:15 Ending
    ```

    gives five chapters: *Waiting room* at 0:00, *Opening talk* at 3:12,
    *First song* at 12:40, *(encore)* at 16:05 and *Ending* at 1:02:15. The
    `✨ Setlist ✨` line has no timestamp and is ignored.

### The offset

A comment's timestamps count from the start of the **broadcast** as the
viewer saw it. Your recording may start later — for example when Moombox
joined a live stream after it had started and could not record from the
beginning. The **Offset** is added to every timestamp to line the two up.

When the dialog opens, Moombox fills the offset in for you when it knows the
answer, and says where it came from in a note under the field:

| Note | Offset | When |
|---|---|---|
| **Pre-filled: joined 1:23:45 in** | `-1:23:45` | A live YouTube capture that started recording that far into the broadcast. |
| **Pre-filled: recorded from the start** | `0:00` | A live YouTube capture that has the whole broadcast. |
| **Pre-filled: VOD** | `0:00` | A VOD, which always starts at the beginning. |
| **start unknown — enter it by hand** | empty | Moombox does not know where the recording starts: a live Twitch capture, an imported file, or a recording made before Moombox noted the join point. |

You can always change it. The field accepts:

- seconds: `90`, `-90`, `12.5`
- a clock with an optional sign: `-1:30`, `+1:02:03`, `-1:02:03.033`
  (milliseconds allowed), and clocks of 100 hours or more for very long
  streams
- full-width digits, and the `−` minus sign often found in copied text

Anything else is refused with "Offset must be seconds or ±H:MM:SS".

!!! tip "Finding the offset by hand"
    Play the recording to a moment the comment names, note the player's
    time, and subtract the comment's time. If the comment says *First song
    12:40* and the song starts at 2:10 in your recording, the offset is
    `2:10 − 12:40 = -10:30`.

### How the list is fitted to the recording

After the offset is applied:

- Chapters that land before the start of the recording, or at or after its
  end, are dropped.
- If the first remaining chapter starts after 0:00, a chapter named
  **Intro** is added at 0:00 to cover the opening.
- Each chapter ends where the next begins; the last ends at the end of the
  recording.

Errors shown in the preview:

| Message | Cause |
|---|---|
| "No timestamps found" | No line in the paste carries a timestamp. |
| "No timestamps fall within the recording" | After the offset, every chapter is outside the recording. Check the offset's sign. |
| "The recording's length is unknown, so chapters cannot be fitted to it" | Moombox could not read the archive's duration. |
| "Offset must be seconds or ±H:MM:SS" | The offset field could not be read. |

### What saving does

1. The first time you import into a recording, Moombox keeps the chapters the
   archive had (possibly none) as its **original**, so they can be restored.
   Later imports replace the imported list but keep that same original.
2. The new list is saved in a `.chapters.json` file beside the archive.
3. For a single-file archive, the file is re-muxed with the new chapters
   embedded, so every player sees them. This rewrites the archive and needs
   free disk space for a temporary copy.

If the re-mux fails (for example the disk is full), the archive is left
exactly as it was and the chapters are still saved beside it; the player uses
them, but other players will not see them. The dashboard warns: *"Chapters
imported; the file could not be rewritten, so they are saved beside it: …"*;
the TUI says "— saved beside the file, which could not be rewritten: …".

A split recording is never re-muxed: the list is saved beside its parts and
used by the player. The TUI's message ends "(saved beside the parts)".

If Moombox cannot read the archive's current chapters on the first import, it
refuses rather than risk losing them: "The archive's current chapters could not
be read, so they could not be kept to restore; nothing was changed".

The import cannot run while another operation is using the job's files — a
trim being made, a set-aside recovery, or a mux — and a trim cannot start
while an import is rewriting the archive. Whichever comes second is refused;
try again once the other has finished.

## Restoring the original chapters

After an import you can go back to the chapters the recording had before the
**first** import.

=== "Web dashboard"

    Open **Import chapters** for the recording. When it has imported
    chapters, a **Restore original** button appears in the dialog's footer.
    Click it and confirm: "Replace the imported chapters with the original
    N?" (or, if there were none, "…The archive had no chapters before the
    import."). The toast says **Original chapters restored**.

=== "Terminal UI"

    Select the job and press `A U` (**Restore Original Chapters**), then
    press ++u++ again within 3 seconds to confirm. The chord is offered only
    for jobs with imported chapters ("no jobs with imported chapters"
    otherwise). The feedback line reads "Restored the original N chapters:
    *title*".

Restoring re-muxes the archive the same way an import does.

!!! warning
    **Reinitialize** deletes a job's imported chapters along with its other
    recorded data; the reinitialize confirmation lists them. See
    [Jobs](jobs.md).

## Limits and notes

- The comment parser is deliberately forgiving: the list does not have to
  be in order or start at 0:00.
- Imported chapters are never applied to a recording automatically; each
  import is something you do.
- The `.chapters.json` file beside the archive holds both the imported list
  and the original. Deleting it removes the ability to restore.
