package extproc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// childVar makes the test binary act as the extension child when New re-execs it; modeVar
// picks a misbehaving child instead of Serve.
const (
	childVar = "EXTPROC_TEST_CHILD"
	modeVar  = "EXTPROC_TEST_MODE"
)

func TestMain(m *testing.M) {
	if os.Getenv(childVar) == "1" {
		os.Exit(runChild(os.Getenv(modeVar)))
	}
	_ = os.Setenv(childVar, "1")
	passEnv = append(passEnv, childVar, modeVar)
	os.Exit(m.Run())
}

// fakeInits are misbehaving children's initialize results, by mode.
var fakeInits = map[string]string{
	"badproto": fmt.Sprintf(`{"protocol":%d}`, ProtocolVersion+1),
	"baddecl":  fmt.Sprintf(`{"protocol":%d,"tools":[{"name":"t","declaration":1}]}`, ProtocolVersion),
}

func runChild(mode string) int {
	if res, ok := fakeInits[mode]; ok {
		// Answers initialize with res, then waits for EOF.
		sc := bufio.NewScanner(os.Stdin)
		sc.Scan()
		fmt.Printf(`{"jsonrpc":"2.0","id":1,"result":%s}`+"\n", res)
		for sc.Scan() {
		}
		return 0
	}
	if err := Serve(os.Stdin, os.Stdout, testFactory, slog.New(slog.NewJSONHandler(os.Stderr, nil))); err != nil {
		return 1
	}
	return 0
}

// startedCtx is the ctx the last testExt.Start received in this process.
var startedCtx context.Context

type testExt struct {
	host      sdk.Host
	failStart bool
	slowStart bool
}

// runsSeen is every RunEnded this child process received, as runEndedParams JSON.
var (
	runsMu   sync.Mutex
	runsSeen []string
)

func (testExt) RunEnded(chatID string, outcome sdk.RunOutcome) {
	b, _ := json.Marshal(runEndedParams{ChatID: chatID, Outcome: outcome})
	runsMu.Lock()
	defer runsMu.Unlock()
	runsSeen = append(runsSeen, string(b))
}

// testDispatchRequest exercises every field kind the wire must carry: bytes, a duration, nested maps.
func testDispatchRequest(id string) sdk.DispatchRequest {
	return sdk.DispatchRequest{
		Chat: sdk.ChatRef{LocalID: id, Title: "t", Origin: &sdk.ChatOrigin{Extension: "x", Kind: "document",
			Labels: map[string][]sdk.LabelValue{"folder": {{Value: "A", Display: "A"}}}}},
		Ask: sdk.Ask{Message: "m", Attachments: []sdk.Attachment{{Name: id + ".pdf", MIME: "application/pdf", Data: []byte{0, 1, 0xff}}}},
		Run: sdk.RunConfig{Workflow: "w", Timeout: time.Minute},
	}
}

func (testExt) Tools() []tool.Tool { return []tool.Tool{echoTool()} }

func (testExt) ArtifactSchemas() map[string]json.RawMessage { return testSchemas }

func (testExt) DecisionPoints() []sdk.DecisionPoint { return testPoints }

func (testExt) UI() sdk.UIDescriptor { return sdk.UIDescriptor{Title: "X", Href: "/x/"} }

func (e testExt) Start(ctx context.Context) error {
	if e.failStart {
		return errors.New("start refused")
	}
	if e.slowStart {
		time.Sleep(2 * time.Second)
	}
	startedCtx = ctx
	return nil
}

