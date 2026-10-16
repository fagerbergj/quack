package android

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
)

const testDump = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0">
<node class="android.widget.FrameLayout" package="com.example.app" enabled="true" bounds="[0,0][1080,2400]">
  <node class="android.widget.Button" text="Sign in" clickable="true" enabled="true" bounds="[100,200][300,400]"/>
  <node class="android.widget.Button" text="Disabled" clickable="true" enabled="false" bounds="[100,500][300,600]"/>
  <node class="androidx.recyclerview.widget.RecyclerView" focusable="true" scrollable="true" enabled="true" bounds="[0,600][1080,2000]">
    <node class="android.widget.LinearLayout" clickable="true" enabled="true" bounds="[0,600][1080,800]">
      <node class="android.widget.TextView" text="Wi-Fi" enabled="true" bounds="[40,620][400,700]"/>
    </node>
  </node>
  <node class="android.widget.EditText" resource-id="com.example.app:id/search" focusable="true" enabled="true" bounds="[0,2000][1080,2100]"/>
  <node class="android.widget.ImageButton" content-desc="Menu" long-clickable="true" enabled="true" bounds="[1000,0][1080,80]"/>
  <node class="android.view.View" clickable="true" enabled="true" bounds="[0,0][0,0]"/>
</node></hierarchy>`

// fakeADB is an adb server speaking the host protocol, with canned output per command.
type fakeADB struct {
	mu      sync.Mutex
	focus   string // pkg/activity
	dump    string
	png     []byte
	ran     []string
	onInput func(cmd string) // runs under mu, e.g. to switch the foreground app
}

func (f *fakeADB) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return ln.Addr().String()
}

func readRequest(r io.Reader) string {
	n := make([]byte, 4)
	_, _ = io.ReadFull(r, n)
	size, _ := strconv.ParseUint(string(n), 16, 16)
	p := make([]byte, size)
	_, _ = io.ReadFull(r, p)
	return string(p)
}

func (f *fakeADB) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	if tr := readRequest(c); tr != "host:transport-any" && tr != "host:transport:emulator-5554" {
		_, _ = io.WriteString(c, "FAIL0010device not found")
		return
	}
	_, _ = io.WriteString(c, "OKAY")
	out := f.run(strings.TrimPrefix(readRequest(c), "exec:"))
	_, _ = c.Write(append([]byte("OKAY"), out...))
}

func (f *fakeADB) run(cmd string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, cmd)
	switch {
	case cmd == focusCmd:
		return []byte("  mCurrentFocus=Window{1a2b u0 " + f.focus + "}\n")
	case cmd == dumpCmd:
		return []byte(f.dump)
	case cmd == "screencap -p":
		return f.png
	case strings.HasPrefix(cmd, "monkey -p com.example.app "):
		f.focus = "com.example.app/.Main"
		return []byte("  bash arg: -p\nEvents injected: 1\n")
	case strings.HasPrefix(cmd, "monkey "):
		return []byte("** No activities found to run, monkey aborted.\n")
	case strings.HasPrefix(cmd, "input "):
		if f.onInput != nil {
			f.onInput(cmd)
		}
		return nil
	}
	return []byte("/system/bin/sh: " + cmd + ": not found")
}

func (f *fakeADB) inputs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in []string
	for _, c := range f.ran {
		if strings.HasPrefix(c, "input ") || strings.HasPrefix(c, "monkey ") {
			in = append(in, c)
		}
	}
	return in
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newExt(t *testing.T, host sdk.Host, yaml string) (*extension, *fakeADB) {
	t.Helper()
	f := &fakeADB{focus: "com.example.app/.Main", dump: testDump, png: tinyPNG(t)}
	ext, err := factory(host, []byte("adb_addr: "+f.serve(t)+"\n"+yaml))
	if err != nil {
		t.Fatal(err)
	}
	e := ext.(*extension)
	e.settle = 0
	return e, f
}

func chat(id string) context.Context {
	return sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: id, UserID: "u"})
}

func TestConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"",
		"adb_addr: emulator",
		"adb_addr: emulator:5037\nmax_elements: -1",
		"adb_addr: emulator:5037\nallowed_packages: [settings]",
		"adb_addr: emulator:5037\nallowed_packages: ['com.x; reboot']",
		"adb_addr: emulator:5037\nallowed: [com.example.app]",
	} {
		if _, err := factory(sdk.Host{}, []byte(bad)); err == nil {
			t.Errorf("config %q: accepted", bad)
		}
	}
	e, _ := newExt(t, sdk.Host{}, "allowed_packages: [com.example.app]\nenabled: true")
	if e.cfg.MaxElements != defaultMaxElements || !e.allowed("com.example.app") || e.allowed("com.example") {
		t.Errorf("defaults: max_elements %d, allowed %v", e.cfg.MaxElements, e.cfg.AllowedPackages)
	}
	if len(e.Tools()) != 4 || len(e.DecisionPoints()) != 1 {
		t.Errorf("%d tools, %d decision points", len(e.Tools()), len(e.DecisionPoints()))
	}
}

func TestRequestFraming(t *testing.T) {
	var sent bytes.Buffer
	rw := struct {
		io.Reader
		io.Writer
	}{strings.NewReader("OKAY"), &sent}
	if err := request(rw, "host:version"); err != nil || sent.String() != "000chost:version" {
		t.Errorf("OKAY: %v, sent %q", err, sent.String())
	}
	rw.Reader = strings.NewReader("FAIL0005nope!")
	if err := request(rw, "exec:input text secret"); err == nil || !strings.Contains(err.Error(), "nope!") ||
		strings.Contains(err.Error(), "secret") {
		t.Errorf("FAIL: %v, want the server's message without the command's arguments", err)
	}
	rw.Reader = strings.NewReader("WHAT")
	if err := request(rw, "host:version"); err == nil {
		t.Error("an unknown reply: accepted")
	}
	f := &fakeADB{}
	c := &adbClient{addr: f.serve(t), serial: "emulator-9999"}
	if _, err := c.exec(context.Background(), "true"); err == nil || !strings.Contains(err.Error(), "device not found") {
		t.Errorf("unknown serial: %v", err)
	}
	c.serial = "emulator-5554"
	if out, err := c.exec(context.Background(), "true"); err != nil || !strings.Contains(string(out), "not found") {
		t.Errorf("known serial: %q %v", out, err)
	}
}

func TestObserveNumbersElementsAndStoresTheScreenshot(t *testing.T) {
	var wrote string
	var jpg []byte
	host := sdk.Host{WriteArtifact: func(chatID, user, name, mime string, data []byte) (int64, bool, error) {
		wrote, jpg = chatID+"/"+user+"/"+name+"/"+mime, data
		return 2, true, nil
	}}
	e, _ := newExt(t, host, "allowed_packages: [com.example.app]\nmax_elements: 3")
	obs, err := e.observe(chat("c"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`[1] Button "Sign in"`, `[2] LinearLayout "Wi-Fi"`, `[3] EditText "search"`}
	if obs.Package != "com.example.app" || obs.Activity != ".Main" || !slices.Equal(obs.Elements, want) || obs.Omitted != 1 || obs.Note != "" {
		t.Errorf("observation %+v, want %q and 1 omitted", obs, want)
	}
	if obs.Screenshot != "bytes:android-screen.jpg" || obs.ScreenshotRevision != 2 || wrote != "c/u/android-screen.jpg/image/jpeg" ||
		!bytes.HasPrefix(jpg, []byte{0xff, 0xd8}) {
		t.Errorf("screenshot %q rev %d written as %q", obs.Screenshot, obs.ScreenshotRevision, wrote)
	}
	if els, w, h, _ := parseElements([]byte(testDump)); len(els) != 4 || els[3].label != `ImageButton "Menu"` || w != 1080 || h != 2400 {
		t.Errorf("uncapped elements %+v, screen %dx%d", els, w, h)
	}
}

func TestActTapsAtTheElementCenter(t *testing.T) {
	e, f := newExt(t, sdk.Host{}, "allowed_packages: [com.example.app]")
	ctx := chat("c")
	if _, err := e.observe(ctx, struct{}{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []actArgs{
		{Element: 1, Action: "tap"},
		{Element: 2, Action: "long_press"},
		{Element: 3, Action: "type", Text: "it's me"},
		{Action: "swipe", Direction: "up"},
		{Action: "back"},
	} {
		if _, err := e.act(ctx, a); err != nil {
			t.Fatalf("act %+v: %v", a, err)
		}
	}
	want := []string{"input tap 200 300", "input swipe 540 700 540 700 800", `input tap 540 2050 && input text 'it'\''s%sme'`,
		"input swipe 540 1920 540 480 300", "input keyevent KEYCODE_BACK"}
	if got := f.inputs(); !slices.Equal(got, want) {
		t.Errorf("inputs %q, want %q", got, want)
	}
	for _, a := range []actArgs{
		{Element: 1, Action: "hover"},
		{Element: 0, Action: "tap"},
		{Element: 5, Action: "tap"},
		{Element: 3, Action: "type"},
		{Action: "swipe", Direction: "sideways"},
	} {
		if _, err := e.act(ctx, a); err == nil {
			t.Errorf("act %+v: accepted", a)
		}
	}
	if _, err := e.act(chat("other"), actArgs{Element: 1, Action: "tap"}); err == nil {
		t.Error("another chat acted on this chat's observation")
	}
	if got := f.inputs(); len(got) != len(want) {
		t.Errorf("refused acts reached the device: %q", got[len(want):])
	}
}

