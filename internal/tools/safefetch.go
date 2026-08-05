package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// SSRF protection: ValidateURL rejects blocked schemes/addresses; GuardedClient re-checks at dial time (defeats DNS rebinding).

const maxRedirects = 10

// ValidateURL: parses and validates URL, rejects non-http(s) and blocked literal IPs.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("safefetch: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("safefetch: scheme %q not allowed (only http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("safefetch: url has no host")
	}
	if ip := net.ParseIP(host); ip != nil && blockedIP(ip) {
		return nil, fmt.Errorf("safefetch: address %s is in a blocked range", ip)
	}
	return u, nil
}

// blockedIP: reports whether ip is in a blocked range (loopback, private, link-local, CGNAT, etc).
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Cloud metadata endpoint.
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		// Carrier-grade NAT, not covered by IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	return false
}

// validateResolvedHost: backstops crawl4ai render path - checks resolved IPs for blocked ranges.
func validateResolvedHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("safefetch: address %s is in a blocked range", ip)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("safefetch: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return fmt.Errorf("safefetch: host %q resolves to blocked address %s", host, ip)
		}
	}
	return nil
}

// safeControl: net.Dialer Control hook - DNS-rebinding backstop.
func safeControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("safefetch: split dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("safefetch: undialable address %q", host)
	}
	if blockedIP(ip) {
		return fmt.Errorf("safefetch: refusing to connect to blocked address %s", ip)
	}
	return nil
}

// GuardedClient: http.Client that blocks private addresses at dial time and re-validates redirects.
func GuardedClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: safeControl}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("safefetch: stopped after %d redirects", maxRedirects)
			}
			if _, err := ValidateURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}
