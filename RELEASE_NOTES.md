### Features

- **Colored Twitch announcement support.** USERNOTICE messages with `msg-id=announcement` now parse as a distinct `announcement` `MessageType` (previously flattened to generic `system`), capturing `msg-param-color` (primary/blue/green/orange/purple) into a new `announcementColor` field. Chat replay sidebar shows announcements with a colored 4px left border + low-opacity background tint matching the announcement color; the niconico overlay swaps the default black text-shadow for a colored glow so announcements stand out against video. Ported from chatterino7 #6927.
