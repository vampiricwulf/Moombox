package utils

import (
	"net"
	"strings"
)

// privateCIDRs is parsed once at init to avoid re-parsing on every call.
var privateCIDRs []*net.IPNet

func init() {
	for _, s := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fd00::/8",  // IPv6 ULA
		"fe80::/10", // IPv6 link-local
	} {
		_, cidr, _ := net.ParseCIDR(s)
		privateCIDRs = append(privateCIDRs, cidr)
	}
}

// IsPrivateIP checks if an IP address is in a private/LAN range.
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, cidr := range privateCIDRs {
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
