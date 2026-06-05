package tools

import (
	"context"
	"net"
	"testing"
)

func TestValidateURL(t *testing.T) {
	allowed := []string{
		"http://example.com",
		"https://example.com/path?q=1",
		"http://8.8.8.8/page",
		"https://93.184.216.34",
	}
	for _, raw := range allowed {
		if _, err := ValidateURL(raw); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want allowed", raw, err)
		}
	}

	blocked := []string{
		"file:///etc/passwd",
		"ftp://example.com",
		"gopher://example.com",
		"javascript:alert(1)",
		"http://",                 // no host
		"http://127.0.0.1",        // loopback
		"http://127.0.0.1:8080/x", // loopback w/ port
		"http://10.0.0.5",         // private
		"http://172.16.4.4",       // private
		"http://192.168.1.1",      // private
		"http://169.254.169.254/", // cloud metadata
		"http://169.254.1.1",      // link-local
		"http://0.0.0.0",          // unspecified
		"http://[::1]/",           // ipv6 loopback
		"http://[fe80::1]",        // ipv6 link-local
		"http://[fc00::1]",        // ipv6 unique-local (private)
	}
	for _, raw := range blocked {
		if _, err := ValidateURL(raw); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want blocked", raw)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         false,
		"93.184.216.34":   false,
		"1.1.1.1":         false,
		"127.0.0.1":       true,
		"10.255.0.1":      true,
		"172.16.0.1":      true,
		"172.31.255.255":  true,
		"192.168.0.1":     true,
		"169.254.169.254": true,
		"169.254.0.1":     true,
		"100.64.0.1":      true,  // CGNAT
		"100.127.255.255": true,  // CGNAT (top of range)
		"100.63.255.255":  false, // just below CGNAT
		"100.128.0.1":     false, // just above CGNAT
		"0.0.0.0":         true,
		"224.0.0.1":       true, // multicast
		"::1":             true,
		"fe80::1":         true,
		"fc00::1":         true,
		"2606:4700::1111": false, // public ipv6
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if got := blockedIP(ip); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

// TestSafeControl exercises the dial-time guard directly — this is what defeats
// DNS rebinding, since it sees the resolved IP rather than the hostname.
func TestSafeControl(t *testing.T) {
	if err := safeControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("safeControl(public) = %v, want nil", err)
	}
	blocked := []string{"127.0.0.1:80", "169.254.169.254:80", "10.0.0.1:443", "[::1]:80", "[fe80::1]:80"}
	for _, addr := range blocked {
		if err := safeControl("tcp", addr, nil); err == nil {
			t.Errorf("safeControl(%q) = nil, want blocked", addr)
		}
	}
}

// TestValidateResolvedHost guards the crawl4ai render path: a hostname that
// resolves to a blocked address must be rejected before crawl4ai fetches it.
func TestValidateResolvedHost(t *testing.T) {
	ctx := context.Background()

	// Literal public IPs are allowed; blocked literals and a hostname that
	// resolves to loopback (localhost, via /etc/hosts — no network needed) are not.
	if err := validateResolvedHost(ctx, "8.8.8.8"); err != nil {
		t.Errorf("validateResolvedHost(8.8.8.8) = %v, want nil", err)
	}
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "100.64.0.1", "localhost"} {
		if err := validateResolvedHost(ctx, host); err == nil {
			t.Errorf("validateResolvedHost(%q) = nil, want blocked", host)
		}
	}
}
