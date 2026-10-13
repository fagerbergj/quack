package extproc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
)

func TestDispatchCrossesTheWire(t *testing.T) {
	var mu sync.Mutex
	var got []sdk.DispatchRequest
	_, srv, _ := startProcessWith(t, func(_ context.Context, req sdk.DispatchRequest) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, req)
		if req.Chat.LocalID == "busy" {
			return fmt.Errorf("chat ext:x:busy %w; retry", sdk.ErrRunInFlight)
		}
		return nil
	})

	if _, body := get(t, srv.URL+"/x/dispatch?id=doc-1"); body != "inflight=false err=<nil>" {
		t.Errorf("accepted dispatch, child saw %q", body)
	}
	// The child's errors.Is must hold, as remarkable counts a busy document by it.
	if _, body := get(t, srv.URL+"/x/dispatch?id=busy"); body != "inflight=true err=chat ext:x:busy already has a run in flight; retry" {
		t.Errorf("refused dispatch, child saw %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []sdk.DispatchRequest{testDispatchRequest("doc-1"), testDispatchRequest("busy")}; !reflect.DeepEqual(got, want) {
		t.Errorf("host received %+v\nwant %+v", got, want)
	}
}

func TestRunEndedReachesChild(t *testing.T) {
	p, srv, _ := startProcess(t)
	var want []string
	for _, o := range []sdk.RunOutcome{
		{Status: sdk.RunPaused, NodeID: "n1", Answer: "interim"},
		{Status: sdk.RunFailed, Error: "boom", PlanRan: true, TimedOut: true, Unverified: "u"},
	} {
		p.RunEnded("ext:x:doc-1", o)
		line, _ := json.Marshal(runEndedParams{ChatID: "ext:x:doc-1", Outcome: o})
		want = append(want, string(line))
		waitFor(t, "RunEnded "+string(o.Status)+" in the child", func() bool {
			_, body := get(t, srv.URL+"/x/runs")
			return body == strings.Join(want, "\n")
		})
	}
}

// Each side serves the other's calls off its read loop: the child's request waits on quack's Dispatch,
// which calls back into the child before answering. Served inline, either side would deadlock.
func TestCallsCrossWithoutDeadlock(t *testing.T) {
	var proc atomic.Pointer[Process]
	p, srv, _ := startProcessWith(t, func(ctx context.Context, req sdk.DispatchRequest) error {
		pp := proc.Load()
		pp.RunEnded("ext:x:"+req.Chat.LocalID, sdk.RunOutcome{Status: sdk.RunDone})
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		pp.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x/pid", nil).WithContext(ctx))
		if rec.Code != http.StatusOK {
			return fmt.Errorf("nested call into the child = %d", rec.Code)
		}
		return nil
	})
	proc.Store(p)

	const n = 16
	bodies := make(chan string, n)
	for i := range n {
		go func() {
			resp, err := http.Get(fmt.Sprintf("%s/x/dispatch?id=%d", srv.URL, i))
			if err != nil {
				bodies <- err.Error()
				return
			}
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			bodies <- string(b)
		}()
	}
	deadline := time.After(10 * time.Second)
	for range n {
		select {
		case b := <-bodies:
			if b != "inflight=false err=<nil>" {
				t.Errorf("dispatch with a nested call = %q", b)
			}
		case <-deadline:
			t.Fatal("concurrent calls in both directions deadlocked")
		}
	}
	waitFor(t, "every RunEnded in the child", func() bool {
		_, body := get(t, srv.URL+"/x/runs")
		return strings.Count(body, `"Status":"done"`) == n
	})
}

func TestClientDisconnectCancelsChildRequest(t *testing.T) {
	_, srv, logs := startProcess(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/x/wait", nil)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitFor(t, "/wait to reach the child", func() bool { return strings.Contains(logs.String(), `"line":"waiting"`) })
	cancel()
	waitFor(t, "the child's request ctx to end", func() bool { return strings.Contains(logs.String(), `"line":"request cancelled"`) })
}

// Stdin EOF must fail a handler's pending call into quack, or the drain waits for the host's SIGKILL.
func TestServeFailsPendingCallsAtEOF(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan error, 1)
	go func() { served <- Serve(inR, outW, testFactory, slog.New(slog.DiscardHandler)) }()
	host := newConn(inW, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, inW.Close() // quack shuts down instead of answering the child's dispatch
	}, slog.New(slog.DiscardHandler))
	go func() { _ = host.read(outR) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.call(ctx, methodInitialize, initParams{Protocol: ProtocolVersion, Name: "x"}, &initResult{}); err != nil {
		t.Fatal(err)
	}
	var res httpResult
	if err := host.call(ctx, methodHTTP, httpParams{Method: http.MethodGet, URL: "/x/dispatch?id=a"}, &res); err != nil || !strings.Contains(string(res.Body), errClosed.Error()) {
		t.Fatalf("dispatch pending at stdin EOF = %s, %v; want it failed with errClosed", res.Body, err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return: the handler's Dispatch is still waiting on a closed host")
	}
}
