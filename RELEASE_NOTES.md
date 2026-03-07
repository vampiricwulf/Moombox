## Features

- Add Ed25519 signature verification for auto-updater — CI signs release binaries, updater verifies signatures before applying, preventing MITM attacks on the update process

## Improvements

- Add stale `.new` and `.new.sig` cleanup on startup for interrupted updates
