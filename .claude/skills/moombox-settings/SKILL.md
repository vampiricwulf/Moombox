---
name: moombox-settings
description: Use when adding, modifying, or renaming a configuration setting — covers config struct, defaults, validation, API, Web UI, TUI, hot-reload, and migration
---

# Settings Workflow

Adding a setting touches 7+ files across config, API, Web UI, and TUI. Missing any step means an incomplete or broken feature.

## Checklist

Complete in order — later steps depend on earlier ones.

### 1. Config Struct
`internal/config/types.go` — Add field to the appropriate nested struct with TOML and JSON tags.
```go
SomeSetting string `toml:"some_setting" json:"some_setting"`
```
- Use `*int`/`*string` for optional fields with `omitempty`
- Use `json:"-"` to exclude from API responses (e.g., password hashes)
- Use `FlexDuration` for time-based settings (supports both numeric and duration string parsing like `"10m"`, `"7d"`)

### 2. Default Value
`internal/config/config.go` → `Defaults()` — Set the default value. For FlexDuration: `FlexDuration{Value: 10}`.
- A default that differs by CPU architecture goes through `platformDefaults(goarch)` in the same file — arm64-motivated caps only (ruling R3: "any future cap whose value was picked for arm's memory belongs here"). `goarch` is a parameter, not `runtime.GOARCH` read inline, so both branches are testable on one host.
- Not every field needs an entry here: `network.public_url` has none because `""` is both its zero value and its documented "unset" state, and `loadFromFile` decodes over `Defaults()` — the same fact step 9's "no migration" rests on for a key like this.

### 3. Config Validation
`internal/config/config.go` → `validateOrNormalize(cfg, reportOnly)` — Add bounds checking, enum validation, or path sanitization. It is the one implementation behind two entry points (DECISIONS #9): `Validate(cfg)` (`reportOnly=true`) reports issues without mutating, and `Normalize(cfg)` (`reportOnly=false`) replaces the offending field with its default. Load normalises; Save validates first and refuses to write a failing config.

### 4. API Validation
`internal/web/routes/config_routes.go` → `validateConfigUpdates()` — Validate the field from API input. Returns `map[string]string` of field→error mappings (e.g., `"network.port": "must be 1-65535"`). Must match constraints from step 3.

### 5. API Application
`internal/web/routes/config_routes.go` → `applyConfigUpdates()` — Apply the field from the snake_case update map to the config struct. Only explicitly allowlisted fields are applied.
- FlexDuration fields: accept both `float64` and `string`, call `config.ParseFlexDuration()`
- Pointer optionals: handle nil vs zero (can set to `&value` or `nil`)

### 6. Web UI
`web/public/modules/settings.js`:
- Add Shoelace input element with `cfg-` prefixed ID (e.g., `cfg-log-max-files`)
- Populate in `populateConfigForm()`
- Gather in `saveConfig()` and include in nested snake_case payload
- Listen for `sl-change`/`sl-input` events for dirty tracking
- If restart required: add `{ path, id }` entry to `RESTART_REQUIRED_FIELDS` array

### 7. TUI Settings
`internal/tui/settings.go`:
- Add `fieldDef` to the appropriate section with type (`fieldText`, `fieldNumber`, `fieldToggle`, or `fieldCycle`)
- Add loading logic in `loadValues()` — for FlexDuration use `.Minutes()` or `.Days()`, for pointers default to sensible value when nil
- Add applying logic in `applyValues()` — for FlexDuration wrap back: `FlexDuration{Value: float64(v)}`, for booleans check `== "Yes"`

### 8. Hot-Reload (if runtime-changeable)
Nine callbacks on `ConfigRoutesCallbacks` (`internal/web/routes/config_routes.go`) support hot-reload; everything else requires restart:
- `OnLogLevelChange(level)` → `log.SetLevel()`
- `OnMaxParallelChange(n)` → `dlWorker.SetParallelDownloads()`
- `OnHideFinishedAgeChanged()` → re-broadcasts the job list
- `OnChannelChange()` → `kickMonitors` to re-evaluate channels
- `OnNotificationsChange()` → `notifyMgr.Reload()` — the notification targets follow the save; it also fires for a `network.public_url` change, and the per-target `enabled`/`mention`/`mention_events` keys all hot-reload through it too (the restart list stays at 16)
- `OnGoSoftLimitChange(mb)` → `debug.SetMemoryLimit` (0 restores the boot limit)
- `OnTrustForwardedProtoChange(trust)` → the `internal/web` atomic flag
- `OnFfmpegPathChange(path)` → `applyFfmpegPath` (`cmd/moombox/hot_reload.go`): `SetFfmpegPath` on the trim service and the download worker
- `OnReorderBudgetChange(downloaderCfg)` → `runState.applyReorderBudget`

`applyReorderBudget` (`cmd/moombox/hot_reload.go`) is the model for a process-wide value: ONE method reached from all three entry points — `initServices` at boot, this PUT callback, and the TUI's `OnSaveConfig` hot-reload block — so the number never travels through `engine.DownloaderOptions` or the worker's per-job call sites.

To add: wire the callback in `ConfigRoutesCallbacks` and connect it in `cmd/moombox`.

### 8b. Config-file-only keys
Some keys are deliberately `config.toml`-only: no Settings row in either UI, absent from both restart lists, documented in `config.example.toml` and the `data-and-storage.md` table, and no `applyConfigUpdates` arm (so a Settings save from either UI leaves them as loaded — `PUT /api/config` merges per key, and a hand-crafted PUT cannot set them either). Each one is a recorded owner ruling, never a shortcut:
- `downloader.progress_interval_ms` (default 16, min 1 — snapshotted per job start)
- `cookies.dpapi_profile_dir` (re-read on every DPAPI pass)

### 9. Config Migration (if renaming/moving)
`internal/config/config.go` → `migrateOldFormat()` — Non-destructive: only applies when new section doesn't exist. Converts legacy field to current location.

## Restart-Required Fields

Both UIs check if changed fields require restart. The two lists are `RESTART_REQUIRED_FIELDS` (`web/public/modules/settings.js`) and `restartRequiredKeys` (`internal/tui/settings.go`), pinned equal by `TestRestartRequiredListsAgree`. Current list (16): `network.{port,network_access,https_enabled,tls_cert_path,tls_key_path}`, `paths.{database_path,log_file_path}`, `logs.{log_max_file_size,log_max_files}`, `cookies.{cookie_file,refresh_interval,auto_enabled,browser_profile_dir}`, `connectivity.probe_targets`, `memory.sidecar_hard_limit_mb`, `bgutils.use_sidecar`.

## Common Mistakes

- Forgetting `applyConfigUpdates()` — field validates but never persists
- Adding to Web UI but not TUI (or vice versa) — breaks dual-UI parity
- Web/Go/TUI validation constraints out of sync — inconsistent error behavior
- Not adding to `RESTART_REQUIRED_FIELDS` for fields that need restart
- Missing default in `Defaults()` — zero value may cause unexpected behavior
- Using raw `int` for a time-based field instead of `FlexDuration`
