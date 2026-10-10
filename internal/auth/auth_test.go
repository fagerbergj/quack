package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
}

func TestNewNilConfigDisablesAuth(t *testing.T) {
	a, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if a != nil {
		t.Fatalf("New(nil) = %+v, want nil *Auth (disabled)", a)
	}
}

func TestMiddlewareNilAuthPassesThrough(t *testing.T) {
	var a *Auth
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	a.Middleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("got %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareTrustedHeaders(t *testing.T) {
	a, err := New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(okHandler())

	tests := []struct {
		name     string
		headers  map[string]string
		wantCode int
		wantBody string
	}{
		{
			name:     "trusted header present",
			headers:  map[string]string{"X-authentik-username": "jason"},
			wantCode: http.StatusOK,
			wantBody: "ok",
		},
		{
			name:     "no headers, no oidc configured -> unauthorized",
			headers:  nil,
			wantCode: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestMiddlewareTrustedHeadersTakePriority: a trusted header wins even with OIDC also configured
// and no bearer token present.
func TestMiddlewareTrustedHeadersTakePriority(t *testing.T) {
	idp := newTestIdP(t)
	a, err := New(&config.InboundAuthConfig{
		OIDC:           &config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"},
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	req.Header.Set("X-authentik-username", "jason")
	// Deliberately no Authorization header - if oidc were checked first this
	// would 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("got %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

// TestMiddlewareOIDCBearer: with no trusted header value, requests fall through to bearer verification.
func TestMiddlewareOIDCBearer(t *testing.T) {
	idp := newTestIdP(t)
	a, err := New(&config.InboundAuthConfig{
		OIDC: &config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(okHandler())

	tests := []struct {
		name       string
		authHeader string
		wantCode   int
	}{
		{
			name:       "valid bearer token",
			authHeader: "Bearer " + idp.token(t, "quack", time.Hour, nil),
			wantCode:   http.StatusOK,
		},
		{
			name:       "missing Authorization header",
			authHeader: "",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "wrong scheme",
			authHeader: "Basic dXNlcjpwYXNz",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "expired token",
			authHeader: "Bearer " + idp.token(t, "quack", -time.Hour, nil),
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "wrong audience",
			authHeader: "Bearer " + idp.token(t, "wrong-aud", time.Hour, nil),
			wantCode:   http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

// TestMiddlewareOIDCRejectionBodyIsGeneric: a rejected token's verifier error goes to the log;
// the response body stays generic.
func TestMiddlewareOIDCRejectionBodyIsGeneric(t *testing.T) {
	idp := newTestIdP(t)
	a, err := New(&config.InboundAuthConfig{
		OIDC: &config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(okHandler())

	tests := []struct {
		name       string
		authHeader string
	}{
		{name: "expired token", authHeader: "Bearer " + idp.token(t, "quack", -time.Hour, nil)},
		{name: "wrong audience", authHeader: "Bearer " + idp.token(t, "wrong-aud", time.Hour, nil)},
		{name: "malformed token", authHeader: "Bearer not-a-jwt"},
		{name: "missing Authorization header", authHeader: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			body := strings.TrimSpace(rec.Body.String())
			if body != "unauthorized" {
				t.Errorf("body = %q, want exactly %q (no verifier detail leaked)", body, "unauthorized")
			}
		})
	}
}
