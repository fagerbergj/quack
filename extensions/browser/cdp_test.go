package browser

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCDP answers every command with an empty result (createTarget and attachToTarget with ids), and once the
// tab is set up pauses one allowed and one disallowed document request.
func fakeCDP(answered chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx := r.Context()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m cdpMessage
			_ = json.Unmarshal(b, &m)
			result := map[string]string{"Target.createTarget": `{"targetId":"t1"}`, "Target.attachToTarget": `{"sessionId":"s1"}`}[m.Method]
			if strings.HasPrefix(m.Method, "Fetch.") && m.Method != "Fetch.enable" {
				answered <- m.Method + " " + string(m.Params)
			}
			_ = ws.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"id":%d,"result":%s}`, m.ID, cmp.Or(result, "{}")))
			if m.Method == "Emulation.setDeviceMetricsOverride" {
				for i, u := range []string{"https://example.com/", "https://evil.com/"} {
					ev := fmt.Sprintf(`{"sessionId":"s1","method":"Fetch.requestPaused","params":{"requestId":"r%d","request":{"url":%q}}}`, i, u)
					_ = ws.Write(ctx, websocket.MessageText, []byte(ev))
				}
			}
		}
	}
}

// TestCDPBlocksDisallowedDocuments: a tab answers each paused document request by the allowlist.
func TestCDPBlocksDisallowedDocuments(t *testing.T) {
	answered := make(chan string, 2)
	srv := httptest.NewServer(fakeCDP(answered))
	defer srv.Close()
	b := &cdpBrowser{url: "ws" + strings.TrimPrefix(srv.URL, "http"), allowed: func(u string) bool { return u == "https://example.com/" }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.NewTab(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for range 2 {
		select {
		case a := <-answered:
			got[a] = true
		case <-ctx.Done():
			t.Fatalf("answers so far %v", got)
		}
	}
	if !got[`Fetch.continueRequest {"requestId":"r0"}`] || !got[`Fetch.failRequest {"errorReason":"BlockedByClient","requestId":"r1"}`] {
		t.Errorf("answers %v, want example.com continued and evil.com failed", got)
	}
}
