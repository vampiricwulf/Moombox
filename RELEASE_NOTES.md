### Improvements

- **Launcher/supervisor restart model** — Moombox now starts as a lightweight launcher that spawns the application as a child process. Both config restarts and update restarts exit the child and let the launcher respawn it, eliminating process chain buildup across multiple restarts and keeping terminal state clean.
- **Consolidated restart logic** — All restart triggers (settings save, update apply, setup wizard, API) now share a single code path, reducing duplication and making the restart mechanism easier to maintain.
- **Simplified update route dependencies** — `UpdateRouteDeps` now uses a single `OnRestart` callback instead of three separate fields, matching the pattern used by all other route handlers.
