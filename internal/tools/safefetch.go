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

// SSRF protection. The web-researcher's `web_fetch` tool retrieves
// user/agent-chosen URLs server-side, and the crawl4ai render backend fetches
// URLs on our behalf too — both are SSRF vectors. We defend in two layers:
//
//  1. ValidateURL rejects non-http(s) schemes and literal addresses that resolve
//     to private / loopback / link-local ranges (incl. the 169.254.169.254 cloud
//     metadata endpoint) before any request is made.
//  2. GuardedClient re-checks the *actual resolved IP* at dial time, on the
//     initial connection and on every redirect hop. Checking at dial time (not
//     just parse time) defeats DNS rebinding, where a hostname resolves to a
//     public IP at validation and a private one at connect.
//
// Trusted internal backends (SearXNG, crawl4ai) live on private Docker IPs and
// are reached with a *plain* client, not the guarded one.

const maxRedirects = 10

// ValidateURL parses raw, requires an http(s) scheme and a host, and rejects
// literal IPs in blocked ranges. Returns the parsed URL on success.
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

// blockedIP reports whether ip is in a range we refuse to connect to:
// loopback, private (10/8, 172.16/12, 192.168/16, fc00::/7), link-local
// (169.254/16, fe80::/10 — covers the cloud metadata endpoint), CGNAT
// (100.64/10), unspecified, or multicast.
func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// The cloud metadata endpoint (already link-local, but it is the
		// canonical SSRF target so we name it explicitly).
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		// Carrier-grade NAT (100.64.0.0/10). net.IP.IsPrivate does not cover it,
		// but it can route to internal services in some deployments.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	return false
}

// validateResolvedHost rejects host if it is, or resolves to, a blocked address.
// It backstops the crawl4ai render path: crawl4ai fetches the URL itself with a
// plain (unguarded) client, and ValidateURL only catches *literal* blocked IPs,
// so a hostname resolving to a private / metadata address would otherwise reach
// crawl4ai unchecked. Literal IPs are checked directly; hostnames are resolved
// and every returned address must be allowed.
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

// safeControl is the net.Dialer Control hook: it inspects the concrete IP about
// to be dialed and refuses blocked ranges. This is the DNS-rebinding backstop.
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

// GuardedClient returns an http.Client safe for fetching untrusted URLs: it
// blocks private/loopback/link-local destinations at dial time and re-validates
// every redirect hop.
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
