package utils

import "testing"

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// 10.0.0.0/8 range
		{name: "10.0.0.1 is private", ip: "10.0.0.1", want: true},
		{name: "10.255.255.255 is private", ip: "10.255.255.255", want: true},

		// 172.16.0.0/12 range
		{name: "172.16.0.1 is private", ip: "172.16.0.1", want: true},
		{name: "172.31.255.255 is private", ip: "172.31.255.255", want: true},
		{name: "172.15.0.1 is NOT private", ip: "172.15.0.1", want: false},
		{name: "172.32.0.1 is NOT private", ip: "172.32.0.1", want: false},

		// 192.168.0.0/16 range
		{name: "192.168.0.1 is private", ip: "192.168.0.1", want: true},
		{name: "192.168.255.255 is private", ip: "192.168.255.255", want: true},

		// IPv6 ULA (fd00::/8)
		{name: "fd00::1 is private", ip: "fd00::1", want: true},
		{name: "fdff::1 is private", ip: "fdff::1", want: true},

		// IPv6 link-local (fe80::/10)
		{name: "fe80::1 is private", ip: "fe80::1", want: true},

		// Public IPs
		{name: "8.8.8.8 is public", ip: "8.8.8.8", want: false},
		{name: "1.1.1.1 is public", ip: "1.1.1.1", want: false},
		{name: "203.0.113.1 is public", ip: "203.0.113.1", want: false},
		{name: "2001:db8::1 is public", ip: "2001:db8::1", want: false},

		// Loopback (not in private ranges list)
		{name: "127.0.0.1 is not private", ip: "127.0.0.1", want: false},
		{name: "::1 is not private", ip: "::1", want: false},

		// Invalid / empty
		{name: "empty string", ip: "", want: false},
		{name: "invalid IP", ip: "not-an-ip", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPrivateIP(tt.ip)
			if got != tt.want {
				t.Errorf("IsPrivateIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// IPv4 loopback
		{name: "127.0.0.1 is loopback", ip: "127.0.0.1", want: true},
		{name: "127.0.0.2 is loopback", ip: "127.0.0.2", want: true},
		{name: "127.255.255.255 is loopback", ip: "127.255.255.255", want: true},

		// IPv6 loopback
		{name: "::1 is loopback", ip: "::1", want: true},
		{name: "[::1] bracket format is loopback", ip: "[::1]", want: true},

		// localhost keyword
		{name: "localhost is loopback", ip: "localhost", want: true},

		// Not loopback
		{name: "10.0.0.1 is not loopback", ip: "10.0.0.1", want: false},
		{name: "192.168.1.1 is not loopback", ip: "192.168.1.1", want: false},
		{name: "8.8.8.8 is not loopback", ip: "8.8.8.8", want: false},
		{name: "::2 is not loopback", ip: "::2", want: false},

		// Invalid / empty
		{name: "empty string is not loopback", ip: "", want: false},
		{name: "invalid string is not loopback", ip: "xyz", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLoopback(tt.ip)
			if got != tt.want {
				t.Errorf("IsLoopback(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}
