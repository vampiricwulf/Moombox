# Statistics

Moombox keeps running totals of what it has recorded and how much disk it
uses. The same figures are shown in two places:

- the web dashboard's **Stats** tab (press ++6++), and
- the terminal UI's **Statistics** overlay, chord `E T`.

Both are built from the same snapshot, so they always agree.

## Opening it

=== "Web dashboard"

    Click the **Stats** tab. The figures load when you open the tab and
    refresh every 60 seconds while it stays open. Refreshing stops when you
    switch to another tab.

=== "Terminal UI"

    Press `E` then `T`. The overlay refreshes every 60 seconds while open.

    | Key | Action |
    |---|---|
    | ++r++ | Refresh now |
    | ++esc++ or ++q++ | Close |

    In a short terminal the overlay compacts itself: below 24 rows the
    **Finished / Error / Cancelled** sizes share one line and **Uptime** is
    left out.

## Storage

### Disk gauge

A bar showing how full the volume holding your [output
directory](../configuration/paths-and-logs.md) is, with the text
**420.5GB used of 1.8TB** and **1.4TB free (23.1% used)**.

The bar's colour follows the disk thresholds:

| Colour | Meaning |
|---|---|
| Green | Below the [warning threshold](../configuration/disk-memory-connectivity.md#diskdisk_warn_percent) (default 90 %). |
| Yellow | At or above the warning threshold. |
| Red | At or above the [critical threshold](../configuration/disk-memory-connectivity.md#diskdisk_critical_percent) (default 95 %). |

On the dashboard, the bar also has a **holding** state, with **· holding
backlog** added to the text: usage reached the critical threshold earlier and
has not yet fallen 2 points below it, so Moombox is still not starting new
backlog downloads, even though the reading itself may look normal again. (The
TUI's bar colours by the reading alone; its **Tasks** rows show **Held: disk
full** for the jobs that are waiting.) See [Storage](storage.md).

The disk is read every 6 minutes, and again straight away when you save a
change to the thresholds or the output directory. Until the first reading
exists the gauge says **No disk reading available**.

### Storage figures

| Figure | What it counts |
|---|---|
| **Total Recorded** | The combined size of the files Moombox has recorded for every job that is `Finished`, `Error` or `Cancelled`. |
| **Total Jobs** | Every job in the database, in any status, including archived ones. |
| **YouTube Storage** | The part of **Total Recorded** that belongs to YouTube jobs. |
| **Twitch Storage** | The part of **Total Recorded** that belongs to Twitch jobs. |
| **Finished** | Size of `Finished` jobs' files. |
| **Error** | Size of what `Error` jobs recorded. |
| **Cancelled** | Size of what `Cancelled` jobs recorded. |

**YouTube Storage** + **Twitch Storage** = **Finished** + **Error** +
**Cancelled** = **Total Recorded**.

!!! note "What the sizes include"
    The sizes are the file sizes Moombox stored for each job. Trims, files in
    staging, and files that no job owns (see the dashboard's **Orphaned** tab)
    are not counted, so **Total Recorded** is normally less than the space
    used on the disk. Jobs that are still running (`Downloading`, `Live`,
    `Muxing`) and jobs not yet started are not counted until they finish,
    fail or are cancelled.

    An imported archive counts as YouTube unless it was recognised as a
    Twitch recording.

## Activity

| Figure | What it counts |
|---|---|
| **Streams Archived** | The number of `Finished` jobs — live captures, VODs, clips and imports alike. |
| **Total Recording Time** | The combined length of all `Finished` recordings. Shown as hours, minutes and seconds, for example `123h 45m 6s`. |
| **Chat Messages** | The total number of chat messages captured across all `Finished` jobs. |
| **Active Downloads** | Jobs that are `Downloading` or `Live` right now. `Upcoming` and `Queued` jobs are waiting, not downloading, and are not counted. |
| **Muxing** | Jobs in `Muxing` right now. |
| **YouTube Jobs** | Every YouTube job in the database, any status. |
| **Twitch Jobs** | Every Twitch job in the database, any status. |
| **Uptime** | How long Moombox has been running since it last started or restarted. |

**YouTube Jobs** + **Twitch Jobs** = **Total Jobs**.

!!! tip
    The dashboard's status bar shows a live count of active work all the
    time: **▶ N** counts jobs that are `Downloading`, `Live` or `Muxing` (so
    it is **Active Downloads** + **Muxing**). See [Web
    dashboard](web-dashboard.md).

## Things to know

- The figures are computed from the job database and cached for about 5
  seconds, so a refresh right after a change can show the previous numbers.
- Deleting a job removes it from every figure, including **Streams
  Archived** and **Total Recording Time**. A finished job that ages into the
  dashboard's **Archived** tab stays in every figure; that tab is only a
  different list.
- A job you **Reinitialize** goes back to waiting, so its recorded size
  leaves the storage totals until it finishes, fails or is cancelled again.
- The figures cover this Moombox installation's database only. They are not
  sent anywhere.
