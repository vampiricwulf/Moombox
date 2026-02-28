package utils

import (
	"net"
	"strings"
)

// IsPrivateIP checks if an IP address is in a private/LAN range.
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	privateRanges := []struct {
		network string
	}{
		{"10.0.0.0/8"},
		{"172.16.0.0/12"},
		{"192.168.0.0/16"},
		{"fd00::/8"},  // IPv6 ULA
		{"fe80::/10"}, // IPv6 link-local
	}

	for _, r := range privateRanges {
		_, cidr, err := net.ParseCIDR(r.network)
		if err != nil {
			continue
		}
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}

// IsLoopback checks if an IP address is a loopback address.
func IsLoopback(ipStr string) bool {
	// Handle [::1] format
	ipStr = strings.Trim(ipStr, "[]")

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr == "localhost"
	}
	return ip.IsLoopback()
}
