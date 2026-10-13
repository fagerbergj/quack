package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
)

// inverseExt is testExt plus every interface quack calls into, a public route, and a page that uses each
// Host callback github does.
type inverseExt struct{ testExt }

func (inverseExt) Deliver(_ context.Context, dc sdk.DeliveryContext) ([]sdk.DeliveryItemOutcome, error) {
	out := make([]sdk.DeliveryItemOutcome, len(dc.Items))
	for i, it := range dc.Items {
		out[i] = sdk.DeliveryItemOutcome{Kind: string(it.Kind), URL: "u/" + it.Title}
	}
	if dc.PushError != "" {
		return out, errors.New("push failed: " + dc.PushError)
	}
	return out, nil
}

func (inverseExt) GitCredential(_ context.Context, rawURL string) (*sdk.GitCredential, error) {
	switch rawURL {
	case "https://github.com/a/b":
		return &sdk.GitCredential{Host: "github.com", Username: "x-access-token", Token: "ghs_secret"}, nil
	case "bad":
		return nil, errors.New("mint failed")
	}
	return nil, nil
}

func (inverseExt) OnAssignment(ctx context.Context, a sdk.Assignment) map[string]any {
	s, ok := ctx.(interface{ SessionID() string })
	if !ok {
		return nil
	}
	return map[string]any{"session": s.SessionID(), "node": a.NodeID, "base": a.Meta["x"]["base"]}
}

func (inverseExt) BeforeAssignment(_ context.Context, a sdk.Assignment) (bool, string) {
	return a.NodeID == "fresh", "node " + a.NodeID
}

func (e inverseExt) RegisterRoutes(authed, public chi.Router) {
	e.testExt.RegisterRoutes(authed, public)
	public.Post("/hook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "hook %s sig=%s", body, r.Header.Get("X-Sig"))
	})
	authed.Get("/host", func(w http.ResponseWriter, r *http.Request) {
		rev, changed, werr := e.host.WriteArtifact("ext:x:1", "u", "a", "text/plain", []byte("A"))
		user, ok := e.host.ChatUser("ext:x:1")
		_, missing := e.host.ChatUser("ext:x:gone")
		aerr, ierr := e.host.ArchiveChat("ext:x:1"), e.host.InvalidateSetup("ext:x:1")
		oerr := e.host.UpdateChatOrigin("gone", sdk.ChatOrigin{Label: "L"})
		label, cerr := e.host.Classify(r.Context(), "p")
		_, _ = fmt.Fprintf(w, "write=%d,%t,%v user=%s,%t,%t archive=%v invalidate=%v unknown=%t classify=%s,%v",
			rev, changed, werr, user, ok, missing, aerr, ierr, errors.Is(oerr, sdk.ErrUnknownChat), label, cerr)
	})
}

// githubHost answers each Host callback github uses.
func githubHost() sdk.Host {
	return sdk.Host{
		WriteArtifact: func(chatID, user, name, mime string, data []byte) (int64, bool, error) {
			if strings.Join([]string{chatID, user, name, mime, string(data)}, " ") != "ext:x:1 u a text/plain A" {
				return 0, false, errors.New("unexpected write")
			}
			return 3, true, nil
		},
		ChatUser:         func(chatID string) (string, bool) { return "alice", chatID == "ext:x:1" },
		ArchiveChat:      func(string) error { return nil },
		InvalidateSetup:  func(chatID string) error { return fmt.Errorf("setup of %s is busy", chatID) },
		UpdateChatOrigin: func(localID string, _ sdk.ChatOrigin) error { return fmt.Errorf("%s: %w", localID, sdk.ErrUnknownChat) },
		Classify:         func(_ context.Context, prompt string) (string, error) { return "yes:" + prompt, nil },
		Decide:           func(context.Context, sdk.DecideRequest) (sdk.Decision, error) { return sdk.Decision{}, nil },
	}
}

// startInverse mounts the child as quack does: authed routes behind an auth check, public routes beside them.
func startInverse(t *testing.T) (*Process, *httptest.Server) {
	t.Helper()
	p, _, err := newProcessHost("inverse", githubHost())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	combined := chi.NewRouter()
	authed := combined.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "ok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	p.RegisterRoutes(authed, combined)
	root := chi.NewRouter()
	root.Mount("/x", combined)
	srv := httptest.NewServer(root)
	t.Cleanup(func() {
		_ = p.Close()
		srv.Close()
	})
	return p, srv
}

func do(t *testing.T, method, url, auth string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader("payload"))
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-Sig", "sha256=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A public route answers without quack's auth, with its own headers intact, while every other path keeps it.
func TestPublicRoutesSkipAuthOthersKeepIt(t *testing.T) {
	_, srv := startInverse(t)
	if code, body := do(t, http.MethodPost, srv.URL+"/x/hook", ""); code != http.StatusOK || body != "hook payload sig=sha256=abc" {
		t.Errorf("public POST /x/hook = %d %q", code, body)
	}
	if code, _ := do(t, http.MethodGet, srv.URL+"/x/pid", ""); code != http.StatusUnauthorized {
		t.Errorf("authed GET /x/pid without auth = %d, want 401", code)
	}
	if code, _ := do(t, http.MethodGet, srv.URL+"/x/pid", "ok"); code != http.StatusOK {
		t.Errorf("authed GET /x/pid = %d, want 200", code)
	}
}

