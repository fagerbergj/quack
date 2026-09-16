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

// promptChat builds one recorded llm.call entry stamped with prompt provenance
// (#1420), the shape PromptSource pins from.
func promptChat(ts time.Time, agent, promptSource, promptVersionID string) entry {
	return chat(ts, "node-a", agent, "worker-r0", "gpt", map[string]any{
		"gen_ai.prompt.name":      agent,
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
		name    string
		entries []entry
		lf      *langfuse.Client
		wantErr string          // substring, "" = no error
		want    map[string]bool // name -> expected in resolved map
	}{
		{
			name:    "static matching",
			entries: []entry{promptChat(ts, "judge", artifactsrc.StaticSource, staticArt.VersionID)},
			want:    map[string]bool{"system/judge": true},
		},
		{
			name:    "static drifted refuses",
			entries: []entry{promptChat(ts, "judge", artifactsrc.StaticSource, "deadbeef0000")},
			wantErr: "refusing to replay a different version",
		},
		{
			name:    "langfuse present",
			entries: []entry{promptChat(ts, "code-reviewer", "langfuse", "7")},
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
			name:    "langfuse missing version refuses naming name@version",
			entries: []entry{promptChat(ts, "code-reviewer", "langfuse", "9")},
			lf: fakeLangfuse(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}),
			wantErr: "system/code-reviewer@9",
		},
		{
			name:    "no store configured refuses",
			entries: []entry{promptChat(ts, "code-reviewer", "langfuse", "7")},
			lf:      nil,
			wantErr: "no prompts.store configured",
		},
		{
			name: "pre-P1 entry has no provenance to pin",
			entries: []entry{chat(ts, "node-a", "judge", "worker-r0", "gpt", map[string]any{
				"gen_ai.response.id": "resp-1",
			})},
			want: map[string]bool{}, // resolved map empty; drift path handles it elsewhere
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionFromEntries(t, tc.entries)
			src, err := NewPromptSource(context.Background(), sess, tc.lf)
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
		})
	}
}
