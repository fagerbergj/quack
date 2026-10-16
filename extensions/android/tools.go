package android

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// toolTimeout bounds one tool call's device work; a uiautomator dump alone can take a few seconds.
const toolTimeout = 30 * time.Second

const (
	dumpCmd  = "uiautomator dump /data/local/tmp/quack-ui.xml >/dev/null && cat /data/local/tmp/quack-ui.xml"
	focusCmd = "dumpsys window | grep mCurrentFocus"
)

type observation struct {
	Package  string   `json:"package"`
	Activity string   `json:"activity,omitempty"`
	Elements []string `json:"elements"`
	Omitted  int      `json:"elements_omitted,omitempty"`
	Note     string   `json:"note,omitempty"`

	Screenshot         string `json:"screenshot_artifact,omitempty"`
	ScreenshotRevision int64  `json:"screenshot_revision,omitempty"`
}

// screen is one reading of the device; e.last keeps the latest observation's, capped to max_elements.
type screen struct {
	chat          string
	pkg, activity string
	elements      []element
	width, height int
	obs           observation
	image         string // screenshot as a data URL, for android_choose
}

type actArgs struct {
	Element   int    `json:"element,omitempty" jsonschema:"element number, for tap, type and long_press"`
	Action    string `json:"action" jsonschema:"tap, type, long_press, back or swipe"`
	Text      string `json:"text,omitempty" jsonschema:"what to type"`
	Direction string `json:"direction,omitempty" jsonschema:"swipe: up, down, left or right, the way the finger moves"`
}

type launchArgs struct {
	Package string `json:"package"`
}

type chooseArgs struct {
	Goal string `json:"goal"`
}

