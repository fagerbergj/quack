package extproc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

// childVar makes the test binary act as the extension child when New re-execs it.
const childVar = "EXTPROC_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childVar) == "1" {
		if err := Serve(os.Stdin, os.Stdout, testFactory, slog.New(slog.NewJSONHandler(os.Stderr, nil))); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	_ = os.Setenv(childVar, "1")
	passEnv = append(passEnv, childVar)
	os.Exit(m.Run())
}

type testExt struct{}

func (testExt) Tools() []tool.Tool { return nil }

func (testExt) UI() sdk.UIDescriptor { return sdk.UIDescriptor{Title: "X", Href: "/x/"} }

func (testExt) RegisterRoutes(authed, _ chi.Router) {
	authed.Post("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "%s auth=%q cookie=%q", body, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
	})
	authed.Get("/pid", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, os.Getpid()) })
	authed.Get("/crash", func(http.ResponseWriter, *http.Request) { os.Exit(3) })
	authed.Get("/block", func(http.ResponseWriter, *http.Request) { select {} })
}

func testFactory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	if string(raw) == "fail" {
		return nil, errors.New("bad config")
	}
	host.Log.Info("factory ran", "data_dir", host.DataDir)
	return testExt{}, nil
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

// startProcess runs the test binary as extension "x" behind a router mounted like quack's.
func startProcess(t *testing.T) (*Process, *httptest.Server, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	host := sdk.Host{Log: slog.New(slog.NewJSONHandler(logs, nil)).With("component", "ext.x"), DataDir: "/data/x"}
	p, err := New("x", os.Args[0], host, []byte("ok"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	root, ext := chi.NewRouter(), chi.NewRouter()
	p.RegisterRoutes(ext, ext)
	root.Mount("/x", ext)
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
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
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(inR, outW, testFactory, slog.New(slog.DiscardHandler)) }()
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

	var init initResult
	_ = json.Unmarshal(roundTrip(1, "initialize", initParams{Protocol: ProtocolVersion, Name: "x"}).Result, &init)
	if init.Protocol != ProtocolVersion || init.UI == nil || init.UI.Title != "X" {
		t.Fatalf("initialize = %+v", init)
	}
	var res httpResult
	_ = json.Unmarshal(roundTrip(2, "http", httpParams{Method: "POST", URL: "/x/echo?a=1", Body: []byte("hi")}).Result, &res)
	if res.Status != http.StatusCreated || !strings.HasPrefix(string(res.Body), "hi ") || res.Header.Get("X-Path") != "/x/echo" {
		t.Fatalf("http = %d %v %q", res.Status, res.Header, res.Body)
	}
	if m := roundTrip(3, "nope", nil); m.Error == nil || m.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method reply = %+v", m)
	}
	if m := roundTrip(4, "http", httpParams{Method: "GET", URL: "/x/missing"}); m.Error != nil {
		t.Fatalf("404 must be a result, not an rpc error: %+v", m.Error)
	}

	_ = inW.Close()
	if err := <-served; err != nil {
		t.Fatalf("Serve after EOF = %v, want nil", err)
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
	waitFor(t, "the child's factory log", func() bool {
		return strings.Contains(logs.String(), `"msg":"factory ran","component":"ext.x","data_dir":"/data/x"`)
	})
}

func TestNewFailsWhenFactoryRejectsConfig(t *testing.T) {
	_, err := New("x", os.Args[0], sdk.Host{Log: slog.New(slog.DiscardHandler)}, []byte("fail"))
	if err == nil || !strings.Contains(err.Error(), "bad config") {
		t.Fatalf("New = %v, want the child's factory error", err)
	}
}

func TestProcessRestartsAfterCrashWithBackoff(t *testing.T) {
	restartMin, restartMax = 50*time.Millisecond, 5*time.Second
	t.Cleanup(func() { restartMin, restartMax = 100*time.Millisecond, 30*time.Second })
	_, srv, logs := startProcess(t)

	for i, wantBackoff := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond} {
		pid := pidOf(t, srv)
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
		time.Sleep(50 * time.Millisecond) // let /block reach the child before it dies
		if code, _ := get(t, srv.URL+"/x/crash"); code != http.StatusBadGateway {
			t.Errorf("crash %d: GET /x/crash = %d, want 502", i, code)
		}
		if code := <-blocked; code != http.StatusBadGateway {
			t.Errorf("crash %d: in-flight request = %d, want 502", i, code)
		}
		waitFor(t, "a restarted child", func() bool {
			code, body := get(t, srv.URL+"/x/pid")
			return code == http.StatusOK && body != strconv.Itoa(pid)
		})
		if !strings.Contains(logs.String(), `"backoff":`+strconv.FormatInt(int64(wantBackoff), 10)) {
			t.Errorf("crash %d: no restart logged with backoff %s:\n%s", i, wantBackoff, logs)
		}
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
