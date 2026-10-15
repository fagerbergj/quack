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

// scriptedCDP answers the methods a tab's lifecycle sends with canned results and records each method called.
func scriptedCDP(calls chan<- string) http.HandlerFunc {
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
			calls <- m.Method
			_ = ws.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"id":%d,"result":%s}`, m.ID, scriptedResult(m)))
		}
	}
}

func scriptedResult(m cdpMessage) string {
	var p struct {
		URL        string `json:"url"`
		Expression string `json:"expression"`
	}
	_ = json.Unmarshal(m.Params, &p)
	value := func(v string) string { return `{"result":{"value":` + v + `}}` }
	switch {
	case m.Method == "Target.createTarget":
		return `{"targetId":"t1"}`
	case m.Method == "Target.attachToTarget":
		return `{"sessionId":"s1"}`
	case m.Method == "Page.navigate" && strings.Contains(p.URL, "broken"):
		return `{"errorText":"net::ERR_NAME_NOT_RESOLVED"}`
	case m.Method == "Page.captureScreenshot":
		return `{"data":"/9j/"}`
	case p.Expression == "document.readyState":
		return value(`"complete"`)
	case p.Expression == "location.href":
		return value(`"https://example.com/"`)
	case strings.Contains(p.Expression, `[9,"click"`):
		return value(`{"error":"element 9 is gone"}`)
	case strings.Contains(p.Expression, ")(...["):
		return value(`{"x":10,"y":20}`)
	case m.Method == "Runtime.evaluate":
		return value(`{"title":"Home","url":"https://example.com/","elements":[{"role":"link","name":"Home"}]}`)
	}
	return "{}"
}

// TestCDPTabLifecycle drives one tab through navigate, observe, act and close against a scripted endpoint.
func TestCDPTabLifecycle(t *testing.T) {
	calls := make(chan string, 64)
	srv := httptest.NewServer(scriptedCDP(calls))
	defer srv.Close()
	b := &cdpBrowser{url: "ws" + strings.TrimPrefix(srv.URL, "http"), allowed: func(string) bool { return true }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tab, err := b.NewTab(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tab.Navigate(ctx, "https://example.com/"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if err := tab.Navigate(ctx, "https://broken.example/"); err == nil || !strings.Contains(err.Error(), "ERR_NAME_NOT_RESOLVED") {
		t.Errorf("Navigate to a failing URL = %v, want its errorText", err)
	}
	if u, err := tab.URL(ctx); err != nil || u != "https://example.com/" {
		t.Errorf("URL = %q, %v", u, err)
	}
	s, err := tab.Snapshot(ctx)
	if err != nil || s.Title != "Home" || len(s.Elements) != 1 || len(s.JPEG) == 0 {
		t.Fatalf("Snapshot = %+v, %v", s, err)
	}
	if err := tab.Act(ctx, 1, "click", ""); err != nil {
		t.Errorf("click: %v", err)
	}
	if err := tab.Act(ctx, 1, "type", "hello"); err != nil {
		t.Errorf("type: %v", err)
	}
	if err := tab.Act(ctx, 9, "click", ""); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("act on a gone element = %v, want the page script's error", err)
	}
	if err := tab.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	close(calls)
	seen := map[string]bool{}
	for c := range calls {
		seen[c] = true
	}
	for _, want := range []string{"Page.navigate", "Page.captureScreenshot", "Input.dispatchMouseEvent", "Input.insertText", "Target.closeTarget"} {
		if !seen[want] {
			t.Errorf("never sent %s", want)
		}
	}
}