func (e testExt) RegisterRoutes(authed, _ chi.Router) {
	authed.Get("/dispatch", func(w http.ResponseWriter, r *http.Request) {
		err := e.host.Dispatch(r.Context(), testDispatchRequest(r.URL.Query().Get("id")))
		_, _ = fmt.Fprintf(w, "inflight=%t err=%v", errors.Is(err, sdk.ErrRunInFlight), err)
	})
	authed.Get("/runs", func(w http.ResponseWriter, _ *http.Request) {
		runsMu.Lock()
		defer runsMu.Unlock()
		_, _ = fmt.Fprint(w, strings.Join(runsSeen, "\n"))
	})
	authed.Get("/wait", func(_ http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(os.Stderr, "waiting")
		<-r.Context().Done()
		fmt.Fprintln(os.Stderr, "request cancelled")
	})
	authed.Post("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "%s auth=%q cookie=%q", body, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
	})
	authed.Get("/pid", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, os.Getpid()) })
	authed.Get("/env", func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, os.Getenv(r.URL.Query().Get("k"))) })
	authed.Get("/started", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, startedCtx != nil && startedCtx.Err() == nil)
	})
	authed.Get("/big", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		_, _ = w.Write(bytes.Repeat([]byte("a"), n))
	})
	authed.Get("/biglog", func(http.ResponseWriter, *http.Request) {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", 2<<20))
		fmt.Fprintln(os.Stderr, "after the big line")
	})
	authed.Get("/callbacks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "read=%t decide=%t", e.host.ReadArtifact != nil, e.host.Decide != nil)
	})
	authed.Get("/read", func(w http.ResponseWriter, r *http.Request) {
		data, ok := e.host.ReadArtifact("ext:x:1", "u", r.URL.Query().Get("name"))
		_, _ = fmt.Fprintf(w, "%s %t", data, ok)
	})
	authed.Get("/decide", func(w http.ResponseWriter, r *http.Request) {
		d, err := e.host.Decide(r.Context(), sdk.DecideRequest{Point: r.URL.Query().Get("point"), Baseline: "true", State: decideState{Zeta: 1, Alpha: "<a>"}})
		_, _ = fmt.Fprintf(w, "%+v %v", d, err)
	})
	authed.Get("/crash", func(http.ResponseWriter, *http.Request) { os.Exit(3) })
	authed.Get("/block", func(http.ResponseWriter, *http.Request) {
		fmt.Fprintln(os.Stderr, "blocking")
		select {}
	})
}

func testFactory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	if string(raw) == "fail" {
		return nil, errors.New("bad config")
	}
	host.Log.Info("factory ran", "data_dir", host.DataDir)
	host.Log.Warn("factory warned")
	return testExt{host: host, failStart: string(raw) == "failstart", slowStart: string(raw) == "slowstart"}, nil
}

// syncBuf collects the host's JSON log output across goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newProcess(config string, dispatch sdk.DispatchFunc) (*Process, *syncBuf, error) {
	return newProcessHost(config, sdk.Host{Dispatch: dispatch})
}

func newProcessHost(config string, host sdk.Host) (*Process, *syncBuf, error) {
	logs := &syncBuf{}
	host.Log, host.DataDir = slog.New(slog.NewJSONHandler(logs, nil)).With("component", "ext.x"), "/data/x"
	p, err := New("x", os.Args[0], host, []byte(config))
	return p, logs, err
}

// startProcess runs the test binary as extension "x" behind a router mounted like quack's.
func startProcess(t *testing.T) (*Process, *httptest.Server, *syncBuf) {
	t.Helper()
	return startProcessWith(t, nil)
}

func startProcessWith(t *testing.T, dispatch sdk.DispatchFunc) (*Process, *httptest.Server, *syncBuf) {
	t.Helper()
	return startProcessHost(t, sdk.Host{Dispatch: dispatch})
}

func startProcessHost(t *testing.T, host sdk.Host) (*Process, *httptest.Server, *syncBuf) {
	t.Helper()
	p, logs, err := newProcessHost("ok", host)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root, ext := chi.NewRouter(), chi.NewRouter()
	p.RegisterRoutes(ext, ext)
	root.Mount("/x", ext)
	srv := httptest.NewServer(root)
	t.Cleanup(func() {
		_ = p.Close() // first: srv.Close waits on requests a hung child still holds
		srv.Close()
	})
	return p, srv, logs
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func pidOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	code, body := get(t, srv.URL+"/x/pid")
	pid, err := strconv.Atoi(body)
	if code != http.StatusOK || err != nil {
		t.Fatalf("GET /x/pid = %d %q", code, body)
	}
	return pid
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestServeRoundTrip(t *testing.T) {
	maxBody = 64
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(inR, outW, testFactory, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() { maxBody = 16 << 20 })
	w, dec := &writer{enc: json.NewEncoder(inW)}, json.NewDecoder(outR)
	roundTrip := func(id int64, method string, params any) message {
		raw, _ := json.Marshal(params)
		if err := w.send(message{ID: id, Method: method, Params: raw}); err != nil {
			t.Fatal(err)
		}
		var m message
		if err := dec.Decode(&m); err != nil || m.ID != id || m.JSONRPC != "2.0" {
			t.Fatalf("%s: reply %+v, err %v", method, m, err)
		}
		return m
	}

	if m := roundTrip(1, "http", httpParams{Method: "GET", URL: "/x/pid"}); m.Error == nil || !strings.Contains(m.Error.Message, "not initialized") {
		t.Fatalf("http before initialize = %+v, want a not-initialized rpc error", m)
	}
	var init initResult
	_ = json.Unmarshal(roundTrip(2, "initialize", initParams{Protocol: ProtocolVersion, Name: "x", Serves: []string{methodTool}}).Result, &init)
	if init.Protocol != ProtocolVersion || init.UI == nil || init.UI.Title != "X" {
		t.Fatalf("initialize = %+v", init)
	}
	var res httpResult
	_ = json.Unmarshal(roundTrip(3, "http", httpParams{Method: "POST", URL: "/x/echo?a=1", Body: []byte("hi")}).Result, &res)
	if res.Status != http.StatusCreated || !strings.HasPrefix(string(res.Body), "hi ") || res.Header.Get("X-Path") != "/x/echo" {
		t.Fatalf("http = %d %v %q", res.Status, res.Header, res.Body)
	}
	if m := roundTrip(4, "nope", nil); m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method reply = %+v", m)
	}
	if m := roundTrip(5, "http", httpParams{Method: "GET", URL: "/x/missing"}); m.Error != nil {
		t.Fatalf("404 must be a result, not an rpc error: %+v", m.Error)
	}
	if m := roundTrip(6, "http", httpParams{Method: "GET", URL: "/x/big?n=65"}); m.Error == nil || !strings.Contains(m.Error.Message, "response too large") {
		t.Fatalf("response over maxBody = %+v, want a response-too-large rpc error", m)
	}

	_ = inW.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve after EOF = %v, want nil", err)
	}
	if startedCtx == nil || startedCtx.Err() == nil {
		t.Errorf("Starter.Start ctx = %v, want started and cancelled once Serve returns", startedCtx)
	}
}