func TestStaleElementIsRefused(t *testing.T) {
	e, f := newExt(t, sdk.Host{}, "allowed_packages: [com.example.app]")
	ctx := chat("c")
	if _, err := e.observe(ctx, struct{}{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dump = strings.Replace(testDump, "[100,200][300,400]", "[100,900][300,1100]", 1)
	f.mu.Unlock()
	if _, err := e.act(ctx, actArgs{Element: 1, Action: "tap"}); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Errorf("tap on a moved element: %v", err)
	}
	if _, err := e.act(ctx, actArgs{Element: 2, Action: "tap"}); err == nil || len(f.inputs()) != 0 {
		t.Errorf("tap after a stale refusal: %v, inputs %q; want refused until the next observation", err, f.inputs())
	}
}

func TestDisallowedPackagesRefuseActions(t *testing.T) {
	e, f := newExt(t, sdk.Host{}, "")
	ctx := chat("c")
	obs, err := e.observe(ctx, struct{}{})
	if err != nil || !strings.Contains(obs.Note, "not in allowed_packages") {
		t.Fatalf("observe with no allowed packages: %v, note %q", err, obs.Note)
	}
	if _, err := e.act(ctx, actArgs{Element: 1, Action: "tap"}); err == nil {
		t.Error("tap with an empty allowlist: accepted")
	}
	if _, err := e.launch(ctx, launchArgs{Package: "com.example.app"}); err == nil {
		t.Error("launch with an empty allowlist: accepted")
	}
	if got := f.inputs(); len(got) != 0 {
		t.Fatalf("refused calls reached the device: %q", got)
	}

	e, f = newExt(t, sdk.Host{}, "allowed_packages: [com.example.app]")
	f.onInput = func(cmd string) {
		if cmd == "input tap 200 300" {
			f.focus = "com.android.chrome/.Main"
		}
	}
	if _, err := e.observe(ctx, struct{}{}); err != nil {
		t.Fatal(err)
	}
	_, err = e.act(ctx, actArgs{Element: 1, Action: "tap"})
	if err == nil || !strings.Contains(err.Error(), "com.android.chrome") || !slices.Contains(f.inputs(), "input keyevent KEYCODE_HOME") {
		t.Errorf("tap that left the allowlist: %v, inputs %q; want refused and Home pressed", err, f.inputs())
	}
	if _, err := e.act(ctx, actArgs{Element: 1, Action: "tap"}); err == nil {
		t.Error("tap inside a disallowed app: accepted")
	}
}

func TestLaunchOnlyAllowedPackages(t *testing.T) {
	e, f := newExt(t, sdk.Host{}, "allowed_packages: [com.example.app, com.example.nolauncher]")
	f.focus = "com.android.launcher3/.Launcher"
	obs, err := e.launch(chat("c"), launchArgs{Package: "com.example.app"})
	if err != nil || obs.Package != "com.example.app" {
		t.Errorf("launch: %+v %v", obs, err)
	}
	if _, err := e.launch(chat("c"), launchArgs{Package: "com.example.nolauncher"}); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("launch without a launcher activity: %v", err)
	}
	if _, err := e.launch(chat("c"), launchArgs{Package: "com.evil"}); err == nil {
		t.Error("launch of a disallowed package: accepted")
	}
}

