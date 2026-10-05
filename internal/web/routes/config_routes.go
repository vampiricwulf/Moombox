package routes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	net2 "net" // aliased: "net" is shadowed by the network update map in this file
	"net/http"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// flexDurationValue extracts the numeric value of a FlexDuration update for
// validation, accepting both the JSON-number form and the string form
// ("12h", "0.5d") that applyConfigUpdates parses. unit must match the apply
// site's unit so both parse identically. Returns ok=false for any other type
// or an unparseable string (the apply path keeps the stored value then).
func flexDurationValue(raw any, unit string) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case string:
		fd := config.ParseFlexDuration(v, unit, math.NaN())
		if math.IsNaN(fd.Value) {
			return 0, false
		}
		return fd.Value, true
	}
	return 0, false
}

// configETag returns a stable short ETag for a marshaled-config response
// body. Hashing the bytes (rather than maintaining a config-mutation
// counter) keeps the route purely functional in body → header. Audit
// reports/web.md Q-1.
func configETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// ConfigRoutesCallbacks contains optional callbacks invoked when config changes require hot-reload.
type ConfigRoutesCallbacks struct {
	// OnLogLevelChange is called when the log_level config field changes.
	OnLogLevelChange func(level string)
	// OnMaxParallelChange is called when num_parallel_downloads changes.
	OnMaxParallelChange func(n int)
	// OnHideFinishedAgeChanged is called when hide_finished_age_days changes,
	// so callers can re-broadcast the job list with updated archive thresholds.
	OnHideFinishedAgeChanged func()
	// OnChannelChange is called when channels are added, updated, or removed,
	// so monitors can re-evaluate their channel lists immediately.
	OnChannelChange func()
	// OnNotificationsChange is called when the notifications list changes,
	// so the notification manager can hot-reload its targets (previously
	// edits silently required a restart nothing prompted for).
	OnNotificationsChange func()
	// OnGoSoftLimitChange is called when memory.go_soft_limit_mb changes so the
	// runtime soft limit follows without a restart (0 = disable).
	OnGoSoftLimitChange func(mb int)
	// OnTrustForwardedProtoChange is called when network.trust_forwarded_proto
	// changes; the web package's atomic flag follows.
	OnTrustForwardedProtoChange func(trust bool)
	// OnFfmpegPathChange is called when paths.ffmpeg_path changes so services
	// that captured the path at construction (TrimService) rebuild.
	OnFfmpegPathChange func(path string)
	// OnReorderBudgetChange is called when either downloader reorder ceiling
	// (reorder_buffer_mb / reorder_budget_mb) changes, so the engine's
	// process-wide budget follows the save without a restart. It takes the
	// whole saved DownloaderConfig because the two keys are reconciled
	// against each other (see config.DownloaderConfig.ReorderLimitBytes).
	OnReorderBudgetChange func(d config.DownloaderConfig)
}

// pathFieldError returns the per-field error for a user-supplied path value,
// or "" when the value is acceptable.
//
// It rejects ".." segments and nothing else. Absolute paths are deliberately
// allowed: the Docker entrypoint seeds every path field as "/data/...", the
// TUI has always accepted absolute values, and config.toml has always taken
// them by hand — so the old "no absolute paths" rule (inherited verbatim from
// the pre-Go TypeScript safePathSchema, not a considered Go boundary) made
// EVERY settings save from a containerized dashboard 400 with no UI
// workaround. PUT /api/config is admin-only, so the rule bought no
// containment it did not already concede.
//
// config.PathHasTraversal is shared with the TUI so the two UIs cannot
// disagree about what a valid path is — that disagreement was the bug.
//
// required mirrors config.Validate's non-empty path fields. Without it an
// empty (or whitespace-only) value for one of those passed validation, was
// applied, and then failed inside config.Save as an opaque 500 "failed to
// save config" naming no field — the same unsavable-settings-page symptom
// from the opposite direction. The TUI settings panel already blocked these.
func pathFieldError(p string, required bool) string {
	if required && strings.TrimSpace(p) == "" {
		return "must not be empty"
	}
	if config.PathHasTraversal(p) {
		return "Path cannot contain a .. segment"
	}
	return ""
}