func TestServeRejectsOtherProtocol(t *testing.T) {
	s := &server{factory: testFactory, log: slog.New(slog.DiscardHandler)}
	raw, _ := json.Marshal(initParams{Protocol: ProtocolVersion + 1})
	if _, err := s.initialize(raw); err == nil {
		t.Fatal("initialize accepted a protocol it does not speak")
	}
}

func TestProcessForwardsRequestsAndLogs(t *testing.T) {
	p, srv, logs := startProcess(t)
	if p.UI().Title != "X" {
		t.Errorf("UI = %+v", p.UI())
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/x/echo?q=1", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if want := `body auth="" cookie=""`; resp.StatusCode != http.StatusCreated || string(body) != want {
		t.Errorf("POST /x/echo = %d %q, want 201 %q", resp.StatusCode, body, want)
	}
	if resp.Header.Get("X-Path") != "/x/echo" {
		t.Errorf("X-Path = %q, want the full mounted path", resp.Header.Get("X-Path"))
	}
	if _, started := get(t, srv.URL+"/x/started"); started != "true" {
		t.Errorf("child Starter ran with a live ctx = %s, want true", started)
	}
	waitFor(t, "the child's factory logs at their own levels", func() bool {
		l := logs.String()
		return strings.Contains(l, `"level":"INFO","msg":"factory ran","component":"ext.x","data_dir":"/data/x"`) &&
			strings.Contains(l, `"level":"WARN","msg":"factory warned","component":"ext.x"}`)
	})
}

func TestChildEnvIsAllowlisted(t *testing.T) {
	t.Setenv("QUACK_FAKE_SECRET", "s3cret")
	_, srv, _ := startProcess(t)
	if _, v := get(t, srv.URL+"/x/env?k=QUACK_FAKE_SECRET"); v != "" {
		t.Errorf("child sees QUACK_FAKE_SECRET=%q, want it absent", v)
	}
	if _, v := get(t, srv.URL+"/x/env?k=PATH"); v != os.Getenv("PATH") {
		t.Errorf("child PATH = %q, want quack's", v)
	}
}

func TestForwardLogsSurvivesOverlongLine(t *testing.T) {
	_, srv, logs := startProcess(t)
	go func() { _, _ = http.Get(srv.URL + "/x/biglog") }() // blocks for good if stderr stops draining
	waitFor(t, "the line after a 2MiB stderr line", func() bool { return strings.Contains(logs.String(), "after the big line") })
}

func TestBodyCaps(t *testing.T) {
	// The child is a separate process and keeps the default cap, so only the host's caps apply.
	maxBody = 1 << 10
	t.Cleanup(func() { maxBody = 16 << 20 })
	p, srv, _ := startProcess(t)

	resp, err := http.Post(srv.URL+"/x/echo", "text/plain", strings.NewReader(strings.Repeat("b", 2<<10)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized request = %d, want 413", resp.StatusCode)
	}
	if code, _ := get(t, srv.URL+"/x/big?n=200000"); code != http.StatusBadGateway {
		t.Errorf("reply line over the host's cap = %d, want 502", code)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x/echo", iotest.ErrReader(errors.New("client went away"))))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreadable request body = %d, want 502 (413 is only for size)", rec.Code)
	}
}

