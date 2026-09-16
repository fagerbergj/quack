package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/langfuse"
)

// promptChat builds one recorded llm.call entry stamped with prompt
// provenance (#1420, #1422): artifact is the resolved artifact name
// (quack.prompt.artifact), agent the (possibly different) agent name.
func promptChat(ts time.Time, round, agent, artifact, promptSource, promptVersionID string) entry {
	return chat(ts, "node-a", agent, round, "gpt", map[string]any{
		"gen_ai.prompt.name":      agent,
		"quack.prompt.artifact":   artifact,
		"quack.prompt.source":     promptSource,
		"quack.prompt.version_id": promptVersionID,
		"gen_ai.response.id":      "resp-1",
	})
}

func sessionFromEntries(t *testing.T, entries []entry) *Session {
	t.Helper()
	s := &Session{streams: map[StreamKey]*streamState{}}
	for _, e := range toEntries(t, entries) {
		s.ingest(e)
	}
	s.finalize()
	return s
}

func fakeLangfuse(t *testing.T, handler http.HandlerFunc) *langfuse.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return langfuse.New(srv.URL, "pub", "secret", langfuse.WithHTTPClient(srv.Client()))
}

func TestNewPromptSource(t *testing.T) {
	ts := time.Now()
	staticArt, err := artifactsrc.Static("system/judge")
	if err != nil {
		t.Fatalf("static system/judge: %v", err)
	}

	tests := []struct {
		name      string
		entries   []entry
		lf        *langfuse.Client
		storeName string
		wantErr   string          // substring, "" = no error
		want      map[string]bool // name -> expected in resolved map
	}{
		{
			name:    "static matching",
			entries: []entry{promptChat(ts, "worker-r0", "judge", "system/judge", artifactsrc.StaticSource, staticArt.VersionID)},
			want:    map[string]bool{"system/judge": true},
		},
		{
			name:    "static drifted refuses",
			entries: []entry{promptChat(ts, "worker-r0", "judge", "system/judge", artifactsrc.StaticSource, "deadbeef0000")},
			wantErr: "refusing to replay a different version",
		},
		{
			name:    "static unknown artifact refuses",
			entries: []entry{promptChat(ts, "worker-r0", "judge", "system/no-such-agent", artifactsrc.StaticSource, "aaaa")},
			wantErr: "unknown artifact",
		},
		{
			// H2: the artifact name comes from the bundle directory, not the
			// agent's own name - an agent key that differs from its bundle
			// dir must still resolve via quack.prompt.artifact.
			name:    "artifact name differs from agent name",
			entries: []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", artifactsrc.StaticSource, mustStaticVersion(t, "system/code-reviewer"))},
			want:    map[string]bool{"system/code-reviewer": true},
		},
		{
			// H1: rounds re-resolve by design: a name recorded at two different
			// versions across rounds has no single version to pin.
			name: "version moved mid-run refuses naming both",
			entries: []entry{
				promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", artifactsrc.StaticSource, "aaaaaaaaaaaa"),
				promptChat(ts.Add(time.Minute), "worker-r1", "reviewer", "system/code-reviewer", artifactsrc.StaticSource, "bbbbbbbbbbbb"),
			},
			wantErr: "system/code-reviewer@aaaaaaaaaaaa vs system/code-reviewer@bbbbbbbbbbbb",
		},
		{
			name:      "langfuse non-numeric version refuses",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "not-a-number")},
			lf:        fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not call langfuse") }),
			storeName: "langfuse",
			wantErr:   "not numeric",
		},
		{
			name:      "langfuse unreachable is distinguished from missing version",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "7")},
			storeName: "langfuse",
			lf: fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}),
			wantErr: "unreachable, replay not attempted",
		},
		{
			name:      "langfuse permanent error is not reported as unreachable",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "7")},
			storeName: "langfuse",
			lf: fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			}),
			wantErr: "status 400",
		},
		{
			name:      "langfuse present",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "7")},
			storeName: "langfuse",
			lf: fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("version"); got != "7" {
					t.Errorf("version = %q, want 7", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"name":"system/code-reviewer","version":7,"type":"text","prompt":"be nice","config":{},"labels":[],"tags":[],"commitMessage":""}`))
			}),
			want: map[string]bool{"system/code-reviewer": true},
		},
		{
			name:      "langfuse missing version refuses naming name@version",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "9")},
			storeName: "langfuse",
			lf: fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}),
			wantErr: "system/code-reviewer@9: version no longer exists",
		},
		{
			name:    "no store configured refuses",
			entries: []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse", "7")},
			lf:      nil,
			wantErr: "no prompts.store configured",
		},
		{
			// H3: the recorded store and the replaying deployment's configured
			// store are different names - version numbers are per Langfuse
			// project, so a version number alone is not enough.
			name:      "recorded from a different store refuses naming both",
			entries:   []entry{promptChat(ts, "worker-r0", "reviewer", "system/code-reviewer", "langfuse-prod", "7")},
			lf:        fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not call langfuse") }),
			storeName: "langfuse-staging",
			wantErr:   `recorded from store "langfuse-prod", but this deployment's prompts.store is "langfuse-staging"`,
		},
		{
			name: "pre-#1422 entry has no provenance to pin",
			entries: []entry{chat(ts, "node-a", "judge", "worker-r0", "gpt", map[string]any{
				"gen_ai.response.id": "resp-1",
			})},
			want: map[string]bool{}, // resolved map empty; drift path handles it elsewhere
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionFromEntries(t, tc.entries)
			src, err := NewPromptSource(context.Background(), sess, tc.lf, tc.storeName)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPromptSource: %v", err)
			}
			for name, want := range tc.want {
				art, ok, gerr := src.Get(context.Background(), name)
				if gerr != nil {
					t.Fatalf("Get(%q): %v", name, gerr)
				}
				if ok != want {
					t.Fatalf("Get(%q) ok = %v, want %v", name, ok, want)
				}
				if want && art.Body == "" {
					t.Fatalf("Get(%q): empty body", name)
				}
			}
			if _, ok, _ := src.Get(context.Background(), "system/never-recorded"); ok {
				t.Fatalf("Get of a never-recorded name should miss")
			}
			if err := src.Seed(context.Background(), "system/anything", artifactsrc.Artifact{}); err != nil {
				t.Fatalf("Seed: %v, want nil (replay never seeds)", err)
			}
		})
	}
}

func mustStaticVersion(t *testing.T, name string) string {
	t.Helper()
	art, err := artifactsrc.Static(name)
	if err != nil {
		t.Fatalf("static %s: %v", name, err)
	}
	return art.VersionID
}
