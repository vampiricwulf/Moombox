package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// network_access has a documented synonym pair: "external" and "public" (the
// latter marks a deployment behind an authenticating reverse proxy). Every
// runtime consumer treats them identically, so every guard that exists to keep
// the dashboard out of the passwordless-external state must cover both.
//
// Before this branch, config validation silently normalised a hand-edited
// "public" back to "localhost" at load, which made "external"-only guards
// unreachable. "public" is now a valid, persistable value — these tests lock
// the widened guards so the alias can't slip past them again.

// newSecuritySettingsModel wires a SettingsModel against a live config the way
// App.SetConfigStore does: m.cfg and the Store share one struct.
func newSecuritySettingsModel(t *testing.T, access, hash string) (*SettingsModel, *config.MoomboxConfig) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Network.NetworkAccess = access
	cfg.Network.PasswordHash = hash
	m := NewSettingsModel()
	m.configStore = config.NewStore(cfg, "")
	m.Open(cfg)
	return m, cfg
}

// TestRemovePasswordResetsExternalAndPublic: removing the dashboard password
// is a CREATE path for the passwordless-external state. It must drop
// network_access back to "localhost" for both spellings of external access,
// and must leave the safe modes alone.
func TestRemovePasswordResetsExternalAndPublic(t *testing.T) {
	tests := []struct {
		name      string
		access    string
		wantReset bool
	}{
		{"external resets", "external", true},
		{"public resets", "public", true},
		{"lan untouched", "lan", false},
		{"localhost untouched", "localhost", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cfg := newSecuritySettingsModel(t, tt.access, "scrypt:salt:hash")
			m.OnVerifyPassword = func(password, hash string) bool { return true }
			saved := 0
			m.OnSave = func(*config.MoomboxConfig) { saved++ }
			m.secRemovePw = "correct-horse"

			m.handleRemovePassword()

			if cfg.Network.PasswordHash != "" {
				t.Fatalf("PasswordHash = %q, want cleared", cfg.Network.PasswordHash)
			}
			if saved != 1 {
				t.Fatalf("OnSave called %d times, want 1", saved)
			}

			wantAccess := tt.access
			if tt.wantReset {
				wantAccess = "localhost"
			}
			if cfg.Network.NetworkAccess != wantAccess {
				t.Errorf("NetworkAccess = %q, want %q", cfg.Network.NetworkAccess, wantAccess)
			}
			// The settings panel's own field map must follow the config, or
			// the next save writes the pre-reset value straight back.
			if m.values["network_access"] != wantAccess {
				t.Errorf("values[network_access] = %q, want %q", m.values["network_access"], wantAccess)
			}
			if tt.wantReset && !strings.Contains(m.secMessage, "reset to localhost") {
				t.Errorf("secMessage = %q, want it to mention the reset", m.secMessage)
			}
		})
	}
}

// TestApplyValuesRequiresPasswordForExternalAndPublic: the settings-save guard
// refuses to persist passwordless external access. "public" reaches
// m.values via loadValues (a hand-edited config file), so the guard sees it.
func TestApplyValuesRequiresPasswordForExternalAndPublic(t *testing.T) {
	tests := []struct {
		name      string
		access    string
		hash      string
		wantError bool
	}{
		{"external without password refused", "external", "", true},
		{"public without password refused", "public", "", true},
		{"external with password allowed", "external", "scrypt:salt:hash", false},
		{"public with password allowed", "public", "scrypt:salt:hash", false},
		{"lan without password allowed", "lan", "", false},
		{"localhost without password allowed", "localhost", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cfg := newSecuritySettingsModel(t, tt.access, tt.hash)
			if got := m.values["network_access"]; got != tt.access {
				t.Fatalf("loadValues dropped the access mode: values[network_access] = %q, want %q", got, tt.access)
			}

			// Clobber cfg (NOT m.values) with a different mode, so the
			// post-save assertion below proves applyValues actually wrote the
			// panel's value through rather than passing because cfg happened
			// to already hold it.
			sentinel := "lan"
			if tt.access == "lan" {
				sentinel = "localhost"
			}
			cfg.Network.NetworkAccess = sentinel

			m.applyValues()

			if tt.wantError {
				if m.status != saveError {
					t.Fatalf("status = %v, want saveError for passwordless %q", m.status, tt.access)
				}
				if !strings.Contains(m.errorMsg, "Password required") {
					t.Errorf("errorMsg = %q, want a password-required message", m.errorMsg)
				}
				return
			}
			if m.status == saveError {
				t.Fatalf("save refused for %q/%q: %s", tt.access, tt.hash, m.errorMsg)
			}
			// The allowed path must write the mode back UNMANGLED. This is the
			// other half of the guard's reachability argument: loadValues copies
			// network_access verbatim into m.values and applyValues writes it
			// straight back to cfg, so a hand-edited "public" round-trips
			// through the settings panel — which is exactly why the guard above
			// has to recognise it.
			if cfg.Network.NetworkAccess != tt.access {
				t.Errorf("NetworkAccess = %q after applyValues, want %q preserved", cfg.Network.NetworkAccess, tt.access)
			}
		})
	}
}