type choice struct {
	Element     int     `json:"element"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
	ClearsActAt bool    `json:"clears_act_at"`
}

var actions = []string{"tap", "type", "long_press", "back", "swipe"}

func (e *extension) observeTool() tool.Tool {
	return newTool("android_observe", "Read the Android screen: foreground package and activity, numbered interactive "+
		"elements and a screenshot artifact. Element numbers are valid only until the next observation.", e.observe)
}

func (e *extension) actTool() tool.Tool {
	return newTool("android_act", "Tap, type into or long_press a numbered element of the latest observation, or press "+
		"back or swipe. Only inside allowed packages. Returns the new observation.", e.act)
}

func (e *extension) launchTool() tool.Tool {
	return newTool("android_launch", "Launch an allowed Android app by package name. Returns the new observation.", e.launch)
}

func (e *extension) chooseTool() tool.Tool {
	return newTool("android_choose", "Ask the decision model which element of the latest observation to act on for a "+
		"goal. Returns its pick and probability; act on it with android_act.", e.choose)
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

func (e *extension) observe(ctx context.Context, _ struct{}) (observation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot(ctx)
}

func (e *extension) act(ctx context.Context, a actArgs) (observation, error) {
	if !slices.Contains(actions, a.Action) {
		return observation{}, fmt.Errorf("android_act: action must be one of %v, got %q", actions, a.Action)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, err := e.look(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("android_act: %w", err)
	}
	if !e.allowed(cur.pkg) {
		return observation{}, fmt.Errorf("android_act: the foreground package %q is not in allowed_packages; "+
			"launch an allowed app with android_launch", cur.pkg)
	}
	cmd, err := e.command(chatOf(ctx), cur, a)
	if err != nil {
		return observation{}, fmt.Errorf("android_act: %w", err)
	}
	if err := e.input(ctx, cmd); err != nil {
		return observation{}, fmt.Errorf("android_act: %w", err)
	}
	return e.after(ctx, "android_act")
}

// command builds a's input command against cur, the screen as it is now.
func (e *extension) command(chat string, cur screen, a actArgs) (string, error) {
	switch a.Action {
	case "back":
		return "input keyevent KEYCODE_BACK", nil
	case "swipe":
		return swipe(cur, a.Direction)
	}
	el, err := e.element(chat, cur, a.Element)
	if err != nil {
		return "", err
	}
	switch a.Action {
	case "tap":
		return fmt.Sprintf("input tap %d %d", el.x, el.y), nil
	case "long_press":
		return fmt.Sprintf("input swipe %d %d %d %d 800", el.x, el.y, el.x, el.y), nil
	}
	if a.Text == "" {
		return "", errors.New("type needs text")
	}
	// input text reads %s as a space and would split on a bare one.
	text := shellQuote(strings.ReplaceAll(a.Text, " ", "%s"))
	return fmt.Sprintf("input tap %d %d && input text %s", el.x, el.y, text), nil
}

// element returns element n of this chat's latest observation, refusing it once the screen has moved under it.
func (e *extension) element(chat string, cur screen, n int) (element, error) {
	if e.last.chat != chat || n < 1 || n > len(e.last.elements) {
		return element{}, fmt.Errorf("element %d is not in this chat's latest observation (1-%d); call android_observe",
			n, len(e.last.elements))
	}
	want := e.last.elements[n-1]
	if n > len(cur.elements) || cur.elements[n-1] != want {
		e.last = screen{}
		return element{}, fmt.Errorf("element %d %s has moved or gone; call android_observe again", n, want.label)
	}
	return want, nil
}

// swipe moves a finger across 60% of the screen through its center.
func swipe(cur screen, dir string) (string, error) {
	x, y, dx, dy := cur.width/2, cur.height/2, cur.width*3/10, cur.height*3/10
	p, ok := map[string][4]int{
		"up": {x, y + dy, x, y - dy}, "down": {x, y - dy, x, y + dy},
		"left": {x + dx, y, x - dx, y}, "right": {x - dx, y, x + dx, y},
	}[dir]
	if !ok {
		return "", fmt.Errorf("swipe direction must be up, down, left or right, got %q", dir)
	}
	return fmt.Sprintf("input swipe %d %d %d %d 300", p[0], p[1], p[2], p[3]), nil
}

func (e *extension) launch(ctx context.Context, a launchArgs) (observation, error) {
	if !e.allowed(a.Package) {
		return observation{}, fmt.Errorf("android_launch: %q is not in allowed_packages", a.Package)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out, err := e.adb.exec(ctx, "monkey -p "+a.Package+" -c android.intent.category.LAUNCHER 1")
	if err != nil {
		return observation{}, fmt.Errorf("android_launch: %w", err)
	}
	if !bytes.Contains(out, []byte("Events injected: 1")) {
		return observation{}, fmt.Errorf("android_launch: %s did not launch: %s", a.Package, bytes.TrimSpace(out))
	}
	return e.after(ctx, "android_launch")
}

// input runs an input command; it prints nothing on success.
func (e *extension) input(ctx context.Context, cmd string) error {
	out, err := e.adb.exec(ctx, cmd)
	if err != nil {
		return err
	}
	if out = bytes.TrimSpace(out); len(out) > 0 {
		return fmt.Errorf("%s: %s", service(cmd), head(out))
	}
	return nil
}

// after lets the UI settle, observes, and presses Home when the screen left the allowed packages.
func (e *extension) after(ctx context.Context, tool string) (observation, error) {
	select {
	case <-time.After(e.settle):
	case <-ctx.Done():
		return observation{}, fmt.Errorf("%s: %w", tool, ctx.Err())
	}
	obs, err := e.snapshot(ctx)
	if err != nil || e.allowed(obs.Package) {
		return obs, err
	}
	e.last = screen{}
	if _, err := e.adb.exec(ctx, "input keyevent KEYCODE_HOME"); err != nil {
		return observation{}, fmt.Errorf("%s: the screen moved to disallowed %q and Home failed: %w", tool, obs.Package, err)
	}
	return observation{}, fmt.Errorf("%s: the screen moved to %q, which is not in allowed_packages; pressed Home", tool, obs.Package)
}

func (e *extension) choose(ctx context.Context, a chooseArgs) (choice, error) {
	if e.host.Decide == nil {
		return choice{}, errors.New("android_choose: this quack has no decision handler; pick an element yourself")
	}
	e.mu.Lock()
	last := e.last
	e.mu.Unlock()
	if last.chat != chatOf(ctx) || len(last.obs.Elements) == 0 {
		return choice{}, errors.New("android_choose: this chat's latest observation has no elements; call android_observe")
	}
	req := sdk.DecideRequest{Point: "element.choose", Options: last.obs.Elements,
		State: map[string]string{"goal": a.Goal, "package": last.pkg, "activity": last.activity}}
	if last.image != "" {
		req.Images = []string{last.image}
	}
	d, err := e.host.Decide(ctx, req)
	if err != nil {
		return choice{}, fmt.Errorf("android_choose: no decision (%w); pick an element yourself", err)
	}
	i := slices.Index(last.obs.Elements, d.Top)
	if i < 0 {
		return choice{}, fmt.Errorf("android_choose: the decision model answered %q, not an element", d.Top)
	}
	return choice{Element: i + 1, Label: d.Top, Probability: d.TopP, ClearsActAt: d.Confident}, nil
}

// look reads the foreground app and the interactive elements, without a screenshot.
func (e *extension) look(ctx context.Context) (screen, error) {
	focus, err := e.adb.exec(ctx, focusCmd)
	if err != nil {
		return screen{}, err
	}
	dump, err := e.adb.exec(ctx, dumpCmd)
	if err != nil {
		return screen{}, err
	}
	s := screen{chat: chatOf(ctx)}
	s.pkg, s.activity = parseFocus(focus)
	s.elements, s.width, s.height, err = parseElements(dump)
	return s, err
}

// snapshot replaces e.last with a fresh observation; the caller holds e.mu.
func (e *extension) snapshot(ctx context.Context) (observation, error) {
	s, err := e.look(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("android: observe: %w", err)
	}
	shot, err := e.adb.exec(ctx, "screencap -p")
	if err != nil {
		return observation{}, fmt.Errorf("android: observe: %w", err)
	}
	jpg, err := toJPEG(shot)
	if err != nil {
		return observation{}, fmt.Errorf("android: observe: %w", err)
	}
	obs := observation{Package: s.pkg, Activity: s.activity, Elements: []string{}}
	obs.Omitted = max(len(s.elements)-e.cfg.MaxElements, 0)
	s.elements = s.elements[:len(s.elements)-obs.Omitted]
	for i, el := range s.elements {
		obs.Elements = append(obs.Elements, fmt.Sprintf("[%d] %s", i+1, el.label))
	}
	obs.Note = e.note(s.pkg, len(obs.Elements))
	s.image = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg)
	obs.Screenshot, obs.ScreenshotRevision = e.saveScreenshot(ctx, jpg)
	s.obs = obs
	e.last = s
	return obs, nil
}

// note tells the agent what to do when it cannot act here; without one agents re-observe until the repeat guard stops them.
func (e *extension) note(pkg string, elements int) string {
	switch {
	case !e.allowed(pkg):
		return fmt.Sprintf("%q is not in allowed_packages, so actions are refused here; launch an allowed app with android_launch", pkg)
	case elements == 0:
		return "no interactive elements on this screen; swipe, go back or launch another app"
	}
	return ""
}

// saveScreenshot stores the screenshot as a chat artifact; "" when the host or call has no artifact store.
func (e *extension) saveScreenshot(ctx context.Context, jpg []byte) (string, int64) {
	ci, ok := sdk.CallInfoFrom(ctx)
	if !ok || e.host.WriteArtifact == nil {
		return "", 0
	}
	const name = "android-screen.jpg"
	rev, _, err := e.host.WriteArtifact(ci.ChatID, ci.UserID, name, "image/jpeg", jpg)
	if err != nil {
		if e.host.Log != nil {
			e.host.Log.Warn("android: save screenshot", "err", err)
		}
		return "", 0
	}
	return "bytes:" + name, rev
}

func chatOf(ctx context.Context) string {
	ci, _ := sdk.CallInfoFrom(ctx)
	return ci.ChatID
}
