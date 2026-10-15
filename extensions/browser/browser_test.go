package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
)

type fakeTab struct {
	url      string
	snap     Snapshot
	actErr   error
	actURL   string // where an action navigates, "" = stays
	acted    []int
	visited  []string
	closed   bool
	redirect string // where Navigate lands instead of its url
}

func (f *fakeTab) Navigate(_ context.Context, u string) error {
	f.visited = append(f.visited, u)
	f.url = u
	if f.redirect != "" && u != "about:blank" {
		f.url = f.redirect
	}
	return nil
}

func (f *fakeTab) URL(context.Context) (string, error) { return f.url, nil }

func (f *fakeTab) Snapshot(context.Context) (Snapshot, error) {
	s := f.snap
	s.URL = f.url
	return s, nil
}

func (f *fakeTab) Act(_ context.Context, i int, _, _ string) error {
	f.acted = append(f.acted, i)
	if f.actURL != "" {
		f.url = f.actURL
	}
	return f.actErr
}

func (f *fakeTab) Close(context.Context) error { f.closed = true; return nil }

type fakeBrowser struct{ tabs []*fakeTab }

func (b *fakeBrowser) NewTab(context.Context) (Tab, error) {
	t := &fakeTab{snap: Snapshot{Title: "Home", JPEG: []byte{0xff, 0xd8},
		Elements: []Element{{"link", "Home"}, {"button", "Sign in"}, {"textbox", "Search"}}}}
	b.tabs = append(b.tabs, t)
	return t, nil
}

func newExt(t *testing.T, host sdk.Host, yaml string) (*extension, *fakeBrowser) {
	t.Helper()
	ext, err := factory(host, []byte("cdp_url: ws://chrome:3000\n"+yaml))
	if err != nil {
		t.Fatal(err)
	}
	e := ext.(*extension)
	b := &fakeBrowser{}
	e.browser = b
	return e, b
}

func chat(id string) context.Context {
	return sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: id, UserID: "u"})
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"",
		"cdp_url: http://chrome:9222",
		"cdp_url: ws://chrome:3000\nmax_elements: -1",
		"cdp_url: ws://chrome:3000\nallowed_hosts: [example.com:443]",
		"cdp_url: ws://chrome:3000\nallowed_hosts: ['*.example.com']",
		"cdp_url: ws://chrome:3000\nallowed: [example.com]",
	} {
		if _, err := factory(sdk.Host{}, []byte(bad)); err == nil {
			t.Errorf("config %q: accepted", bad)
		}
	}
	e, _ := newExt(t, sdk.Host{}, "allowed_hosts: [Example.COM]\nenabled: true")
	if e.cfg.MaxElements != defaultMaxElements || !e.allowed("https://example.com/x") {
		t.Errorf("defaults: max_elements %d, example.com allowed %v", e.cfg.MaxElements, e.allowed("https://example.com/x"))
	}
}

func TestOpenRefusesDisallowedHosts(t *testing.T) {
	e, b := newExt(t, sdk.Host{}, "")
	if _, err := e.open(chat("c"), openArgs{URL: "https://example.com"}); err == nil || len(b.tabs) != 0 {
		t.Errorf("empty allowed_hosts: err %v, %d tabs opened; want refused before any tab", err, len(b.tabs))
	}
	e, b = newExt(t, sdk.Host{}, "allowed_hosts: [example.com]")
	for _, u := range []string{"https://evil.com", "https://example.com.evil.com", "file:///etc/passwd", "javascript:alert(1)"} {
		if _, err := e.open(chat("c"), openArgs{URL: u}); err == nil {
			t.Errorf("open %q: allowed", u)
		}
	}
	if len(b.tabs) != 0 {
		t.Fatalf("%d tabs opened for refused urls", len(b.tabs))
	}
	e.browser = &redirectingBrowser{b}
	if _, err := e.open(chat("c"), openArgs{URL: "https://example.com"}); err == nil || !b.tabs[0].closed {
		t.Errorf("redirect to evil.com: err %v, closed %v; want refused and the tab closed", err, b.tabs[0].closed)
	}
}

type redirectingBrowser struct{ *fakeBrowser }

func (r *redirectingBrowser) NewTab(ctx context.Context) (Tab, error) {
	t, _ := r.fakeBrowser.NewTab(ctx)
	t.(*fakeTab).redirect = "https://evil.com/"
	return t, nil
}

