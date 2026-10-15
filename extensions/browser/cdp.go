package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// cdpBrowser speaks raw CDP over one browser-level websocket, dialed on first use and redialed after it drops.
// Each tab blocks disallowed document requests (Fetch) before they leave the browser.
type cdpBrowser struct {
	url     string
	allowed func(string) bool

	mu   sync.Mutex
	conn *cdpConn
}

func (b *cdpBrowser) NewTab(ctx context.Context) (Tab, error) {
	c, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &t); err != nil {
		return nil, err
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &s); err != nil {
		_ = c.call(context.WithoutCancel(ctx), "", "Target.closeTarget", map[string]any{"targetId": t.TargetID}, nil)
		return nil, err
	}
	tab := &cdpTab{c: c, target: t.TargetID, session: s.SessionID}
	c.onPaused(s.SessionID, b.allowed)
	if err := tab.setup(ctx); err != nil {
		_ = tab.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return tab, nil
}

func (b *cdpBrowser) dial(ctx context.Context) (*cdpConn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && b.conn.alive() {
		return b.conn, nil
	}
	ws, _, err := websocket.Dial(ctx, b.url, nil) //nolint:bodyclose // Dial owns resp.Body: nil on success, drained and NopCloser'd on error
	if err != nil {
		return nil, fmt.Errorf("connect to cdp_url: %w", err)
	}
	ws.SetReadLimit(64 << 20) // full-page screenshots arrive base64 in one message
	b.conn = &cdpConn{ws: ws, pending: map[int64]chan cdpMessage{}, paused: map[string]func(string) bool{}, done: make(chan struct{})}
	go b.conn.read()
	return b.conn, nil
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type cdpConn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpMessage
	paused  map[string]func(string) bool // by session: the allow check for Fetch.requestPaused
	done    chan struct{}
	err     error
}

func (c *cdpConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *cdpConn) onPaused(session string, allowed func(string) bool) {
	c.mu.Lock()
	c.paused[session] = allowed
	c.mu.Unlock()
}

func (c *cdpConn) call(ctx context.Context, session, method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id, ch := c.next, make(chan cdpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(cdpMessage{ID: id, SessionID: session, Method: method, Params: raw})
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("cdp %s: %w", method, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("cdp %s: %s", method, m.Error.Message)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-c.done:
		return fmt.Errorf("cdp %s: connection closed: %w", method, c.err)
	case <-ctx.Done():
		return fmt.Errorf("cdp %s: %w", method, ctx.Err())
	}
}

func (c *cdpConn) read() {
	for {
		_, b, err := c.ws.Read(context.Background())
		if err != nil {
			c.err = err
			close(c.done)
			return
		}
		var m cdpMessage
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.Method == "Fetch.requestPaused" {
			c.decide(m)
			continue
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// decide answers a paused document request off the read loop, which must keep reading for the reply.
func (c *cdpConn) decide(m cdpMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	_ = json.Unmarshal(m.Params, &p)
	c.mu.Lock()
	allowed := c.paused[m.SessionID]
	c.mu.Unlock()
	method, params := "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}
	if allowed != nil && allowed(p.Request.URL) {
		method, params = "Fetch.continueRequest", map[string]any{"requestId": p.RequestID}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.call(ctx, m.SessionID, method, params, nil)
	}()
}

type cdpTab struct {
	c       *cdpConn
	target  string
	session string
}

func (t *cdpTab) call(ctx context.Context, method string, params, out any) error {
	return t.c.call(ctx, t.session, method, params, out)
}

func (t *cdpTab) setup(ctx context.Context) error {
	if err := t.call(ctx, "Fetch.enable", map[string]any{"patterns": []map[string]string{{"resourceType": "Document"}}}, nil); err != nil {
		return err
	}
	return t.call(ctx, "Emulation.setDeviceMetricsOverride",
		map[string]any{"width": 1280, "height": 800, "deviceScaleFactor": 1, "mobile": false}, nil)
}

func (t *cdpTab) Navigate(ctx context.Context, url string) error {
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if err := t.call(ctx, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return err
	}
	if r.ErrorText != "" {
		return fmt.Errorf("navigate to %q: %s", url, r.ErrorText)
	}
	return t.settle(ctx)
}

// settle waits for the document to finish loading.
// ponytail: polls readyState after a fixed pause, so a late JS navigation can land after it; use lifecycle events if that bites.
func (t *cdpTab) settle(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		var state string
		err := t.eval(ctx, "document.readyState", &state)
		if err == nil && state == "complete" {
			return nil
		}
		if err != nil && !t.c.alive() {
			return err
		}
	}
}

func (t *cdpTab) URL(ctx context.Context) (string, error) {
	var u string
	return u, t.eval(ctx, "location.href", &u)
}

func (t *cdpTab) Snapshot(ctx context.Context) (Snapshot, error) {
	var s struct {
		Title    string    `json:"title"`
		URL      string    `json:"url"`
		Elements []Element `json:"elements"`
	}
	if err := t.eval(ctx, snapshotJS, &s); err != nil {
		return Snapshot{}, err
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := t.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 70}, &shot); err != nil {
		return Snapshot{}, err
	}
	jpeg, err := base64.StdEncoding.DecodeString(shot.Data)
	return Snapshot{Title: s.Title, URL: s.URL, Elements: s.Elements, JPEG: jpeg}, err
}

func (t *cdpTab) Act(ctx context.Context, index int, action, text string) error {
	args, _ := json.Marshal([]any{index, action, text})
	var r struct {
		Error string  `json:"error"`
		X     float64 `json:"x"`
		Y     float64 `json:"y"`
	}
	if err := t.eval(ctx, "("+actJS+")(..."+string(args)+")", &r); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	switch action {
	case "click":
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			ev := map[string]any{"type": typ, "x": r.X, "y": r.Y, "button": "left", "clickCount": 1}
			if err := t.call(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
				return err
			}
		}
	case "type":
		if err := t.call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
			return err
		}
	}
	return t.settle(ctx)
}