func TestChooseAsksTheDecisionPoint(t *testing.T) {
	var got sdk.DecideRequest
	answer := `[1] Button "Sign in"`
	host := sdk.Host{Decide: func(_ context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
		got = req
		return sdk.Decision{Top: answer, TopP: 0.91, Confident: true}, nil
	}}
	e, _ := newExt(t, host, "allowed_packages: [com.example.app]")
	ctx := chat("c")
	if _, err := e.choose(ctx, chooseArgs{Goal: "sign in"}); err == nil {
		t.Error("choose before any observation: accepted")
	}
	obs, err := e.observe(ctx, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.choose(ctx, chooseArgs{Goal: "sign in"})
	if err != nil || c != (choice{Element: 1, Label: answer, Probability: 0.91, ClearsActAt: true}) {
		t.Errorf("choice %+v %v", c, err)
	}
	state, _ := got.State.(map[string]string)
	if got.Point != "element.choose" || state["goal"] != "sign in" || state["package"] != "com.example.app" ||
		!slices.Equal(got.Options, obs.Elements) || len(got.Images) != 1 || !strings.HasPrefix(got.Images[0], "data:image/jpeg;base64,") {
		t.Errorf("request %+v", got)
	}
	answer = "[9] Button"
	if _, err := e.choose(ctx, chooseArgs{Goal: "x"}); err == nil {
		t.Error("an answer that is not an element: accepted")
	}
	if _, err := e.choose(chat("other"), chooseArgs{Goal: "x"}); err == nil {
		t.Error("another chat chose from this chat's observation")
	}
	e.host.Decide = nil
	if _, err := e.choose(ctx, chooseArgs{Goal: "x"}); err == nil {
		t.Error("no decision handler: accepted")
	}
}

func TestParseFocus(t *testing.T) {
	for out, want := range map[string]string{
		"  mCurrentFocus=Window{8f u0 com.android.settings/com.android.settings.Settings}\n": "com.android.settings",
		"  mCurrentFocus=null\n": "",
		"":                       "",
	} {
		if pkg, _ := parseFocus([]byte(out)); pkg != want {
			t.Errorf("parseFocus(%q) = %q, want %q", out, pkg, want)
		}
	}
	if _, err := toJPEG([]byte("error: no display")); err == nil {
		t.Error("toJPEG of non-PNG output: accepted")
	}
	if _, _, _, err := parseElements([]byte("ERROR: null root node")); err == nil {
		t.Error("parseElements of an error message: accepted")
	}
}
