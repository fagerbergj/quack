package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// fakeExportAPI serves the chats in st from the same stores a local export reads, the way the quack API does:
// list, detail with its setup, recording bundle, artifact listing and artifact bytes.
func fakeExportAPI(t *testing.T, ls ledger.LedgerStore, st *store.Store, rc *recordstore.Client, setup extsdk.Setup, arts []schema.ArtifactSummary) *httptest.Server {
	t.Helper()
	summary := func(ctx context.Context, id string) schema.ChatSummary {
		c, err := st.GetChat(ctx, id)
		if err != nil || c == nil {
			t.Errorf("get chat %q: %v", id, err)
			return schema.ChatSummary{}
		}
		repo, href, state := c.GitHub()
		return schema.ChatSummary{Id: c.ID, UpdatedAt: c.UpdatedAt, GithubRepo: &repo, GithubUrl: &href, GithubState: (*schema.ChatSummaryGithubState)(&state)}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		chats, _, err := st.ListChats(r.Context(), store.ChatsPageMaxLimit, "", store.ChatsScope{Active: true, Archived: true})
		if err != nil {
			t.Errorf("list chats: %v", err)
		}
		out := schema.ChatList{Data: []schema.ChatSummary{}}
		for _, c := range chats {
			out.Data = append(out.Data, summary(r.Context(), c.ID))
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/v1/chats/{chat}", func(w http.ResponseWriter, r *http.Request) {
		s := summary(r.Context(), r.PathValue("chat"))
		writeJSON(w, schema.ChatDetail{
			Id: s.Id, UpdatedAt: s.UpdatedAt, GithubRepo: s.GithubRepo, GithubUrl: s.GithubUrl,
			GithubState: (*schema.ChatDetailGithubState)(s.GithubState),
			Setup:       &schema.ChatSetup{Repo: &setup.Repo, BaseRef: &setup.BaseRef, WorkBranch: &setup.WorkBranch, ExistingHeadRef: &setup.ExistingHeadRef},
		})
	})
	mux.HandleFunc("GET /api/v1/chats/{chat}/recording", func(w http.ResponseWriter, r *http.Request) {
		if err := ledger.AssembleBundle(r.Context(), ls, r.PathValue("chat"), "test", w); err != nil {
			t.Errorf("assemble bundle: %v", err)
		}
	})
	mux.HandleFunc("GET /api/v1/chats/{chat}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, schema.ArtifactList{Data: arts})
	})
	mux.HandleFunc("GET /api/v1/chats/{chat}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		rev, _ := strconv.Atoi(r.URL.Query().Get("revision"))
		data, ok, err := rc.LoadVersion(r.Context(), r.PathValue("name"), rev)
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunRemoteDatasetExport_MatchesLocal: exporting over a server's API yields the same items a local export of
// the same recording does - pinned clone, input artifacts at their listed revision, the over-budget artifact left
// out by name - whether the chat is named or found by --repo.
func TestRunRemoteDatasetExport_MatchesLocal(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chatID := "ext:github:github-acme-widget-1"
	setup := extsdk.Setup{Repo: "https://github.com/acme/widget.git", BaseRef: "main", WorkBranch: "quack/issue-1", ExistingHeadRef: "feat/x"}
	origin, _ := json.Marshal(struct {
		extsdk.ChatOrigin
		Setup extsdk.Setup `json:"quackSetup"`
	}{extsdk.ChatOrigin{
		Extension: "github", Label: "acme/widget#1", Href: "https://github.com/acme/widget/pull/1", State: extsdk.SubjectMerged,
		Labels: map[string][]extsdk.LabelValue{"repo": {{Value: "acme/widget"}}},
	}, setup})
	if err := st.SetChatOrigin(ctx, chatID, "u", string(origin)); err != nil {
		t.Fatal(err)
	}
	chat, err := st.GetChat(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, artifactref.AppName, store.SessionUserFor(*chat), chatID)
	for _, a := range [][2]string{{"issue", `{"title":"as listed"}`}, {"issue", `{"title":"edited later"}`}, {"files", `"` + strings.Repeat("f", itemBudget) + `"`}} {
		if _, _, err := rc.SaveBlob(ctx, "bytes", []byte(a[1]), "application/json", a[0], recordstore.Lineage{}); err != nil {
			t.Fatal(err)
		}
	}
	task := "review\n<artifacts>\n  <artifact id=\"bytes:issue\" revision=\"1\" status=\"new\">1 object</artifact>\n" +
		"  <artifact id=\"bytes:files\" revision=\"1\" status=\"new\">3000 files</artifact>\n" +
		"  <artifact id=\"bytes:gone\" revision=\"1\" status=\"new\">1 object</artifact>\n</artifacts>\n"
	ls := ledgertest.NewMemStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var revs []schema.ArtifactRevisionInfo
	for i, turn := range []string{"t1", "t2"} {
		at, head, node := base.Add(time.Duration(i)*time.Hour), strings.Repeat(fmt.Sprint(i+1), 40), "review"
		payload := fmt.Sprintf(`{"id":"review_record:%s","revision":1,"lineage":{"node_id":"review","head_sha":%q}}`, turn, head)
		for _, e := range []ledger.Entry{
			{ChatID: chatID, TurnID: turn, NodeID: node, Kind: ledger.KindNodeStarted, At: at},
			{ChatID: chatID, TurnID: turn, NodeID: node, Kind: ledger.KindArtifactRevision, Key: "review_record:" + turn, At: at.Add(time.Second), Payload: json.RawMessage(payload)},
			llmCallRoundEntry(chatID, node, "code-reviewer", "worker-r0", at.Add(2*time.Second), task+turn, "verdict "+turn),
		} {
			if _, err := ls.AppendIntent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		created := at.Add(time.Second)
		revs = append(revs, schema.ArtifactRevisionInfo{Revision: 1, TurnId: &turn, CreatedAt: &created, Lineage: &schema.ArtifactLineage{NodeId: &node, HeadSha: &head}})
	}
	api := fakeExportAPI(t, ls, st, rc, setup, []schema.ArtifactSummary{{Name: "review_record", Revisions: revs}})

	local := newFakeLangfuse(t)
	lsrv := local.server()
	defer lsrv.Close()
	if _, _, err := RunDatasetExport(ctx, ls, st, newTestClient(t, lsrv), ExportOpts{ChatID: chatID, Dataset: "my-dataset", Artifacts: svc}); err != nil {
		t.Fatal(err)
	}
	if len(local.items) != 2 {
		t.Fatalf("local export: want one item per review turn, got %d", len(local.items))
	}
	for _, body := range local.items {
		in := body["input"].(map[string]any)
		if in["setup"] == nil || in["artifacts"] == nil || fmt.Sprint(in["skipped_artifacts"]) != "[files]" || body["expectedOutput"] == nil {
			t.Fatalf("fixture must exercise setup, artifacts, the skip budget and expected output; got %v", in)
		}
	}
	for _, opts := range []ExportOpts{{ChatID: chatID}, {Repo: "acme/widget"}} {
		remote := newFakeLangfuse(t)
		rsrv := remote.server()
		opts.Dataset = "my-dataset"
		_, _, err := RunRemoteDatasetExport(ctx, &Client{BaseURL: api.URL, HTTP: api.Client()}, newTestClient(t, rsrv), opts)
		rsrv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(remote.items, local.items) {
			lb, _ := json.Marshal(local.items)
			rb, _ := json.Marshal(remote.items)
			t.Fatalf("opts %+v: remote items differ from local\nlocal  %.3000s\nremote %.3000s", opts, lb, rb)
		}
	}
}
