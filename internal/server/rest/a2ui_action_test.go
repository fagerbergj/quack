package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendChatMessage_A2UIAction(t *testing.T) {
	post := func(h *Handler, chatID, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.SendChatMessage(rec, req, chatID)
		return rec.Code
	}

	m := &recallModel{}
	h := newTestHandlerWithModel(t, m)
	chatID := mustCreateChat(t, h)
	body := `{"content":"","a2ui_action":{"context":{"answers":{"q1":["a"]}},"source_component_id":"submit","name":"submit_quiz","surface_id":"pr-1085-tutor"}}`
	if code := post(h, chatID, body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	want := `[a2ui_action] {"surface_id":"pr-1085-tutor","name":"submit_quiz","source_component_id":"submit","context":{"answers":{"q1":["a"]}}}`
	m.mu.Lock()
	seen := strings.Join(m.requests, "\n")
	m.mu.Unlock()
	if !strings.Contains(seen, want+"\n") {
		t.Fatalf("model did not see the action line %q; saw:\n%s", want, seen)
	}

	for name, bad := range map[string]string{
		"no surface":         `{"content":"","a2ui_action":{"name":"submit_quiz"}}`,
		"bad surface id":     `{"content":"","a2ui_action":{"name":"submit_quiz","surface_id":"../x"}}`,
		"action and content": `{"content":"hi","a2ui_action":{"name":"submit_quiz","surface_id":"s1"}}`,
		"no action":          `{"content":""}`,
	} {
		if code := post(h, chatID, bad); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, code)
		}
	}
}