func TestNewFails(t *testing.T) {
	for _, tc := range []struct{ name, config, mode, want string }{
		{"factory rejects config", "fail", "", "bad config"},
		{"Starter fails", "failstart", "", "start refused"},
		{"child speaks another protocol", "ok", "badproto", "protocol"},
		{"child reports an undecodable tool", "ok", "baddecl", "tool t: declaration"},
		{"child initializes past initTimeout", "slowstart", "", "deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := initTimeout
			initTimeout = 300 * time.Millisecond
			t.Cleanup(func() { initTimeout = orig })
			t.Setenv(modeVar, tc.mode)
			if _, _, err := newProcess(tc.config, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

var backoffRE = regexp.MustCompile(`"msg":"extension process exited; restarting".*"backoff":(\d+)`)

// crash kills the child and returns the backoff its restart logged.
func crash(t *testing.T, srv *httptest.Server, logs *syncBuf) time.Duration {
	t.Helper()
	pid, before := pidOf(t, srv), len(logs.String())
	if code, _ := get(t, srv.URL+"/x/crash"); code != http.StatusBadGateway {
		t.Errorf("GET /x/crash = %d, want 502", code)
	}
	waitFor(t, "a restarted child", func() bool {
		code, body := get(t, srv.URL+"/x/pid")
		return code == http.StatusOK && body != strconv.Itoa(pid)
	})
	m := backoffRE.FindStringSubmatch(logs.String()[before:])
	if m == nil {
		t.Fatalf("no restart logged:\n%s", logs.String()[before:])
	}
	ns, _ := strconv.ParseInt(m[1], 10, 64)
	return time.Duration(ns)
}

func TestProcessRestartsAfterCrashWithBackoff(t *testing.T) {
	restartMin, restartMax = 50*time.Millisecond, 2*time.Second
	t.Cleanup(func() { restartMin, restartMax = 100*time.Millisecond, 30*time.Second })
	_, srv, logs := startProcess(t)

	blocked := make(chan int, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/x/block")
		if err != nil {
			blocked <- 0
			return
		}
		_ = resp.Body.Close()
		blocked <- resp.StatusCode
	}()
	waitFor(t, "/block to reach the child", func() bool { return strings.Contains(logs.String(), `"line":"blocking"`) })
	got := []time.Duration{crash(t, srv, logs)}
	if code := <-blocked; code != http.StatusBadGateway {
		t.Errorf("in-flight request across a crash = %d, want 502", code)
	}
	got = append(got, crash(t, srv, logs))
	time.Sleep(restartMax + 200*time.Millisecond) // outlive the cap, so the next crash starts over
	got = append(got, crash(t, srv, logs))
	if want := []time.Duration{restartMin, 2 * restartMin, restartMin}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("restart backoffs = %v, want %v (double, then reset after outliving restartMax)", got, want)
	}
}

func TestCloseStopsChild(t *testing.T) {
	p, srv, _ := startProcess(t)
	pid := pidOf(t, srv)
	_ = p.Close()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("child %d still exists after Close (kill -0: %v)", pid, err)
	}
	if code, _ := get(t, srv.URL+"/x/pid"); code != http.StatusBadGateway {
		t.Errorf("GET after Close = %d, want 502", code)
	}
}

// A child stuck on an in-flight request ignores stdin EOF; Close must SIGKILL it after stopGrace.
func TestCloseKillsChildAfterGrace(t *testing.T) {
	stopGrace = 200 * time.Millisecond
	t.Cleanup(func() { stopGrace = 5 * time.Second })
	p, srv, logs := startProcess(t)
	pid := pidOf(t, srv)
	go func() { _, _ = http.Get(srv.URL + "/x/block") }()
	waitFor(t, "/block to reach the child", func() bool { return strings.Contains(logs.String(), `"line":"blocking"`) })
	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL) // so the cleanup's Close can return
		t.Fatal("Close did not return: the hung child was never killed")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("child %d still exists after Close (kill -0: %v)", pid, err)
	}
}

func TestCloseInterruptsBackoff(t *testing.T) {
	restartMin = time.Hour
	t.Cleanup(func() { restartMin = 100 * time.Millisecond })
	p, srv, logs := startProcess(t)
	get(t, srv.URL+"/x/crash")
	waitFor(t, "the restart wait", func() bool { return strings.Contains(logs.String(), "restarting") })
	start := time.Now()
	_ = p.Close()
	if d := time.Since(start); d > time.Second {
		t.Errorf("Close took %s during an hour-long backoff", d)
	}
}