func TestObserveNumbersElementsAndStoresTheScreenshot(t *testing.T) {
	var wrote string
	host := sdk.Host{WriteArtifact: func(chatID, user, name, mime string, data []byte) (int64, bool, error) {
		wrote = chatID + "/" + user + "/" + name + "/" + mime
		return 3, true, nil
	}}
	e, _ := newExt(t, host, "allowed_hosts: [example.com]\nmax_elements: 2")
	obs, err := e.open(chat("c"), openArgs{URL: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`[1] link "Home"`, `[2] button "Sign in"`}
	if obs.Page != "p1" || !slices.Equal(obs.Elements, want) || obs.Omitted != 1 {
		t.Errorf("observation %+v, want p1 with %q and 1 omitted", obs, want)
	}
	if obs.Screenshot != "bytes:browser-p1.jpg" || obs.ScreenshotRevision != 3 || wrote != "c/u/browser-p1.jpg/image/jpeg" {
		t.Errorf("screenshot %q rev %d written as %q", obs.Screenshot, obs.ScreenshotRevision, wrote)
	}
	if _, err := e.observe(chat("other"), pageArgs{Page: "p1"}); err == nil {
		t.Error("another chat observed p1")
	}
}

func TestActOnlyOnTheLatestObservation(t *testing.T) {
	e, b := newExt(t, sdk.Host{}, "allowed_hosts: [example.com]\nmax_elements: 2")
	ctx := chat("c")
	if _, err := e.open(ctx, openArgs{URL: "https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	tab := b.tabs[0]
	for _, a := range []actArgs{
		{Page: "p1", Element: 0, Action: "click"},
		{Page: "p1", Element: 3, Action: "click"}, // past max_elements
		{Page: "p1", Element: 1, Action: "hover"},
		{Page: "p9", Element: 1, Action: "click"},
	} {
		if _, err := e.act(ctx, a); err == nil {
			t.Errorf("act %+v: accepted", a)
		}
	}
	if len(tab.acted) != 0 {
		t.Fatalf("refused acts reached the tab: %v", tab.acted)
	}
	if _, err := e.act(ctx, actArgs{Page: "p1", Element: 2, Action: "click"}); err != nil || !slices.Equal(tab.acted, []int{1}) {
		t.Errorf("act on [2]: %v, tab saw %v; want index 1", err, tab.acted)
	}
	tab.actErr = errors.New("element 2 is no longer on the page")
	if _, err := e.act(ctx, actArgs{Page: "p1", Element: 2, Action: "click"}); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Errorf("stale element: err %v", err)
	}
	tab.actErr, tab.actURL = nil, "https://evil.com/"
	_, err := e.act(ctx, actArgs{Page: "p1", Element: 1, Action: "click"})
	if err == nil || tab.url != "about:blank" {
		t.Errorf("act that left the allowlist: err %v, url %q; want refused and reset", err, tab.url)
	}
	if _, err := e.act(ctx, actArgs{Page: "p1", Element: 1, Action: "click"}); err == nil {
		t.Error("act after the reset used the pre-reset elements")
	}
}

func TestChooseAsksTheDecisionPoint(t *testing.T) {
	var got sdk.DecideRequest
	answer := `[2] button "Sign in"`
	host := sdk.Host{Decide: func(_ context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
		got = req
		return sdk.Decision{Top: answer, TopP: 0.93, Confident: true}, nil
	}}
	e, _ := newExt(t, host, "allowed_hosts: [example.com]")
	ctx := chat("c")
	obs, err := e.open(ctx, openArgs{URL: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.choose(ctx, chooseArgs{Page: "p1", Goal: "sign in"})
	if err != nil || c != (choice{Element: 2, Label: answer, Probability: 0.93, ClearsActAt: true}) {
		t.Errorf("choice %+v %v", c, err)
	}
	state, _ := got.State.(map[string]string)
	if got.Point != "element.choose" || state["goal"] != "sign in" || !slices.Equal(got.Options, obs.Elements) ||
		len(got.Images) != 1 || !strings.HasPrefix(got.Images[0], "data:image/jpeg;base64,") {
		t.Errorf("request %+v", got)
	}
	answer = "[7] link"
	if _, err := e.choose(ctx, chooseArgs{Page: "p1", Goal: "x"}); err == nil {
		t.Error("an answer that is not an element: accepted")
	}
	e.host.Decide = nil
	if _, err := e.choose(ctx, chooseArgs{Page: "p1", Goal: "x"}); err == nil {
		t.Error("no decision handler: accepted")
	}
}