// ffmpegPathError refuses an FFmpeg path whose executable is not named
// ffmpeg (or ffmpeg.exe). Moombox runs whatever this names — `-version` on
// the check route, every mux after it is stored — and a LAN client could
// point it at bytes it planted: POST /api/import writes an uploaded file
// under the output directory, and Windows runs a PE whatever its extension.
// An imported file is always named "<title> [<id>].<ext>", so it can never
// carry this name. Empty means "ffmpeg from PATH" and passes.
func ffmpegPathError(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(p, `\`, "/")))
	if base != "ffmpeg" && base != "ffmpeg.exe" {
		return "the executable must be named ffmpeg (or ffmpeg.exe)"
	}
	return ""
}

// newFFmpegPathError applies ffmpegPathError to a paths.ffmpeg_path update
// only when it CHANGES the stored value: the dashboard's full-form save
// sends the stored path back on every save, and a path stored before this
// rule (or hand-edited into config.toml) must not make every unrelated save
// fail on a field the operator never touched.
func newFFmpegPathError(updates map[string]any, stored string) string {
	paths, ok := updates["paths"].(map[string]any)
	if !ok {
		return ""
	}
	v, ok := paths["ffmpeg_path"].(string)
	if !ok || strings.TrimSpace(v) == strings.TrimSpace(stored) {
		return ""
	}
	return ffmpegPathError(v)
}

// pathField names one path-shaped config field and whether config.Validate
// refuses to persist it empty.
type pathField struct {
	key      string
	required bool
}

// validateConfigUpdates validates the config update map against the field
// constraints config.Validate enforces, so a bad value is a 400 naming the
// field rather than a save the store refuses. Returns a map of field->error
// messages (empty if valid).
func validateConfigUpdates(updates map[string]any) map[string]string {
	errs := make(map[string]string)

	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			if v < 1 || v > 65535 {
				errs["network.port"] = "port must be between 1 and 65535"
			}
		}
		if v, ok := net["network_access"].(string); ok {
			switch v {
			case "localhost", "lan", "external":
			default:
				errs["network.network_access"] = "network_access must be localhost, lan, or external"
			}
		}
		if v, ok := net["client_token_ttl_days"].(float64); ok {
			if v < 1 || v > 3650 {
				errs["network.client_token_ttl_days"] = "client_token_ttl_days must be between 1 and 3650"
			}
		}
		if v, ok := net["trusted_proxies"].([]any); ok {
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					errs["network.trusted_proxies"] = "trusted_proxies must be an array of strings"
					break
				}
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				valid := false
				if strings.Contains(s, "/") {
					_, _, err := net2.ParseCIDR(s)
					valid = err == nil
				} else {
					valid = net2.ParseIP(s) != nil
				}
				if !valid {
					errs["network.trusted_proxies"] = fmt.Sprintf("%q is not a valid IP or CIDR", s)
					break
				}
			}
		}
		// public_url: validated at the API edge with the same
		// config.ValidatePublicURL the apply arm canonicalises with, so a
		// value the route accepts is never one Normalize then silently
		// rewrites behind the operator's back.
		if v, ok := net["public_url"].(string); ok {
			if _, err := config.ValidatePublicURL(v); err != nil {
				errs["network.public_url"] = err.Error()
			}
		}
		// Empty means "no TLS material configured" — legitimate.
		for _, f := range []pathField{{"tls_cert_path", false}, {"tls_key_path", false}} {
			if v, ok := net[f.key].(string); ok {
				if msg := pathFieldError(v, f.required); msg != "" {
					errs["network."+f.key] = msg
				}
			}
		}
	}

	// Paths sub-fields. ffmpeg_path may be empty ("use ffmpeg from PATH");
	// the other four are required by config.Validate.
	if paths, ok := updates["paths"].(map[string]any); ok {
		for _, f := range []pathField{
			{"log_file_path", true},
			{"database_path", true},
			{"output_directory", true},
			{"staging_directory", true},
			{"ffmpeg_path", false},
		} {
			if v, ok := paths[f.key].(string); ok {
				if msg := pathFieldError(v, f.required); msg != "" {
					errs["paths."+f.key] = msg
				}
			}
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			switch strings.ToUpper(v) {
			case "DEBUG", "INFO", "WARN", "ERROR":
			default:
				errs["logs.log_level"] = "log_level must be DEBUG, INFO, WARN, or ERROR"
			}
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			if v < 1024 || v > 1073741824 {
				errs["logs.log_max_file_size"] = "log_max_file_size must be between 1024 and 1073741824"
			}
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			if v < 1 || v > 100 {
				errs["logs.log_max_files"] = "log_max_files must be between 1 and 100"
			}
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["archive_window_days"].(float64); ok {
			if v < 1 || v > 3650 {
				errs["monitors.archive_window_days"] = "archive_window_days must be between 1 and 3650"
			}
		}
		if v, ok := mon["archive_slots"].(float64); ok {
			if v < 1 || v > 100 {
				errs["monitors.archive_slots"] = "archive_slots must be between 1 and 100"
			}
		}
		if v, ok := mon["decapi_check_interval"].(float64); ok {
			if v < 15 || v > 3600 {
				errs["monitors.decapi_check_interval"] = "decapi_check_interval must be between 15 and 3600"
			}
		}
		if v, ok := mon["twitch_check_interval"].(float64); ok {
			if v < 5 || v > 3600 {
				errs["monitors.twitch_check_interval"] = "twitch_check_interval must be between 5 and 3600"
			}
		}
		// FlexDuration fields accept BOTH JSON numbers and strings ("12h",
		// "0.5d") in the apply path — validate both forms here, or an
		// out-of-range string passes the API validator and only fails later
		// in config.Save with an opaque 500 instead of a clean 400.
		if raw, exists := mon["feed_check_interval"]; exists {
			if v, ok := flexDurationValue(raw, "minutes"); ok && (v < 1 || v > 1440) {
				errs["monitors.feed_check_interval"] = "feed_check_interval must be between 1 and 1440"
			}
		}
		if raw, exists := mon["hide_finished_age_days"]; exists {
			// Match the canonical 0..365 range enforced by config.Validate.
			if v, ok := flexDurationValue(raw, "days"); ok && (v < 0 || v > 365) {
				errs["monitors.hide_finished_age_days"] = "hide_finished_age_days must be between 0 and 365"
			}
		}
		if raw, exists := mon["probe_cooldown"]; exists {
			// 0 disables, no maximum — only a negative value is invalid. Match
			// config.Validate so a hand-edited TOML can't sneak past the API.
			if v, ok := flexDurationValue(raw, "seconds"); ok && v < 0 {
				errs["monitors.probe_cooldown"] = "probe_cooldown must be >= 0 seconds (0 disables)"
			}
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["output_template"].(string); ok {
			if len(v) > 500 {
				errs["downloader.output_template"] = "output_template must be at most 500 characters"
			}
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			if v < 1 {
				errs["downloader.num_parallel_downloads"] = "num_parallel_downloads must be at least 1"
			}
		}
		// segment_workers: no upper bound — a high value is honoured exactly
		// as written (DECISIONS: owner-mandated). Only < 1 is rejected.
		if v, ok := dl["segment_workers"].(float64); ok {
			if v < 1 {
				errs["downloader.segment_workers"] = "must be >= 1"
			}
		}
		// The two reorder ceilings: 0 is the documented "unbounded" value on
		// either, so only a negative is rejected and there is no maximum.
		// Matches config.Validate — a hand-edited TOML and a PUT must be
		// judged the same way.
		if v, ok := dl["reorder_buffer_mb"].(float64); ok {
			if v < 0 {
				errs["downloader.reorder_buffer_mb"] = "reorder_buffer_mb must be >= 0 MB (0 = unbounded)"
			}
		}
		if v, ok := dl["reorder_budget_mb"].(float64); ok {
			if v < 0 {
				errs["downloader.reorder_budget_mb"] = "reorder_budget_mb must be >= 0 MB (0 = unbounded)"
			}
		}
		// 0 = unbounded (ruling R1); mirrors config.Validate's floor.
		if v, ok := dl["max_video_resolution"].(float64); ok {
			if v < 0 {
				errs["downloader.max_video_resolution"] = "max_video_resolution must be at least 0 (0 = unbounded)"
			}
		}
		if v, ok := dl["maximum_timeout"].(float64); ok {
			if v < 30 {
				errs["downloader.maximum_timeout"] = "maximum_timeout must be at least 30 seconds"
			}
		}
		// interruption_timeout: 0 disables the resume stall, no maximum —
		// only a negative value is invalid. Match config.Validate so a
		// hand-edited TOML can't sneak past the API.
		if raw, exists := dl["interruption_timeout"]; exists {
			if v, ok := flexDurationValue(raw, "minutes"); ok && v < 0 {
				errs["downloader.interruption_timeout"] = "interruption_timeout must be >= 0 minutes (0 disables)"
			}
		}
		// incomplete_staging_expiry_days: 0 preserves forever, no maximum —
		// only a negative value is invalid. Match config.Validate.
		if raw, exists := dl["incomplete_staging_expiry_days"]; exists {
			if v, ok := flexDurationValue(raw, "days"); ok && v < 0 {
				errs["downloader.incomplete_staging_expiry_days"] = "incomplete_staging_expiry_days must be >= 0 days (0 preserves forever)"
			}
		}
	}

	// Disk sub-fields
	if dk, ok := updates["disk"].(map[string]any); ok {
		warnPct, warnOK := dk["disk_warn_percent"].(float64)
		critPct, critOK := dk["disk_critical_percent"].(float64)
		if warnOK {
			// 1..99 mirrors config.validateOrNormalize — a value this
			// validator called valid was then refused inside config.Save as
			// an opaque 500 "failed to save config" (CORE-21).
			if warnPct < 1 || warnPct > 99 {
				errs["disk.disk_warn_percent"] = "disk_warn_percent must be between 1 and 99"
			}
		}
		if critOK {
			if critPct < 1 || critPct > 99 {
				errs["disk.disk_critical_percent"] = "disk_critical_percent must be between 1 and 99"
			}
		}
		if warnOK && critOK && critPct <= warnPct {
			errs["disk.disk_critical_percent"] = "critical threshold must be higher than warning threshold"
		}
	}

	// Memory sub-fields. 0 disables a given knob; any other value must be
	// a sensible MB number. Hard sidecar limit must sit above the soft
	// trigger or the GC fires constantly without ever reclaiming.
	if mk, ok := updates["memory"].(map[string]any); ok {
		goSoft, hasGoSoft := mk["go_soft_limit_mb"].(float64)
		sideSoft, hasSideSoft := mk["sidecar_soft_limit_mb"].(float64)
		sideHard, hasSideHard := mk["sidecar_hard_limit_mb"].(float64)
		if hasGoSoft && (goSoft < 0 || goSoft > 65536) {
			errs["memory.go_soft_limit_mb"] = "go_soft_limit_mb must be between 0 and 65536"
		}
		if hasSideSoft && (sideSoft < 0 || sideSoft > 65536) {
			errs["memory.sidecar_soft_limit_mb"] = "sidecar_soft_limit_mb must be between 0 and 65536"
		}
		if hasSideHard && (sideHard < 0 || sideHard > 65536) {
			errs["memory.sidecar_hard_limit_mb"] = "sidecar_hard_limit_mb must be between 0 and 65536"
		}
		if hasSideSoft && hasSideHard && sideSoft > 0 && sideHard > 0 && sideHard <= sideSoft {
			errs["memory.sidecar_hard_limit_mb"] = "hard limit must be higher than soft limit"
		}
	}

	// Connectivity sub-fields
	if conn, ok := updates["connectivity"].(map[string]any); ok {
		if v, ok := conn["probe_targets"].([]any); ok {
			valid := 0
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					errs["connectivity.probe_targets"] = "probe_targets must be an array of host:port strings"
					break
				}
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				if _, _, err := net2.SplitHostPort(s); err != nil {
					errs["connectivity.probe_targets"] = fmt.Sprintf("%q is not a valid host:port", s)
					break
				}
				valid++
			}
			if _, bad := errs["connectivity.probe_targets"]; !bad && valid == 0 {
				errs["connectivity.probe_targets"] = "at least one probe target is required"
			}
		}
	}

	// Cookies sub-fields
	if ck, ok := updates["cookies"].(map[string]any); ok {
		// browser_profile_dir may be empty ("auto-cookies not configured");
		// cookie_file is required by config.Validate.
		for _, f := range []pathField{{"cookie_file", true}, {"browser_profile_dir", false}} {
			if v, ok := ck[f.key].(string); ok {
				if msg := pathFieldError(v, f.required); msg != "" {
					errs["cookies."+f.key] = msg
				}
			}
		}
		// browser_path: must be empty (auto-detect) or pass the static
		// browser-validation checks (absolute, exists, executable, known type).
		// Full --version check is intentionally NOT run here to avoid blocking
		// the request handler; that's done by /api/auto-cookies/validate-browser-path.
		if pathRaw, hasPath := ck["browser_path"].(string); hasPath {
			path := strings.TrimSpace(pathRaw)
			if path != "" {
				typ, _ := ck["browser_type"].(string)
				if err := cookies.ValidateBrowserPathQuick(path, strings.TrimSpace(typ)); err != nil {
					errs["cookies.browser_path"] = err.Error()
				}
			}
		}
		// browser_type alone (without browser_path) is allowed but unused — no validation needed
		if raw, exists := ck["refresh_interval"]; exists {
			// 10..10080 mirrors config.validateOrNormalize (CORE-21). The
			// string form ("12h") is checked too: applyConfigUpdates parses
			// it, and an out-of-range string that reached the store failed
			// Validate there and came back as a 500.
			if v, ok := flexDurationValue(raw, "minutes"); ok && (v < 10 || v > 10080) {
				errs["cookies.refresh_interval"] = "refresh_interval must be between 10 and 10080"
			}
		}
		// acquisition: mirrors config.validateOrNormalize's enum exactly, so a
		// value the API accepts is never one Normalize then rewrites behind
		// the operator's back. Empty is the "leave it at the default" case and
		// is not an error — applyConfigUpdates treats it the same way.
		if v, ok := ck["acquisition"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "", "auto", "profile":
			default:
				errs["cookies.acquisition"] = "acquisition must be auto or profile"
			}
		}
	}

	// Channels — audit R-4: PUT /config accepts a channels[] replace, and
	// POST /api/config/channels is the other writer. Validate each entry here
	// so a bulk replace can't smuggle in empty IDs, duplicates, or unknown
	// platforms (the platform rule is validChannelPlatform, shared with POST).
	if chs, ok := updates["channels"].([]any); ok {
		// The decode gate, ahead of the per-field rules. applyConfigUpdates
		// decodes this array through the same helper and assigns nothing when
		// the decode fails, so without a 400 here one type-mismatched field
		// in one entry silently drops the whole channels update behind a 200
		// (the stored list stays as it was; the operator's edit vanishes).
		if entries, decErrs := decodeConfigEntries[config.ChannelConfig]("channels", chs); decErrs != nil {
			maps.Copy(errs, decErrs)
		} else {
			// Every entry decoded, so entries[i] is chs[i]. The overrides
			// Save's Validate would refuse — a 500 with the field's name
			// lost, had they got that far.
			for i, ch := range entries {
				for field, msg := range config.ChannelOverrideErrors(ch) {
					errs[fmt.Sprintf("channels[%d].%s", i, field)] = msg
				}
			}
		}
		seen := make(map[string]bool, len(chs))
		for i, raw := range chs {
			obj, ok := raw.(map[string]any)
			if !ok {
				errs[fmt.Sprintf("channels[%d]", i)] = "must be an object"
				continue
			}
			id, _ := obj["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				errs[fmt.Sprintf("channels[%d].id", i)] = "channel ID required"
				continue
			}
			if seen[id] {
				errs[fmt.Sprintf("channels[%d].id", i)] = "duplicate channel ID"
				continue
			}
			seen[id] = true
			if v, ok := obj["platform"].(string); ok && !validChannelPlatform(v) {
				errs[fmt.Sprintf("channels[%d].platform", i)] = "platform must be youtube or twitch"
			}
		}
	}

	// Notifications — mention must be a well-formed Discord mention token
	// (config.ParseMention's four accepted forms); a token Discord cannot
	// resolve renders as literal text and pings nobody, which looks like a
	// delivery failure. mode must be one of the two the file loader accepts.
	// Unknown event names are NOT errors here: they are
	// stripped in applyConfigUpdates, where an all-unknown Events filter is
	// deliberately left as written rather than rejected or emptied.
	if notifs, ok := updates["notifications"].([]any); ok {
		// Same decode gate as the channels arm above, and the same reason:
		// `"enabled": "false"` on one target used to drop the whole
		// notifications update behind a 200.
		if _, decErrs := decodeConfigEntries[config.NotificationConfig]("notifications", notifs); decErrs != nil {
			maps.Copy(errs, decErrs)
		}
		for i, raw := range notifs {
			nm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if v, ok := nm["mention"].(string); ok && v != "" {
				if _, _, _, err := config.ParseMention(v); err != nil {
					errs[fmt.Sprintf("notifications[%d].mention", i)] = err.Error()
				}
			}
			// The per-target delivery mode. Matches the config-side constraint
			// in validateOrNormalize — a value the file loader would refuse
			// must not be reachable through a PUT either.
			if v, ok := nm["mode"].(string); ok {
				switch v {
				case "", "separate", "edit":
				default:
					errs[fmt.Sprintf("notifications[%d].mode", i)] = `mode must be "separate" or "edit"`
				}
			}
		}
	}

	return errs
}

// decodeConfigEntries re-decodes one of PUT /api/config's object arrays —
// `channels` and `notifications`, the two the SPA round-trips whole — into its
// typed slice, through the JSON round trip both arms used separately before.
//
// One helper, and it decodes ENTRY BY ENTRY, because the failure it exists for
// is per entry and used to be silent: a single type-mismatched field
// (`"enabled": "false"` on a raw PUT) made json.Unmarshal fail for the WHOLE
// array, the `if … == nil` guard around the assignment skip, and the whole
// array update drop silently (the stored list stayed as it was) — while the route answered 200. The
// per-entry decode is what lets the error name the entry and the field, so
// validateConfigUpdates can turn it into a 400 instead.
//
// A non-empty error map means NOTHING is returned: a partial array is the same
// silent loss under another name.
func decodeConfigEntries[T any](field string, raw []any) ([]T, map[string]string) {
	out := make([]T, 0, len(raw))
	var errs map[string]string
	fail := func(key, msg string) {
		if errs == nil {
			errs = map[string]string{}
		}
		errs[key] = msg
	}
	for i, item := range raw {
		key := fmt.Sprintf("%s[%d]", field, i)
		data, err := json.Marshal(item)
		if err != nil {
			fail(key, "is not encodable as JSON")
			continue
		}
		var entry T
		if err := json.Unmarshal(data, &entry); err != nil {
			var te *json.UnmarshalTypeError
			if errors.As(err, &te) {
				if te.Field != "" {
					key += "." + te.Field
				}
				fail(key, fmt.Sprintf("expected %s, got %s", jsonTypeName(te.Type), te.Value))
				continue
			}
			fail(key, "is not a valid entry")
			continue
		}
		out = append(out, entry)
	}
	if errs != nil {
		return nil, errs
	}
	return out, nil
}

// jsonTypeName renders a Go type as the JSON type an operator typed against,
// so the 400 reads "expected bool, got string" rather than naming a Go kind
// nobody sent. A pointer field (NotificationConfig.Enabled is *bool, the
// three-state trick) reports what it points at — the operator wrote a bool or
// they did not.
func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return jsonTypeName(t.Elem())
	case reflect.Bool:
		return "bool"
	case reflect.String:
		return "string"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	default:
		return t.Kind().String()
	}
}

// applyConfigUpdates applies allowlisted config fields from a snake_case map
// to the config struct. Used by both PUT /config and POST /setup/complete.
func applyConfigUpdates(cfg *config.MoomboxConfig, updates map[string]any) {
	// Network sub-fields
	if net, ok := updates["network"].(map[string]any); ok {
		if v, ok := net["port"].(float64); ok {
			cfg.Network.Port = int(v)
		}
		if v, ok := net["network_access"].(string); ok {
			cfg.Network.NetworkAccess = v
		}
		if v, ok := net["https_enabled"].(bool); ok {
			cfg.Network.HTTPSEnabled = v
		}
		if v, ok := net["tls_cert_path"].(string); ok {
			cfg.Network.TLSCertPath = v
		}
		if v, ok := net["tls_key_path"].(string); ok {
			cfg.Network.TLSKeyPath = v
		}
		if v, ok := net["client_token_ttl_days"].(float64); ok {
			cfg.Network.ClientTokenTTLDays = int(v)
		}
		if v, ok := net["trust_forwarded_proto"].(bool); ok {
			cfg.Network.TrustForwardedProto = v
		}
		if v, ok := net["trusted_proxies"].([]any); ok {
			proxies := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						proxies = append(proxies, s)
					}
				}
			}
			cfg.Network.TrustedProxies = proxies
		}
		if v, ok := net["public_url"].(string); ok {
			// The error case is unreachable here — validateConfigUpdates
			// already rejected an unusable value — but fall back to leaving
			// the stored value untouched rather than writing garbage.
			if canonical, err := config.ValidatePublicURL(v); err == nil {
				cfg.Network.PublicURL = canonical
			}
		}
	}

	// Paths sub-fields
	if paths, ok := updates["paths"].(map[string]any); ok {
		if v, ok := paths["log_file_path"].(string); ok {
			cfg.Paths.LogFilePath = v
		}
		if v, ok := paths["database_path"].(string); ok {
			cfg.Paths.DatabasePath = v
		}
		if v, ok := paths["output_directory"].(string); ok {
			cfg.Paths.OutputDirectory = v
		}
		if v, ok := paths["staging_directory"].(string); ok {
			cfg.Paths.StagingDirectory = v
		}
		if v, ok := paths["ffmpeg_path"].(string); ok {
			cfg.Paths.FfmpegPath = v
		}
	}

	// Logs sub-fields
	if logs, ok := updates["logs"].(map[string]any); ok {
		if v, ok := logs["log_level"].(string); ok {
			cfg.Logs.LogLevel = v
		}
		if v, ok := logs["log_max_file_size"].(float64); ok {
			cfg.Logs.LogMaxFileSize = int(v)
		}
		if v, ok := logs["log_max_files"].(float64); ok {
			cfg.Logs.LogMaxFiles = int(v)
		}
	}

	// Monitors sub-fields
	if mon, ok := updates["monitors"].(map[string]any); ok {
		if v, ok := mon["archive_window_days"].(float64); ok {
			cfg.Monitors.ArchiveWindowDays = int(v)
		}
		if v, ok := mon["archive_slots"].(float64); ok {
			cfg.Monitors.ArchiveSlots = int(v)
		}
		if v, ok := mon["feed_check_interval"].(float64); ok {
			cfg.Monitors.FeedCheckInterval = config.FlexDuration{Value: v}
		} else if vs, ok := mon["feed_check_interval"].(string); ok {
			cfg.Monitors.FeedCheckInterval = config.ParseFlexDuration(vs, "minutes", cfg.Monitors.FeedCheckInterval.Value)
		}
		if val, exists := mon["decapi_check_interval"]; exists {
			if v, ok := val.(float64); ok {
				n := int(v)
				cfg.Monitors.DecapiCheckInterval = &n
			} else {
				cfg.Monitors.DecapiCheckInterval = nil
			}
		}
		if val, exists := mon["twitch_check_interval"]; exists {
			if v, ok := val.(float64); ok {
				n := int(v)
				cfg.Monitors.TwitchCheckInterval = &n
			} else {
				cfg.Monitors.TwitchCheckInterval = nil
			}
		}
		if v, ok := mon["hide_finished_age_days"].(float64); ok {
			cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: v}
		} else if vs, ok := mon["hide_finished_age_days"].(string); ok {
			cfg.Monitors.HideFinishedAgeDays = config.ParseFlexDuration(vs, "days", cfg.Monitors.HideFinishedAgeDays.Value)
		}
		if v, ok := mon["probe_cooldown"].(float64); ok {
			cfg.Monitors.ProbeCooldown = config.FlexDuration{Value: v}
		} else if vs, ok := mon["probe_cooldown"].(string); ok {
			cfg.Monitors.ProbeCooldown = config.ParseFlexDuration(vs, "seconds", cfg.Monitors.ProbeCooldown.Value)
		}
		if v, ok := mon["membership_discovery"].(bool); ok {
			cfg.Monitors.MembershipDiscovery = &v
		}
	}

	// Downloader sub-fields
	if dl, ok := updates["downloader"].(map[string]any); ok {
		if v, ok := dl["max_video_resolution"].(float64); ok {
			cfg.Downloader.MaxVideoResolution = int(v)
		}
		if v, ok := dl["output_template"].(string); ok {
			cfg.Downloader.OutputTemplate = v
		}
		if v, ok := dl["num_parallel_downloads"].(float64); ok {
			cfg.Downloader.NumParallelDownloads = int(v)
		}
		if v, ok := dl["segment_workers"].(float64); ok {
			cfg.Downloader.SegmentWorkers = int(v)
		}
		if v, ok := dl["reorder_buffer_mb"].(float64); ok {
			cfg.Downloader.ReorderBufferMB = int(v)
		}
		if v, ok := dl["reorder_budget_mb"].(float64); ok {
			cfg.Downloader.ReorderBudgetMB = int(v)
		}
		if v, ok := dl["download_chat"].(bool); ok {
			cfg.Downloader.DownloadChat = v
		}
		if v, ok := dl["prefer_60fps"].(bool); ok {
			cfg.Downloader.Prefer60fps = v
		}
		if v, ok := dl["maximum_timeout"].(float64); ok {
			cfg.Downloader.MaximumTimeout = int(v)
		}
		if v, ok := dl["interruption_timeout"].(float64); ok {
			cfg.Downloader.InterruptionTimeout = config.FlexDuration{Value: v}
		} else if vs, ok := dl["interruption_timeout"].(string); ok {
			cfg.Downloader.InterruptionTimeout = config.ParseFlexDuration(vs, "minutes", cfg.Downloader.InterruptionTimeout.Value)
		}
		if v, ok := dl["incomplete_staging_expiry_days"].(float64); ok {
			cfg.Downloader.IncompleteStagingExpiryDays = config.FlexDuration{Value: v}
		} else if vs, ok := dl["incomplete_staging_expiry_days"].(string); ok {
			cfg.Downloader.IncompleteStagingExpiryDays = config.ParseFlexDuration(vs, "days", cfg.Downloader.IncompleteStagingExpiryDays.Value)
		}
	}

	// Cookies
	if ck, ok := updates["cookies"].(map[string]any); ok {
		if v, ok := ck["cookie_file"].(string); ok {
			cfg.Cookies.CookieFile = v
		}
		if v, ok := ck["auto_enabled"].(bool); ok {
			cfg.Cookies.AutoEnabled = v
		}
		if v, ok := ck["browser_profile_dir"].(string); ok {
			cfg.Cookies.BrowserProfileDir = v
		}
		if v, ok := ck["browser_path"].(string); ok {
			cfg.Cookies.BrowserPath = strings.TrimSpace(v)
		}
		if v, ok := ck["browser_type"].(string); ok {
			cfg.Cookies.BrowserType = strings.TrimSpace(v)
		}
		if v, ok := ck["platforms"].([]any); ok {
			var platforms []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					platforms = append(platforms, s)
				}
			}
			cfg.Cookies.Platforms = platforms
		}
		if v, ok := ck["active_platforms"].([]any); ok {
			// Non-nil even when empty: [] is the explicit "both off"
			// override, distinct from no override at all.
			activePlatforms := []string{}
			for _, p := range v {
				if s, ok := p.(string); ok {
					activePlatforms = append(activePlatforms, s)
				}
			}
			cfg.Cookies.ActivePlatforms = activePlatforms
		}
		if val, exists := ck["refresh_interval"]; exists {
			if v, ok := val.(float64); ok {
				cfg.Cookies.RefreshInterval = config.FlexDuration{Value: v}
			} else if vs, ok := val.(string); ok {
				cfg.Cookies.RefreshInterval = config.ParseFlexDuration(vs, "minutes", cfg.Cookies.RefreshInterval.Value)
			} else {
				// null resets to the default. Zero is not a usable "unset":
				// Validate refuses anything under 10 minutes, so storing it
				// failed the save with a 500.
				cfg.Cookies.RefreshInterval = config.Defaults().Cookies.RefreshInterval
			}
		}
		if v, ok := ck["dpapi_fallback"].(bool); ok {
			cfg.Cookies.DpapiFallback = v
		}
		// Trimmed and lower-cased for the same reason browser_path is trimmed:
		// the TUI's and the dashboard's controls both feed strings, and a
		// value that differs from the enum only in case or whitespace would
		// pass validation above and then be silently replaced by Normalize.
		// An empty string is left to config.Normalize, which fills in the
		// default — assigning "" here and letting it through is what makes a
		// UI that omits the control harmless.
		if v, ok := ck["acquisition"].(string); ok {
			cfg.Cookies.Acquisition = strings.ToLower(strings.TrimSpace(v))
		}
	}

	// Disk
	if dk, ok := updates["disk"].(map[string]any); ok {
		if v, ok := dk["disk_warn_percent"].(float64); ok {
			cfg.Disk.WarnPercent = int(v)
		}
		if v, ok := dk["disk_critical_percent"].(float64); ok {
			cfg.Disk.CriticalPercent = int(v)
		}
	}

	// Updates
	if upd, ok := updates["updates"].(map[string]any); ok {
		if v, ok := upd["auto_check_updates"].(bool); ok {
			cfg.Updates.AutoCheckUpdates = v
		}
	}

	// Memory
	if mk, ok := updates["memory"].(map[string]any); ok {
		if v, ok := mk["go_soft_limit_mb"].(float64); ok {
			cfg.Memory.GoSoftLimitMB = int(v)
		}
		if v, ok := mk["sidecar_soft_limit_mb"].(float64); ok {
			cfg.Memory.SidecarSoftLimitMB = int(v)
		}
		if v, ok := mk["sidecar_hard_limit_mb"].(float64); ok {
			cfg.Memory.SidecarHardLimitMB = int(v)
		}
	}

	// Bgutils
	if bg, ok := updates["bgutils"].(map[string]any); ok {
		if v, ok := bg["use_sidecar"].(bool); ok {
			cfg.Bgutils.UseSidecar = v
		}
	}

	// Connectivity
	if conn, ok := updates["connectivity"].(map[string]any); ok {
		if v, ok := conn["probe_targets"].([]any); ok {
			targets := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						targets = append(targets, s)
					}
				}
			}
			if len(targets) > 0 {
				cfg.Connectivity.ProbeTargets = targets
			}
		}
	}

	// Notifications
	// Decoded through decodeConfigEntries, the shared helper the channels arm
	// below also uses, rather than rebuilt field by field: every tagged field
	// carries through unconditionally, so a field a later arc adds (N3's
	// `mode`) needs no route edit here and survives a save written before it
	// existed — the SPA round-trips the whole stored array on every settings
	// save, so a field this route didn't know about would otherwise be
	// silently dropped. A decode error is already a 400 by the time control
	// reaches here (validateConfigUpdates runs the same helper), so the
	// nothing-decoded arm is the unreachable one.
	if notifs, ok := updates["notifications"].([]any); ok {
		if ncs, decErrs := decodeConfigEntries[config.NotificationConfig]("notifications", notifs); decErrs == nil {
			for i := range ncs {
				n := &ncs[i]
				if n.Mention != "" {
					// Unreachable on error — validateConfigUpdates already
					// rejected an unusable mention — so an error here leaves
					// the decoded (operator-typed) value untouched.
					if canonical, _, _, err := config.ParseMention(n.Mention); err == nil {
						n.Mention = canonical
					}
				}
				// Unknown event names are stripped UNLESS doing so would
				// empty an otherwise non-empty filter: buildTargets treats an
				// absent/nil Events filter as "every event", so reducing an
				// all-garbage filter to empty would turn "matches nothing"
				// into "matches everything". The manager's startup Warn is
				// what tells the operator about the garbage that survives,
				// and the web UI's own chip editor can't produce this state.
				if len(n.Events) > 0 {
					kept := make([]string, 0, len(n.Events))
					for _, e := range n.Events {
						if notifications.KnownEvents[e] {
							kept = append(kept, e)
						}
					}
					if len(kept) > 0 {
						n.Events = kept
					}
				}
				// mention_events has no such trap — stripping only ever
				// narrows who gets pinged, and an emptied list is the
				// meaningful "never" — so unknown entries are dropped
				// unconditionally, all the way down to an explicit empty
				// list.
				if n.MentionEvents != nil {
					kept := make([]string, 0, len(*n.MentionEvents))
					for _, e := range *n.MentionEvents {
						if notifications.KnownEvents[e] {
							kept = append(kept, e)
						}
					}
					n.MentionEvents = &kept
				}
			}
			cfg.Notifications = ncs
		}
	}

	// Channels
	if chs, ok := updates["channels"].([]any); ok {
		if channels, decErrs := decodeConfigEntries[config.ChannelConfig]("channels", chs); decErrs == nil {
			cfg.Channels = channels
		}
	}
}

// ConfigRoutes registers config-related API routes. The Store carries the
// cfg pointer + lock; PUT /api/config keeps the copy-on-write pattern so
// validation or save failures never leak partial mutations into the live
// config seen by other readers.
func ConfigRoutes(r chi.Router, store *config.Store, callbacks *ConfigRoutesCallbacks) {
	mu := store.RWMutex()
	cfg := store.Config()

	// GET /api/config
	r.Get("/api/config", func(rw http.ResponseWriter, req *http.Request) {
		// Snapshot the config under the read lock, then release before
		// encoding to the socket — json.NewEncoder.Encode streams its
		// output directly, so a slow client would otherwise hold the
		// RLock for the full TCP send window and block any writer
		// (PUT /config, setup/complete, password change) until the
		// client finished receiving. PasswordHash has json:"-" so it's
		// omitted from marshaling regardless.
		cfgCopy := store.Snapshot()

		resp := struct {
			*config.MoomboxConfig
			HasPassword bool `json:"hasPassword"`
		}{
			MoomboxConfig: cfgCopy,
			HasPassword:   cfgCopy.Network.PasswordHash != "",
		}
		// ETag + 304 short-circuit: the config payload includes a large
		// channels slice that rarely changes; serving 304s when the body
		// is byte-identical avoids re-shipping it on every settings-tab
		// open. SHA-256 of the marshalled body is overkill for a TTL-less
		// cache key but it's quick and gives stable ordering. Audit
		// reports/web.md Q-1.
		body, err := json.Marshal(resp)
		if err != nil {
			jsonError(rw, "marshal config", http.StatusInternalServerError)
			return
		}
		etag := configETag(body)
		rw.Header().Set("ETag", etag)
		rw.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
		if match := req.Header.Get("If-None-Match"); match != "" && match == etag {
			rw.WriteHeader(http.StatusNotModified)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write(body)
	})

	// PUT /api/config
	r.Put("/api/config", func(rw http.ResponseWriter, req *http.Request) {
		var updates map[string]any
		if err := json.NewDecoder(req.Body).Decode(&updates); err != nil {
			jsonError(rw, "invalid request body", http.StatusBadRequest)
			return
		}

		// Validate the field constraints before anything is applied.
		validationErrs := validateConfigUpdates(updates)
		var storedFFmpeg string
		store.Read(func(c *config.MoomboxConfig) { storedFFmpeg = c.Paths.FfmpegPath })
		if msg := newFFmpegPathError(updates, storedFFmpeg); msg != "" {
			validationErrs["paths.ffmpeg_path"] = msg
		}

		// Notification webhook URLs must parse at save time — previously a
		// bad paste was accepted with a success toast, then silently
		// warn-skipped at the next startup ("notifications just don't
		// work"). Only NEW/EDITED URLs are validated: a pre-existing
		// hand-edited entry (e.g. a legacy non-Discord URL, tolerated as a
		// boot-time warn-skip) is grandfathered, because the web UI's
		// full-form save always includes the stored notifications array —
		// hard-failing it would block every unrelated settings save. The
		// TUI editor validates on edit the same way.
		if notifs, ok := updates["notifications"].([]any); ok {
			existing := make(map[string]struct{})
			store.Read(func(c *config.MoomboxConfig) {
				for _, nc := range c.Notifications {
					existing[nc.URL] = struct{}{}
				}
			})
			for i, n := range notifs {
				if nm, ok := n.(map[string]any); ok {
					if v, ok := nm["url"].(string); ok && v != "" {
						if _, known := existing[v]; known {
							continue
						}
						if err := notifications.ValidateURL(v); err != nil {
							validationErrs[fmt.Sprintf("notifications[%d].url", i)] = err.Error()
						}
					}
				}
			}
		}

		if len(validationErrs) > 0 {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(rw).Encode(map[string]any{
				"error":   "Validation failed",
				"details": validationErrs,
			})
			return
		}

		mu.Lock()

		// Prevent enabling external access without a password
		if net, ok := updates["network"].(map[string]any); ok {
			if v, ok := net["network_access"].(string); ok && v == "external" {
				if cfg.Network.PasswordHash == "" {
					mu.Unlock()
					jsonError(rw, "A password must be set before enabling external access. Set one in Settings \u2192 Network \u2192 Password.", http.StatusBadRequest)
					return
				}
			}
		}

		// Snapshot values that need hot-reload comparison after save.
		oldLogLevel := cfg.Logs.LogLevel
		oldNumParallel := cfg.Downloader.NumParallelDownloads
		oldHideAge := cfg.Monitors.HideFinishedAgeDays.Value
		oldGoSoft := cfg.Memory.GoSoftLimitMB
		oldTrust := cfg.Network.TrustForwardedProto
		oldFfmpeg := cfg.Paths.FfmpegPath
		oldReorderPerJob := cfg.Downloader.ReorderBufferMB
		oldReorderBudget := cfg.Downloader.ReorderBudgetMB
		oldPublicURL := cfg.Network.PublicURL

		// Work on a copy so the live config isn't modified if save fails.
		// SaveLocked persists s.cfg, so we need to commit-then-save in a
		// way that lets us roll back on save failure. Save the standalone
		// copy directly via config.Save, then assign back only on success.
		cfgCopy := *cfg
		applyConfigUpdates(&cfgCopy, updates)

		if err := config.Save(&cfgCopy, store.SavePath()); err != nil {
			mu.Unlock()
			jsonError(rw, "failed to save config", http.StatusInternalServerError)
			return
		}

		// Save succeeded — apply to live config
		*cfg = cfgCopy

		// Read new values while still holding the lock
		newLogLevel := cfg.Logs.LogLevel
		newNumParallel := cfg.Downloader.NumParallelDownloads
		newHideAge := cfg.Monitors.HideFinishedAgeDays.Value
		newGoSoft := cfg.Memory.GoSoftLimitMB
		newTrust := cfg.Network.TrustForwardedProto
		newFfmpeg := cfg.Paths.FfmpegPath
		newReorderPerJob := cfg.Downloader.ReorderBufferMB
		newReorderBudget := cfg.Downloader.ReorderBudgetMB
		newPublicURL := cfg.Network.PublicURL
		// A copy, taken under the lock: DownloaderConfig holds only value
		// types, so the callback below can read it after mu.Unlock without
		// racing the next PUT.
		newDownloader := cfg.Downloader

		mu.Unlock()

		// Hot-reload runtime-reloadable settings (outside the lock to avoid deadlocks in callbacks)
		if callbacks != nil {
			if newLogLevel != oldLogLevel && callbacks.OnLogLevelChange != nil {
				callbacks.OnLogLevelChange(newLogLevel)
			}
			if newNumParallel != oldNumParallel && callbacks.OnMaxParallelChange != nil {
				callbacks.OnMaxParallelChange(newNumParallel)
			}
			if newHideAge != oldHideAge && callbacks.OnHideFinishedAgeChanged != nil {
				callbacks.OnHideFinishedAgeChanged()
			}
			if _, hasChannels := updates["channels"]; hasChannels && callbacks.OnChannelChange != nil {
				callbacks.OnChannelChange()
			}
			// public_url lives in [network], not [notifications], but the
			// notification manager is its only consumer — it reads the base
			// URL at send time from the config Reload hands it. The web
			// form's Save sends `network` WITHOUT `notifications` when no
			// webhook is configured, so without this second trigger a
			// public_url change would sit unread until the next restart.
			// (The TUI has no such gap: its save path calls Reload
			// unconditionally — cmd/moombox/tui_wiring.go.)
			_, hasNotifs := updates["notifications"]
			if (hasNotifs || newPublicURL != oldPublicURL) && callbacks.OnNotificationsChange != nil {
				callbacks.OnNotificationsChange()
			}
			if newGoSoft != oldGoSoft && callbacks.OnGoSoftLimitChange != nil {
				callbacks.OnGoSoftLimitChange(newGoSoft)
			}
			if newTrust != oldTrust && callbacks.OnTrustForwardedProtoChange != nil {
				callbacks.OnTrustForwardedProtoChange(newTrust)
			}
			if newFfmpeg != oldFfmpeg && callbacks.OnFfmpegPathChange != nil {
				callbacks.OnFfmpegPathChange(newFfmpeg)
			}
			if (newReorderPerJob != oldReorderPerJob || newReorderBudget != oldReorderBudget) &&
				callbacks.OnReorderBudgetChange != nil {
				callbacks.OnReorderBudgetChange(newDownloader)
			}
		}

		jsonResponse(rw, map[string]any{"success": true})
	})
}
