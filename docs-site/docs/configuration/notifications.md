# Notifications

The `[[notifications]]` array lists the places Moombox posts notifications
to. Each entry is one **target**: a Discord webhook, plus options that pick
which events it receives, whether it pings someone, and how it delivers a
job's progress. You can have as many targets as you like — for example one
channel for everything and a second, pinged one for failures only.

Discord webhooks are the only supported target type. For what each event
means, how messages look, batching, rate limits and outages, see the
[Notifications guide](../guide/notifications.md).

```toml title="config.toml"
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEf..."

[[notifications]]
url = "discord://123456789012345678/GhIjKl..."
events = ["error", "auth", "disk_warning", "sidecar_down", "update_failed"]
mention = "<@&987654321098765432>"
mode = "separate"
```

!!! info "When changes take effect"
    Every key on this page applies **immediately** when you save it in the
    web dashboard or the terminal UI — no restart. A target that is still
    configured keeps its queued notifications while its options change. A
    hand edit to `config.toml` is read at the next start.

Where to edit targets:

- **Web dashboard:** Settings → **Integrations** → **Notifications**. Each
  target is a card; **Add Notification** opens the *Add Webhook* dialog.
  Every change on a card (the switch, a chip, the mention field) is saved as
  soon as you make it. The paper-plane button on a card sends a test, and the
  bin button removes the target (after a confirmation).
- **Terminal UI:** Settings (++grave++) → **Integrations**. ++a++ adds,
  ++enter++ edits the selected target, ++d++ ++d++ deletes, ++t++ sends a
  test. In the editor, ++space++ toggles the focused row and ++m++ toggles
  the focused event's mention; ++enter++ keeps the edit, and the change is
  written when you save the settings (Esc → **Y**, or **Save & Return**).

### `notifications[].url`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| None (required) | A Discord webhook URL in one of the forms below | Immediately (hot reload) | Dashboard: Integrations → **Add Notification** → **Webhook URL** (the URL is shown at the top of each card; to change it, delete the card and add the new one). TUI: Integrations editor → **Webhook URL**. |

The webhook to post to. Two spellings are accepted:

| Form | Example |
|---|---|
| Full URL | `https://discord.com/api/webhooks/<id>/<token>` |
| Shorthand | `discord://<id>/<token>` |

Details:

- The full URL must be `https://`. `discordapp.com` (Discord's old domain)
  and the `ptb.` / `canary.` hosts are accepted and treated as
  `discord.com`; a trailing slash is ignored.
- A forum-thread webhook keeps its query: `…/<token>?thread_id=<id>` (or
  `discord://<id>/<token>?thread_id=<id>`) posts into that thread.
- The ID must be numeric. Anything glued onto the end of a paste (a `)` from
  a Markdown link, a stray space) makes the URL invalid.

To get a webhook URL in Discord: channel settings → **Integrations** →
**Webhooks** → **New Webhook** → **Copy Webhook URL**.

Edge cases:

- **Saving** an invalid URL from either UI is refused with the reason (for
  example "unsupported notification URL scheme (Discord webhooks only)").
  Use the dialog's **Send Test** button to check a URL before adding it.
- An invalid URL in a **hand-edited** file is skipped at startup with a
  warning in the log; the other targets still work.
- The same webhook listed twice — even in two spellings, such as the
  shorthand and the full URL — is one target and posts once. The first
  entry's mention and mode win; the two event filters are combined (if
  either has no filter, the merged target receives everything).
- An entry with an empty `url` is ignored.

!!! warning "The webhook URL is a password"
    Anyone with the URL can post to your channel. Moombox never writes the
    full URL to its logs or error messages (they show only
    `https://discord.com/…<redacted>`), but `config.toml` holds it in plain
    text — protect that file. If a URL leaks, delete the webhook in Discord
    and create a new one.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
```

### `notifications[].enabled`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| `true` (when absent) | `true` / `false` | Immediately (hot reload) | Dashboard: the switch in the card header (tooltip *Delivering* / *Muted — kept, but delivers nothing*). TUI: Integrations editor → **Enabled** (++space++). |

A mute switch. `false` keeps the target — URL, filter, mention, mode — in
the config but delivers nothing to it. Use it to silence a webhook for a
while without losing its settings.

- An absent key means enabled, so older configs keep working.
- A muted target's dashboard card is dimmed and carries a **Muted** badge;
  the TUI list marks it **Muted**.
- Clearing every event from the filter does **not** mute a target — an
  empty filter means *all events*. Use this switch instead.
- A muted target is not checked at startup, so a broken URL on a muted
  target produces no warning.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
enabled = false
```

