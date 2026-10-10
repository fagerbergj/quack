package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/schema"
)

func TestChatOrigin_Empty(t *testing.T) {
	if got := chatOrigin(""); got != nil {
		t.Errorf("chatOrigin(\"\") = %+v, want nil", got)
	}
}

func TestChatOrigin_Malformed(t *testing.T) {
	if got := chatOrigin("not json"); got != nil {
		t.Errorf("chatOrigin(malformed) = %+v, want nil", got)
	}
}

func TestChatOrigin_MissingRequiredField(t *testing.T) {
	// Extension set but no Label - required per the ChatOrigin schema.
	b, _ := json.Marshal(extsdk.ChatOrigin{Extension: "remarkable"})
	if got := chatOrigin(string(b)); got != nil {
		t.Errorf("chatOrigin(missing label) = %+v, want nil", got)
	}
}

// TestChatOrigin_RoundTrip: the store's JSON (*extsdk.ChatOrigin, untagged so capitalized keys) decodes into
// the wire schema's lowercase shape, including nested Labels.
func TestChatOrigin_RoundTrip(t *testing.T) {
	sdkOrigin := extsdk.ChatOrigin{
		Extension: "remarkable",
		Label:     "Meeting notes",
		Kind:      "document",
		Href:      "https://remarkable.example/doc/42",
		Badge:     "draft",
		Labels: map[string][]extsdk.LabelValue{
			"tags": {
				{Value: "work", Display: "Work"},
				{Value: "urgent"},
			},
		},
	}
	b, err := json.Marshal(sdkOrigin)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	got := chatOrigin(string(b))
	if got == nil {
		t.Fatal("chatOrigin returned nil for a well-formed origin")
	}
	if got.Extension != "remarkable" || got.Label != "Meeting notes" {
		t.Errorf("extension/label = %q/%q, want remarkable/Meeting notes", got.Extension, got.Label)
	}
	if got.Kind == nil || *got.Kind != "document" {
		t.Errorf("Kind = %v, want document", got.Kind)
	}
	if got.Href == nil || *got.Href != "https://remarkable.example/doc/42" {
		t.Errorf("Href = %v, want the doc link", got.Href)
	}
	if got.Badge == nil || *got.Badge != "draft" {
		t.Errorf("Badge = %v, want draft", got.Badge)
	}
	if got.Labels == nil {
		t.Fatal("Labels is nil, want the tags dimension")
	}
	tags := (*got.Labels)["tags"]
	if len(tags) != 2 {
		t.Fatalf("Labels[tags] = %+v, want 2 values", tags)
	}
	if tags[0].Value != "work" || tags[0].Display == nil || *tags[0].Display != "Work" {
		t.Errorf("tags[0] = %+v, want value=work display=Work", tags[0])
	}
	if tags[1].Value != "urgent" || tags[1].Display != nil {
		t.Errorf("tags[1] = %+v, want value=urgent with no display", tags[1])
	}
}

// TestListChats_SurfacesOrigin: origin JSON persisted by SetChatOrigin, as an extension's Dispatch writes it,
// decodes onto ChatSummary.origin.
func TestListChats_SurfacesOrigin(t *testing.T) {
	h := newTestHandler(t)
	chatID := "ext:remarkable:doc-42"
	sdkOrigin := extsdk.ChatOrigin{Extension: "remarkable", Label: "Meeting notes"}
	b, _ := json.Marshal(sdkOrigin)
	if err := h.store.SetChatOrigin(context.Background(), chatID, "ext", string(b)); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.ChatList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("Data = %+v, want 1 chat", out.Data)
	}
	if out.Data[0].Origin == nil || out.Data[0].Origin.Label != "Meeting notes" {
		t.Errorf("Origin = %+v, want label=Meeting notes", out.Data[0].Origin)
	}
}

// TestGetChat_SurfacesOriginAndGithubFields: GetChat builds its summary fields separately from toSummary,
// so github_url/github_repo/github_state/archived/origin must each be set there too.
func TestGetChat_SurfacesOriginAndGithubFields(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	sdkOrigin := extsdk.ChatOrigin{Extension: "remarkable", Label: "Meeting notes"}
	b, _ := json.Marshal(sdkOrigin)
	if err := h.store.SetChatOrigin(context.Background(), chatID, "", string(b)); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID, nil)
	rec := httptest.NewRecorder()
	h.GetChat(rec, req, chatID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var detail schema.ChatDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Origin == nil || detail.Origin.Label != "Meeting notes" {
		t.Errorf("Origin = %+v, want label=Meeting notes", detail.Origin)
	}
}