func (t *cdpTab) Close(ctx context.Context) error {
	return t.c.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.target}, nil)
}

func (t *cdpTab) eval(ctx context.Context, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := t.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &r); err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		return fmt.Errorf("page script: %s", r.ExceptionDetails.Text)
	}
	return json.Unmarshal(r.Result.Value, out)
}

// snapshotJS tags every visible interactive element with data-quack-el=<index> and lists role and accessible name.
const snapshotJS = `(() => {
  const implicit = {A: 'link', BUTTON: 'button', SELECT: 'combobox', TEXTAREA: 'textbox', SUMMARY: 'button'};
  const inputs = {checkbox: 'checkbox', radio: 'radio', submit: 'button', button: 'button', reset: 'button', image: 'button', range: 'slider'};
  const role = el => el.getAttribute('role') || implicit[el.tagName] || (el.tagName === 'INPUT' ? inputs[el.type] || 'textbox' : 'generic');
  const text = id => (document.getElementById(id) || {}).innerText || '';
  const name = el => {
    const by = el.getAttribute('aria-labelledby');
    const n = el.getAttribute('aria-label') || (by && by.split(' ').map(text).join(' ')) || (el.labels && el.labels[0] && el.labels[0].innerText) ||
      el.innerText || el.getAttribute('alt') || (el.querySelector('img[alt]') || {}).alt || el.title || el.placeholder ||
      (['submit', 'button'].includes(el.type) ? el.value : '') || '';
    return n.replace(/\s+/g, ' ').trim().slice(0, 80);
  };
  document.querySelectorAll('[data-quack-el]').forEach(el => el.removeAttribute('data-quack-el'));
  const sel = 'a[href],button,input:not([type=hidden]),select,textarea,summary,[contenteditable=""],[contenteditable=true],[onclick],' +
    '[tabindex]:not([tabindex="-1"]),[role=button],[role=link],[role=checkbox],[role=radio],[role=tab],[role=menuitem],[role=option],[role=switch],[role=combobox],[role=textbox]';
  const elements = [];
  for (const el of document.querySelectorAll(sel)) {
    const r = el.getBoundingClientRect(), s = getComputedStyle(el);
    if (r.width === 0 || r.height === 0 || s.visibility === 'hidden' || el.disabled) continue;
    el.setAttribute('data-quack-el', elements.length);
    elements.push({role: role(el), name: name(el)});
  }
  return {title: document.title, url: location.href, elements};
})()`

// actJS finds a tagged element and prepares the action: a click returns its centre, type focuses and clears
// it, select picks the option whose label or value matches.
const actJS = `(i, action, text) => {
  const el = document.querySelector('[data-quack-el="' + i + '"]');
  if (!el) return {error: 'element ' + (i + 1) + ' is no longer on the page; call browser_observe again'};
  el.scrollIntoView({block: 'center'});
  if (action === 'click') { const r = el.getBoundingClientRect(); return {x: r.x + r.width / 2, y: r.y + r.height / 2}; }
  if (action === 'type') { el.focus(); if ('value' in el) el.value = ''; return {}; }
  const opt = Array.from(el.options || []).find(o => o.label.trim() === text || o.value === text);
  if (!opt) return {error: 'element ' + (i + 1) + ' has no option ' + JSON.stringify(text)};
  el.value = opt.value;
  el.dispatchEvent(new Event('input', {bubbles: true}));
  el.dispatchEvent(new Event('change', {bubbles: true}));
  return {};
}`
