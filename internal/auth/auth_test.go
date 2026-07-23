package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/config"
)

func echoIdentityHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("no-identity"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(id.User))
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
	a.Middleware(echoIdentityHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "no-identity" {
		t.Errorf("got %d %q, want 200 no-identity", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareTrustedHeaders(t *testing.T) {
	a, err := New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username", Groups: "X-authentik-groups"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(echoIdentityHandler())

	tests := []struct {
		name     string
		headers  map[string]string
		wantCode int
		wantBody string
	}{
		{
			name:     "trusted header present",
			headers:  map[string]string{"X-authentik-username": "jason", "X-authentik-groups": "admins,devs"},
			wantCode: http.StatusOK,
			wantBody: "jason",
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

// TestMiddlewareTrustedHeadersTakePriority pins the documented precedence: a
// trusted header wins even when oidc is ALSO configured and the request
// carries no bearer token that would otherwise be required.
func TestMiddlewareTrustedHeadersTakePriority(t *testing.T) {
	idp := newTestIdP(t)
	a, err := New(&config.InboundAuthConfig{
		OIDC:           &config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"},
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(echoIdentityHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	req.Header.Set("X-authentik-username", "jason")
	// Deliberately no Authorization header - if oidc were checked first this
	// would 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "jason" {
		t.Errorf("got %d %q, want 200 jason", rec.Code, rec.Body.String())
	}
}

// TestMiddlewareOIDCFallsBackWhenNoTrustedHeader covers a request that has
// neither a trusted header value nor bearer token satisfied by headers -
// falls through to bearer verification.
func TestMiddlewareOIDCBearer(t *testing.T) {
	idp := newTestIdP(t)
	a, err := New(&config.InboundAuthConfig{
		OIDC: &config.OIDCConfig{Issuer: idp.srv.URL, Audience: "quack"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := a.Middleware(echoIdentityHandler())

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

func TestSplitGroups(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "single", raw: "admins", want: []string{"admins"}},
		{name: "multiple with spaces", raw: "admins, devs , ops", want: []string{"admins", "devs", "ops"}},
		{name: "empty", raw: "", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitGroups(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("splitGroups(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitGroups(%q)[%d] = %q, want %q", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}