// TestSecurityCommitNotifiesApp: recalcLayout subtracts the security banner's
// rendered height from the panel area, so a commit that can flip the banner
// must tell the App to re-derive the layout — the same contract
// OnRestartRequired has. Setting a password on a passwordless external config
// is the reachable non-empty → empty transition; without the callback View()
// drops the banner while contentH is still short by its height, leaving a dead
// row until the next resize.
func TestSecurityCommitNotifiesApp(t *testing.T) {
	t.Run("set password notifies", func(t *testing.T) {
		m, cfg := newSecuritySettingsModel(t, "external", "")
		m.OnHashPassword = func(string) string { return "scrypt:salt:hash" }
		notified := 0
		m.OnSecurityChanged = func() { notified++ }
		m.secNewPw = "correct-horse"
		m.secConfirmPw = "correct-horse"

		m.handleSetPassword()

		if cfg.Network.PasswordHash == "" {
			t.Fatalf("password was not committed: %s", m.secMessage)
		}
		if notified != 1 {
			t.Errorf("OnSecurityChanged called %d times, want 1", notified)
		}
	})

	t.Run("rejected set does not notify", func(t *testing.T) {
		m, _ := newSecuritySettingsModel(t, "external", "")
		m.OnHashPassword = func(string) string { return "scrypt:salt:hash" }
		notified := 0
		m.OnSecurityChanged = func() { notified++ }
		m.secNewPw = "correct-horse"
		m.secConfirmPw = "typo"

		m.handleSetPassword()

		if notified != 0 {
			t.Errorf("OnSecurityChanged called %d times for a rejected commit, want 0", notified)
		}
	})

	t.Run("remove password notifies", func(t *testing.T) {
		m, _ := newSecuritySettingsModel(t, "external", "scrypt:salt:hash")
		m.OnVerifyPassword = func(string, string) bool { return true }
		notified := 0
		m.OnSecurityChanged = func() { notified++ }
		m.secRemovePw = "correct-horse"

		m.handleRemovePassword()

		if notified != 1 {
			t.Errorf("OnSecurityChanged called %d times, want 1", notified)
		}
	})
}

// TestRenderSecurityRemoveWarnsForExternalAndPublic: the remove-password
// confirmation warns that network access will be reset. The warning must
// appear exactly when handleRemovePassword actually resets, or the panel
// silently changes the deployment mode out from under the user.
func TestRenderSecurityRemoveWarnsForExternalAndPublic(t *testing.T) {
	tests := []struct {
		access   string
		wantWarn bool
	}{
		{"external", true},
		{"public", true},
		{"lan", false},
		{"localhost", false},
	}
	for _, tt := range tests {
		t.Run(tt.access, func(t *testing.T) {
			m, _ := newSecuritySettingsModel(t, tt.access, "scrypt:salt:hash")
			out := m.renderSecurityRemove(60)
			if got := strings.Contains(out, "reset to localhost"); got != tt.wantWarn {
				t.Errorf("renderSecurityRemove warning present = %v, want %v\n%s", got, tt.wantWarn, out)
			}
		})
	}
}
