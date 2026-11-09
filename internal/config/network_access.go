package config

// The network.network_access values. NetworkAccess stays a plain string in
// the config (its TOML and JSON bytes are these values).
const (
	NetworkAccessLocalhost = "localhost"
	NetworkAccessLAN       = "lan"
	NetworkAccessExternal  = "external"
	// NetworkAccessPublic is external's hand-edit-only synonym, for a
	// deployment behind an authenticating reverse proxy: config.toml accepts
	// it, no settings surface offers or sets it (SettableNetworkAccess).
	NetworkAccessPublic = "public"
)

// SettableNetworkAccess is the values the settings surfaces offer and the
// config API accepts — every value but the hand-edit-only "public".
var SettableNetworkAccess = []string{NetworkAccessLocalhost, NetworkAccessLAN, NetworkAccessExternal}

// IsExternalAccess reports whether a network_access value exposes the
// dashboard beyond the local network: "external", or its synonym "public".
// The one alias check every runtime consumer makes — the IP gate, the origin
// and Host checks, auth's requirement, the boot warnings, the TUI's security
// banner and the passwordless-external guards — so none re-spells the pair
// (a guard that checked "external" alone once missed the alias).
func IsExternalAccess(access string) bool {
	return access == NetworkAccessExternal || access == NetworkAccessPublic
}

// ListensBeyondLoopback reports whether a network_access value binds the
// dashboard to every interface rather than loopback alone: lan, or either
// external value.
func ListensBeyondLoopback(access string) bool {
	return access == NetworkAccessLAN || IsExternalAccess(access)
}