### `notifications[].events`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| Absent (all events) | A list of event names (see below) | Immediately (hot reload) | Dashboard: card → **Filter...** to show the event chips, click a chip to turn it on or off; **Clear filter** goes back to all events. TUI: Integrations editor → the event list (++space++ on each row). |

An allowlist: when set, the target receives only these events. When absent
(or empty), it receives every event.

| Group | Events |
|---|---|
| Job Lifecycle | `found`, `added`, `scheduled`, `rescheduled`, `downloading`, `quality_split`, `gap_split`, `live_edge`, `muxing`, `finished`, `error`, `cancelled`, `auth`, `auth_recovered` |
| Connectivity | `connectivity_resume`, `connectivity_split`, `connectivity_restored` |
| Trim | `trim_created`, `trim_deleted`, `trim_error` |
| System | `disk_warning`, `disk_critical`, `disk_ok`, `update_available`, `update_applied`, `update_failed`, `crash_recovered`, `channel_unhealthy`, `channel_healthy`, `sidecar_down`, `sidecar_restored`, `captcha_required` |

The [Notifications guide](../guide/notifications.md) describes what
triggers each one.

Some events are **covered by an older or broader name**, so you do not need
to list both:

| Listing this… | …also delivers |
|---|---|
| `disk_warning` | `disk_critical`, `disk_ok` |
| `channel_unhealthy` | `channel_healthy` |
| `sidecar_down` | `sidecar_restored` |
| `auth` | `auth_recovered` |

The reverse is not true: a filter of `disk_ok` alone receives only the
all-clear. `captcha_required` covers both the captcha alert and its
*YouTube Captcha Cleared* close (they share the name).

Edge cases:

- In the dashboard, turning chips off until none are left, or turning all of
  them on, removes the filter (all events). The TUI does the same: zero or
  every box ticked saves as "no filter".