func TestHostCallbacksForGithubCrossTheWire(t *testing.T) {
	_, srv := startInverse(t)
	_, body := do(t, http.MethodGet, srv.URL+"/x/host", "ok")
	if want := "write=3,true,<nil> user=alice,true,false archive=<nil> invalidate=setup of ext:x:1 is busy unknown=true classify=yes:p,<nil>"; body != want {
		t.Errorf("callbacks from the child = %q\nwant %q", body, want)
	}
}

type sessionKeyed struct{ context.Context }

func (sessionKeyed) SessionID() string { return "ext:x:42" }

func TestInverseInterfacesCrossTheWire(t *testing.T) {
	p, _ := startInverse(t)
	ctx := context.Background()
	for _, iface := range []string{"Deliverer", "GitCredentialSource", "AssignmentMetaExtension", "AssignmentFreshnessChecker"} {
		if !p.Implements(iface) {
			t.Errorf("Implements(%s) = false", iface)
		}
	}
	dc := sdk.DeliveryContext{ChatID: "ext:x:1", Items: []sdk.StagedDelivery{{Kind: sdk.KindReview, Title: "t",
		Comments: []sdk.ReviewComment{{Path: "a.go", Line: 3, Body: "nit", Severity: "nit"}}}}}
	for _, pushErr := range []string{"", "rejected"} {
		dc.PushError = pushErr
		want, wantErr := inverseExt{}.Deliver(ctx, dc)
		got, err := p.Deliver(ctx, dc)
		if !reflect.DeepEqual(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("Deliver(push %q) = %+v, %v; in process %+v, %v", pushErr, got, err, want, wantErr)
		}
	}
	for _, u := range []string{"https://github.com/a/b", "https://gitlab.com/a/b", "bad"} {
		want, wantErr := inverseExt{}.GitCredential(ctx, u)
		got, err := p.GitCredential(ctx, u)
		if !reflect.DeepEqual(got, want) || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("GitCredential(%s) = %+v, %v; want %+v, %v", u, got, err, want, wantErr)
		}
	}
	a := sdk.Assignment{NodeID: "n1", Meta: map[string]map[string]any{"x": {"base": "abc"}}}
	if got := p.OnAssignment(sessionKeyed{ctx}, a); !reflect.DeepEqual(got, map[string]any{"session": "ext:x:42", "node": "n1", "base": "abc"}) {
		t.Errorf("OnAssignment = %v, want the session id the agent ctx carried", got)
	}
	if got := p.OnAssignment(ctx, a); got != nil {
		t.Errorf("OnAssignment without a session = %v, want nil as in-process", got)
	}
	for _, node := range []string{"fresh", "stale"} {
		if fresh, reason := p.BeforeAssignment(ctx, sdk.Assignment{NodeID: node}); fresh != (node == "fresh") || reason != "node "+node {
			t.Errorf("BeforeAssignment(%s) = %t %q", node, fresh, reason)
		}
	}

	_ = p.Close()
	if fresh, reason := p.BeforeAssignment(ctx, a); fresh || !strings.Contains(reason, "unavailable") {
		t.Errorf("BeforeAssignment with the child gone = %t %q, want stale", fresh, reason)
	}
	if meta := p.OnAssignment(ctx, a); meta != nil {
		t.Errorf("OnAssignment with the child gone = %v, want nil", meta)
	}
	if _, err := p.Deliver(ctx, dc); !errors.Is(err, errClosed) {
		t.Errorf("Deliver with the child gone = %v", err)
	}
	if c, err := p.GitCredential(ctx, "https://github.com/a/b"); c != nil || !errors.Is(err, errClosed) {
		t.Errorf("GitCredential with the child gone = %v, %v", c, err)
	}
}

// Each refusal names what this quack lacks instead of starting a child that would run without it.
func TestChildRefusesWhatTheHostCannotCarry(t *testing.T) {
	all := serves(hostCalls(githubHost()))
	without := func(drop string) []string {
		return slices.DeleteFunc(slices.Clone(all), func(s string) bool { return s == drop })
	}
	for _, tc := range []struct {
		name, config string
		needs        []Callback
		serves       []string
		want         string
	}{
		{"a needed callback", "ok", []Callback{Classify}, without(methodClassify), "does not serve the classify callback"},
		{"a Deliverer", "inverse", nil, without(methodDeliver), "does not call Deliverer"},
		{"public routes", "inverse", nil, without(methodPublicHTTP), "serves no public routes"},
		{"everything served", "inverse", []Callback{Classify, WriteArtifact}, all, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{ctx: context.Background(), factory: testFactory, log: slog.New(slog.DiscardHandler), needs: tc.needs}
			raw, _ := json.Marshal(initParams{Protocol: ProtocolVersion, Name: "x", Config: tc.config, Serves: tc.serves})
			if _, err := s.initialize(raw); (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("initialize = %v, want %q", err, tc.want)
			}
		})
	}
}
