package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// toolTimeout bounds one tool call's browser work, page load included.
const toolTimeout = 30 * time.Second

type observation struct {
	Page     string   `json:"page"`
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Elements []string `json:"elements"`
	Omitted  int      `json:"elements_omitted,omitempty"`
	Note     string   `json:"note,omitempty"`

	Screenshot         string `json:"screenshot_artifact,omitempty"`
	ScreenshotRevision int64  `json:"screenshot_revision,omitempty"`
}

type openArgs struct {
	URL string `json:"url"`
}

type pageArgs struct {
	Page string `json:"page"`
}

type actArgs struct {
	Page    string `json:"page"`
	Element int    `json:"element"`
	Action  string `json:"action" jsonschema:"click, type or select"`
	Text    string `json:"text,omitempty" jsonschema:"what to type, or the option to select"`
}

type chooseArgs struct {
	Page string `json:"page"`
	Goal string `json:"goal"`
}

type choice struct {
	Element     int     `json:"element"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
	ClearsActAt bool    `json:"clears_act_at"`
}

func (e *extension) openTool() tool.Tool {
	return newTool("browser_open", "Open a URL in a new browser tab, if its host is allowed. Returns the page id "+
		"and an observation: title, URL, numbered interactive elements and a screenshot artifact.", e.open)
}

func (e *extension) observeTool() tool.Tool {
	return newTool("browser_observe", "Re-read an open page: title, URL, numbered interactive elements and a "+
		"screenshot artifact. Element numbers are valid only until the next observation.", e.observe)
}

func (e *extension) actTool() tool.Tool {
	return newTool("browser_act", "Click, type into, or select an option of a numbered element from the page's "+
		"latest observation. Returns the page's new observation.", e.act)
}

func (e *extension) chooseTool() tool.Tool {
	return newTool("browser_choose", "Ask the decision model which element of the page's latest observation to act "+
		"on for a goal. Returns its pick and probability; act on it with browser_act.", e.choose)
}

func newTool[A, R any](name, desc string, run func(context.Context, A) (R, error)) tool.Tool {
	t, _ := functiontool.New(functiontool.Config{Name: name, Description: desc},
		func(ctx adkagent.Context, a A) (R, error) {
			c, cancel := context.WithTimeout(ctx, toolTimeout)
			defer cancel()
			return run(c, a)
		})
	return t
}

func (e *extension) open(ctx context.Context, a openArgs) (observation, error) {
	if !e.allowed(a.URL) {
		return observation{}, fmt.Errorf("browser_open: %q is not on an allowed host", a.URL)
	}
	tab, err := e.browser.NewTab(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("browser_open: %w", err)
	}
	p := &page{chat: chatOf(ctx), tab: tab}
	if err := e.guard(ctx, p, tab.Navigate(ctx, a.URL)); err != nil {
		_ = tab.Close(context.WithoutCancel(ctx))
		return observation{}, fmt.Errorf("browser_open: %w", err)
	}
	e.mu.Lock()
	e.next++
	id := "p" + strconv.Itoa(e.next)
	e.pages[id] = p
	e.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.snapshot(ctx, id, p)
}

func (e *extension) observe(ctx context.Context, a pageArgs) (observation, error) {
	p, err := e.page(ctx, a.Page)
	if err != nil {
		return observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.snapshot(ctx, a.Page, p)
}

func (e *extension) act(ctx context.Context, a actArgs) (observation, error) {
	if !slices.Contains([]string{"click", "type", "select"}, a.Action) {
		return observation{}, fmt.Errorf("browser_act: action must be click, type or select, got %q", a.Action)
	}
	p, err := e.page(ctx, a.Page)
	if err != nil {
		return observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Element < 1 || a.Element > len(p.last.Elements) {
		return observation{}, fmt.Errorf("browser_act: element %d is not in %s's latest observation (1-%d)", a.Element, a.Page, len(p.last.Elements))
	}
	if err := e.guard(ctx, p, p.tab.Act(ctx, a.Element-1, a.Action, a.Text)); err != nil {
		return observation{}, fmt.Errorf("browser_act: %w", err)
	}
	return e.snapshot(ctx, a.Page, p)
}

// guard re-checks the tab's URL after a navigation or action, resetting it to about:blank on a disallowed host.
func (e *extension) guard(ctx context.Context, p *page, actErr error) error {
	if actErr != nil {
		return actErr
	}
	u, err := p.tab.URL(ctx)
	if err != nil {
		return err
	}
	if e.allowed(u) {
		return nil
	}
	p.last = observation{}
	if err := p.tab.Navigate(ctx, "about:blank"); err != nil {
		return fmt.Errorf("page went to disallowed %q and could not be reset: %w", u, err)
	}
	return fmt.Errorf("page went to %q, which is not on an allowed host; it was reset to about:blank", u)
}

func (e *extension) choose(ctx context.Context, a chooseArgs) (choice, error) {
	if e.host.Decide == nil {
		return choice{}, errors.New("browser_choose: this quack has no decision handler; pick an element yourself")
	}
	p, err := e.page(ctx, a.Page)
	if err != nil {
		return choice{}, err
	}
	p.mu.Lock()
	obs, img := p.last, p.image
	p.mu.Unlock()
	if len(obs.Elements) == 0 {
		return choice{}, fmt.Errorf("browser_choose: %s's latest observation has no elements", a.Page)
	}
	req := sdk.DecideRequest{Point: "element.choose", Options: obs.Elements,
		State: map[string]string{"goal": a.Goal, "page": obs.Title, "url": obs.URL}}
	if img != "" {
		req.Images = []string{img}
	}
	d, err := e.host.Decide(ctx, req)
	if err != nil {
		return choice{}, fmt.Errorf("browser_choose: no decision (%w); pick an element yourself", err)
	}
	i := slices.Index(obs.Elements, d.Top)
	if i < 0 {
		return choice{}, fmt.Errorf("browser_choose: the decision model answered %q, not an element", d.Top)
	}
	return choice{Element: i + 1, Label: d.Top, Probability: d.TopP, ClearsActAt: d.Confident}, nil
}

// page looks up an open tab; a tab belongs to the chat that opened it.
func (e *extension) page(ctx context.Context, id string) (*page, error) {
	e.mu.Lock()
	p, ok := e.pages[id]
	e.mu.Unlock()
	if !ok || p.chat != chatOf(ctx) {
		return nil, fmt.Errorf("browser: no open page %q; open one with browser_open", id)
	}
	return p, nil
}

// snapshot replaces p's latest observation; the caller holds p.mu.
func (e *extension) snapshot(ctx context.Context, id string, p *page) (observation, error) {
	s, err := p.tab.Snapshot(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("browser: observe %s: %w", id, err)
	}
	obs := observation{Page: id, Title: s.Title, URL: s.URL, Elements: []string{}}
	for i, el := range s.Elements[:min(len(s.Elements), e.cfg.MaxElements)] {
		obs.Elements = append(obs.Elements, fmt.Sprintf("[%d] %s %q", i+1, el.Role, el.Name))
	}
	obs.Omitted = len(s.Elements) - len(obs.Elements)
	if len(obs.Elements) == 0 {
		// Observing again returns the same; without the note agents re-observe until the repeat guard stops them.
		obs.Note = "no interactive elements: the page is blank or has nothing to act on - open another URL"
	}
	p.image = ""
	if len(s.JPEG) > 0 {
		p.image = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(s.JPEG)
		obs.Screenshot, obs.ScreenshotRevision = e.saveScreenshot(ctx, id, s.JPEG)
	}
	p.last = obs
	return obs, nil
}

// saveScreenshot stores the screenshot as a chat artifact; "" when the host or call has no artifact store.
func (e *extension) saveScreenshot(ctx context.Context, id string, jpeg []byte) (string, int64) {
	ci, ok := sdk.CallInfoFrom(ctx)
	if !ok || e.host.WriteArtifact == nil {
		return "", 0
	}
	name := "browser-" + id + ".jpg"
	rev, _, err := e.host.WriteArtifact(ci.ChatID, ci.UserID, name, "image/jpeg", jpeg)
	if err != nil {
		if e.host.Log != nil {
			e.host.Log.Warn("browser: save screenshot", "page", id, "err", err)
		}
		return "", 0
	}
	return "bytes:" + name, rev
}

func chatOf(ctx context.Context) string {
	ci, _ := sdk.CallInfoFrom(ctx)
	return ci.ChatID
}