- A misspelled name in a hand-edited file never matches anything; Moombox
  warns about it in the log at startup ("filters on unknown event — it will
  never match"). Empty entries are ignored.
- A dashboard save strips unknown names from the list — unless that would
  leave it empty, because an empty filter would mean *everything*.
- `connectivity_pause` is a retired name. A filter that still lists it
  receives `connectivity_resume` (which now carries the pause time) and no
  warning is logged. Replace it with `connectivity_resume`: the alias is
  temporary. `connectivity_lost` no longer exists and is warned about.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
events = ["finished", "error", "cancelled", "disk_warning"]
```

### `notifications[].mention`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| Absent (no ping) | `<@&ROLE_ID>`, `<@USER_ID>` (or `<@!USER_ID>`), `@everyone`, `@here` | Immediately (hot reload) | Dashboard: card → **Mention** field (saved when you leave the field or press ++enter++). TUI: Integrations editor → **Mention**. |

Who to ping. Discord embeds can never ping anyone on their own, so this text
is put in the message itself, above the embeds, for the events chosen by
[`mention_events`](#notificationsmention_events).

| Value | Pings |
|---|---|
| `<@&123456789012345678>` | A role |
| `<@123456789012345678>` | A user (`<@!…>` is accepted and stored as `<@…>`) |
| `@here` | Everyone online in the channel |
| `@everyone` | Everyone in the channel |

To get an ID, turn on Developer Mode in Discord (User Settings → Advanced),
then right-click the role or user → **Copy ID**. If the text shows but
nobody is pinged, check the role's mention permissions in your server
settings.

Edge cases:

- Saving anything else is refused in both UIs: "mention … is not a role
  (<@&ID>), a user (<@ID>), @everyone, or @here". The dashboard keeps what
  you typed in the field so you can correct it.
- An invalid value in a hand-edited file is removed at startup (with a
  message), and the target then pings no one.
- Clearing the mention keeps your `mention_events` list; typing a mention
  back restores it.
- Only the named role or user is pinged — other mentions in the message
  text are never allowed to ping.
- In [`mode = "edit"`](#notificationsmode) only events that are posted as
  new messages can ping; Discord does not notify on an edited message.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
mention = "<@&987654321098765432>"
```

### `notifications[].mention_events`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| Absent: `error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered`, `sidecar_down` | A list of event names (same names as [`events`](#notificationsevents)); `[]` means never | Immediately (hot reload) | Dashboard: the **Mentions:** chip rows on the card (shown only while a mention is set). TUI: Integrations editor → ++m++ on an event row (the `@` column). |

Which events carry the [`mention`](#notificationsmention). It has no effect
while `mention` is empty. Three states:

| In `config.toml` | Meaning |
|---|---|
| key absent | The default six: `error`, `auth`, `disk_critical`, `update_failed`, `crash_recovered`, `sidecar_down` |
| `mention_events = []` | Never ping |
| a list | Ping for exactly these events |

This is separate from [`events`](#notificationsevents): an event must first
be delivered to the target to ping. A `mention_events` entry for an event
the filter excludes does nothing.

Edge cases:

- The broader names work here too: listing `disk_warning` also pings for
  `disk_critical`. The **all-clear** events (`disk_ok`, `channel_healthy`,
  `sidecar_restored`, `auth_recovered`) never ping through their alert's
  name — an all-clear asks nothing of anyone. List one explicitly if you
  want it to ping.
- The first change you make to the mention chips writes the whole list out,
  starting from the six defaults; turning the last chip off writes `[]`
  (never), not "back to defaults".
- On load, entries are trimmed and blanks and duplicates removed. A hand-typed
  unknown name is warned about in the log; a dashboard save removes it.
- Several notifications batched into one Discord message ping once.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
mention = "<@123456789012345678>"
mention_events = ["error", "finished"]
```

### `notifications[].mode`

| Default | Allowed values | When it takes effect | Where to change it |
|---|---|---|---|
| `separate` (when absent) | `separate`, `edit` | Immediately (hot reload) | Dashboard: card → **Delivery:** **Separate messages** / **One message per job**. TUI: Integrations editor → **Delivery** (++space++; the list row shows *· one message per job* when on). |

How the target delivers a job's progress.

`separate`
:   One Discord message per event, never changed afterwards. The default.

`edit`
:   One Discord message per job. The first lifecycle event the target
    receives for a job creates the message; every later one rewrites it in
    place. The rewritten message carries a **Status** field (for example
    *Downloading*, *Muxing*, *Finished*) and a **History** field with a
    relative timestamp per state, oldest lines dropped when it grows too
    long. This keeps a busy channel to one message per stream.

In edit mode:

- The lifecycle events that share the job's message are `found`, `added`,
  `scheduled`, `rescheduled`, `downloading`, `quality_split`, `gap_split`,
  `connectivity_resume`, `connectivity_split`, `muxing` and `finished`.
- `error` and `cancelled` do **both**: the job's message is rewritten to
  *Failed* / *Cancelled*, **and** a separate message is posted (this is the
  one that can ping). If no message is open for the job yet, they are simply
  posted.
- Everything else (`auth`, `live_edge`, trims, all System events) is always
  its own message.
- Discord does not notify on edits, so only new posts can ping — the
  event that creates the job's message, and the separate posts above.
- The 5-second batching (see the guide) is off for this target: every
  notification goes out on its own.
- The message ID is stored with the job, so a restart keeps editing the same
  message (its History starts again). If someone deletes the message in
  Discord, the next event posts a new one.
- Deleting the job in Moombox never deletes the Discord message.

Edge cases: any value other than `separate` or `edit` in a hand-edited file
is reset to `separate` at startup, with a message. The match is exact and
lower-case: `"Edit"` is reset too. Switching an existing
target to `edit` takes effect at each job's next event.

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGh"
mode = "edit"
```

## Related settings

- [`network.public_url`](network.md#networkpublic_url) — when set, each
  job notification's title links to that job in your dashboard instead of
  to YouTube or Twitch (the platform link moves to the channel line).
- [`disk.disk_warn_percent`](disk-memory-connectivity.md#diskdisk_warn_percent)
  and [`disk.disk_critical_percent`](disk-memory-connectivity.md#diskdisk_critical_percent)
  set when the disk alerts fire.
