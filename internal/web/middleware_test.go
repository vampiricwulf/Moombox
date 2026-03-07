package web

import (
	"net/http"
	"testing"
)

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{
			name:       "IPv4 with port",
			remoteAddr: "192.168.1.1:12345",
			expected:   "192.168.1.1",
		},
		{
			name:       "IPv4 without port",
			remoteAddr: "192.168.1.1",
			expected:   "192.168.1.1",
		},
		{
			name:       "IPv6 loopback with port",
			remoteAddr: "[::1]:8080",
			expected:   "::1",
		},
		{
			name:       "IPv6 full with port",
			remoteAddr: "[2001:db8::1]:443",
			expected:   "2001:db8::1",
		},
		{
			name:       "localhost with port",
			remoteAddr: "127.0.0.1:9090",
			expected:   "127.0.0.1",
		},
		{
			name:       "does not trust X-Forwarded-For",
			remoteAddr: "10.0.0.1:1234",
			expected:   "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remoteAddr,
				Header:     http.Header{},
			}
			// Set X-Forwarded-For to prove it's ignored
			r.Header.Set("X-Forwarded-For", "99.99.99.99")
			r.Header.Set("X-Real-IP", "88.88.88.88")

			result := ExtractIP(r)
			if result != tt.expected {
				t.Errorf("ExtractIP(RemoteAddr=%q) = %q, expected %q", tt.remoteAddr, result, tt.expected)
			}
		})
	}
}

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		name          string
		origin        string
		networkAccess string
		expected      bool
	}{
		{
			name:          "local mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "local",
			expected:      true,
		},
		{
			name:          "local mode allows 127.0.0.1",
			origin:        "http://127.0.0.1:8080",
			networkAccess: "local",
			expected:      true,
		},
		{
			name:          "local mode rejects LAN IP",
			origin:        "http://192.168.1.100:3000",
			networkAccess: "local",
			expected:      false,
		},
		{
			name:          "local mode rejects external",
			origin:        "http://example.com",
			networkAccess: "local",
			expected:      false,
		},
		{
			name:          "lan mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows 127.0.0.1",
			origin:        "http://127.0.0.1:3000",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 192.168",
			origin:        "http://192.168.1.50:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 10.x",
			origin:        "http://10.0.0.5:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode allows private IP 172.16",
			origin:        "http://172.16.0.1:8080",
			networkAccess: "lan",
			expected:      true,
		},
		{
			name:          "lan mode rejects external",
			origin:        "http://example.com",
			networkAccess: "lan",
			expected:      false,
		},
		{
			name:          "public mode allows everything",
			origin:        "http://example.com",
			networkAccess: "public",
			expected:      true,
		},
		{
			name:          "public mode allows external HTTPS",
			origin:        "https://evil.example.org",
			networkAccess: "public",
			expected:      true,
		},
		{
			name:          "default (empty) mode allows localhost",
			origin:        "http://localhost:3000",
			networkAccess: "",
			expected:      true,
		},
		{
			name:          "default (empty) mode rejects external",
			origin:        "http://example.com",
			networkAccess: "",
			expected:      false,
		},
		{
			name:          "invalid origin URL",
			origin:        "://bad",
			networkAccess: "public",
			expected:      false,
		},
		{
			name:          "empty origin",
			origin:        "",
			networkAccess: "public",
			expected:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllowedOrigin(tt.origin, tt.networkAccess)
			if result != tt.expected {
				t.Errorf("isAllowedOrigin(%q, %q) = %v, expected %v",
					tt.origin, tt.networkAccess, result, tt.expected)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		{name: "127.0.0.1", ip: "127.0.0.1", expected: true},
		{name: "127.0.0.2", ip: "127.0.0.2", expected: true},
		{name: "::1", ip: "::1", expected: true},
		{name: "localhost string", ip: "localhost", expected: true},
		{name: "192.168.1.1 not loopback", ip: "192.168.1.1", expected: false},
		{name: "10.0.0.1 not loopback", ip: "10.0.0.1", expected: false},
		{name: "8.8.8.8 not loopback", ip: "8.8.8.8", expected: false},
		{name: "empty string", ip: "", expected: false},
		{name: "garbage", ip: "not-an-ip", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isLoopback(tt.ip)
			if result != tt.expected {
				t.Errorf("isLoopback(%q) = %v, expected %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		{name: "10.0.0.1 (class A)", ip: "10.0.0.1", expected: true},
		{name: "10.255.255.255 (class A end)", ip: "10.255.255.255", expected: true},
		{name: "172.16.0.1 (class B start)", ip: "172.16.0.1", expected: true},
		{name: "172.31.255.255 (class B end)", ip: "172.31.255.255", expected: true},
		{name: "172.32.0.1 (outside class B)", ip: "172.32.0.1", expected: false},
		{name: "192.168.0.1 (class C)", ip: "192.168.0.1", expected: true},
		{name: "192.168.255.255 (class C end)", ip: "192.168.255.255", expected: true},
		{name: "127.0.0.1 (loopback counts as private)", ip: "127.0.0.1", expected: true},
		{name: "::1 (IPv6 loopback counts as private)", ip: "::1", expected: true},
		{name: "8.8.8.8 (public)", ip: "8.8.8.8", expected: false},
		{name: "1.1.1.1 (public)", ip: "1.1.1.1", expected: false},
		{name: "fc00::1 (IPv6 private)", ip: "fc00::1", expected: true},
		{name: "fd00::1 (IPv6 private)", ip: "fd00::1", expected: true},
		{name: "2001:db8::1 (IPv6 public)", ip: "2001:db8::1", expected: false},
		{name: "fe80::1 (IPv6 link-local)", ip: "fe80::1", expected: true},
		{name: "fe80::a1b2:c3d4 (IPv6 link-local)", ip: "fe80::a1b2:c3d4", expected: true},
		{name: "169.254.1.1 (IPv4 link-local)", ip: "169.254.1.1", expected: true},
		{name: "169.254.255.255 (IPv4 link-local end)", ip: "169.254.255.255", expected: true},
		{name: "invalid IP", ip: "not-an-ip", expected: false},
		{name: "empty string", ip: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPrivateIP(tt.ip)
			if result != tt.expected {
				t.Errorf("isPrivateIP(%q) = %v, expected %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestShouldSkipCompression(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{
			name:     "video endpoint",
			path:     "/api/jobs/abc123/video",
			expected: true,
		},
		{
			name:     "video endpoint with different job ID",
			path:     "/api/jobs/xyz-789/video",
			expected: true,
		},
		{
			name:     "jobs list endpoint",
			path:     "/api/jobs",
			expected: false,
		},
		{
			name:     "job detail (not video)",
			path:     "/api/jobs/abc123",
			expected: false,
		},
		{
			name:     "status endpoint",
			path:     "/api/status",
			expected: false,
		},
		{
			name:     "root path",
			path:     "/",
			expected: false,
		},
		{
			name:     "video suffix but wrong prefix",
			path:     "/other/jobs/abc123/video",
			expected: false,
		},
		{
			name:     "jobs prefix but no video suffix",
			path:     "/api/jobs/abc123/logs",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldSkipCompression(tt.path)
			if result != tt.expected {
				t.Errorf("shouldSkipCompression(%q) = %v, expected %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestIsLoopbackRequest(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   bool
	}{
		{
			name:       "loopback IPv4",
			remoteAddr: "127.0.0.1:8080",
			expected:   true,
		},
		{
			name:       "loopback IPv6",
			remoteAddr: "[::1]:8080",
			expected:   true,
		},
		{
			name:       "LAN IP",
			remoteAddr: "192.168.1.1:8080",
			expected:   false,
		},
		{
			name:       "public IP",
			remoteAddr: "8.8.8.8:1234",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remoteAddr,
				Header:     http.Header{},
			}
			result := IsLoopbackRequest(r)
			if result != tt.expected {
				t.Errorf("IsLoopbackRequest(RemoteAddr=%q) = %v, expected %v",
					tt.remoteAddr, result, tt.expected)
			}
		})
	}
}
