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

### 3. Config Validation
`internal/config/config.go` → `validate()` — Add bounds checking, enum validation, or path sanitization. Replace invalid values with defaults. Called on both load and save.

### 4. API Validation
`internal/web/routes/jobs.go` → `validateConfigUpdates()` — Validate the field from API input. Returns `map[string]string` of field→error mappings (e.g., `"network.port": "must be 1-65535"`). Must match constraints from step 3.

### 5. API Application
`internal/web/routes/jobs.go` → `applyConfigUpdates()` — Apply the field from the snake_case update map to the config struct. Only explicitly allowlisted fields are applied.
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
Only 4 settings currently support hot-reload (most require restart):
- `OnLogLevelChange` → `log.SetLevel()`
- `OnMaxParallelChange` → `dlWorker.SetParallelDownloads()`
- `OnHideFinishedAgeChanged` → re-broadcasts job list
- `OnChannelChange` → `kickMonitors` to re-evaluate channels

To add: wire callback in `ConfigRoutesCallbacks` struct (`internal/web/routes/jobs.go`) and connect in `cmd/moombox/main.go`.

### 9. Config Migration (if renaming/moving)
`internal/config/config.go` → `migrateOldFormat()` — Non-destructive: only applies when new section doesn't exist. Converts legacy field to current location.

## Restart-Required Fields

Both UIs check if changed fields require restart. Current list: port, network_access, https_enabled, tls_cert_path, tls_key_path, database_path, log_file_path, log_max_file_size, log_max_files.

## Common Mistakes

- Forgetting `applyConfigUpdates()` — field validates but never persists
- Adding to Web UI but not TUI (or vice versa) — breaks dual-UI parity
- Web/Go/TUI validation constraints out of sync — inconsistent error behavior
- Not adding to `RESTART_REQUIRED_FIELDS` for fields that need restart
- Missing default in `Defaults()` — zero value may cause unexpected behavior
- Using raw `int` for a time-based field instead of `FlexDuration`
