package extproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

const maxLogLine = 64 << 10

// vars so tests can shrink them. maxBody caps request and response bodies (PiG's RPC command limit).
// initTimeout bounds factory+Start; it matches the longest in-process Start (remarkable's 30s login client).
var (
	stopGrace   = 5 * time.Second
	maxBody     = 16 << 20
	initTimeout = 30 * time.Second
)

// passEnv is all of quack's environment a child sees: what an outbound HTTP client needs, never secrets.
var passEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// The restart backoff's floor and cap; vars so tests can shrink them.
var restartMin, restartMax = 100 * time.Millisecond, 30 * time.Second

// Process is an extension running as a supervised child: started by New, restarted with backoff
// when it exits, stopped by Close. It implements sdk.Extension, sdk.UI, sdk.RunObserver, sdk.ArtifactSchemas
// and sdk.DecisionPoints for quack's loader, the last three from what the child reported at initialize.
type Process struct {
	name     string
	path     string
	params   initParams
	log      *slog.Logger
	host     sdk.Host
	ui       sdk.UIDescriptor
	observes bool
	tools    []tool.Tool
	schemas  map[string]json.RawMessage
	points   []sdk.DecisionPoint

	mu  sync.Mutex
	cur *child

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var (
	_ sdk.Extension       = (*Process)(nil)
	_ sdk.UI              = (*Process)(nil)
	_ sdk.RunObserver     = (*Process)(nil)
	_ sdk.ArtifactSchemas = (*Process)(nil)
	_ sdk.DecisionPoints  = (*Process)(nil)
	_ io.Closer           = (*Process)(nil)
)

// New starts name's extension from the binary at path and initializes it with host's settings and
// the raw extensions.<name> block; an error means the child refused or never answered.
func New(name, path string, host sdk.Host, config []byte) (*Process, error) {
	p := &Process{
		name: name, path: path, log: host.Log, host: host,
		params: initParams{Protocol: ProtocolVersion, Name: name, Config: string(config),
			DataDir: host.DataDir, Version: host.Version, PublicURL: host.PublicURL, Serves: serves(host)},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if host.Location != nil {
		p.params.Location = host.Location.String()
	}
	c, res, err := p.spawn()
	if err != nil {
		return nil, err
	}
	if p.tools, err = remoteTools(p, res.Tools); err != nil {
		c.stop()
		return nil, fmt.Errorf("extension %s: %w", name, err)
	}
	if res.UI != nil {
		p.ui = *res.UI
	}
	p.observes, p.schemas, p.points = res.ObservesRuns, res.ArtifactSchemas, res.DecisionPoints
	p.cur = c
	go p.supervise(c)
	return p, nil
}

// serves lists the optional child calls host can answer; Dispatch is always offered, as before them.
func serves(host sdk.Host) []string {
	s := []string{methodTool}
	if host.ReadArtifact != nil {
		s = append(s, methodReadArtifact)
	}
	if host.Decide != nil {
		s = append(s, methodDecide)
	}
	return s
}

// Tools are proxies built once from the first child's report; each call goes to whichever child is current.
func (p *Process) Tools() []tool.Tool { return p.tools }

func (p *Process) ArtifactSchemas() map[string]json.RawMessage { return p.schemas }

func (p *Process) DecisionPoints() []sdk.DecisionPoint { return p.points }

// RunEnded forwards the outcome without waiting on the child; one sent while the child is down is lost.
func (p *Process) RunEnded(chatID string, outcome sdk.RunOutcome) {
	if !p.observes {
		return
	}
	if err := p.current().notify(methodRunEnded, runEndedParams{ChatID: chatID, Outcome: outcome}); err != nil {
		p.log.Warn("extension process down; RunEnded not delivered", "chat", chatID, "status", outcome.Status, "err", err)
	}
}

// serveChild answers the child's calls into quack; it asks only for the callbacks initialize offered.
func (p *Process) serveChild(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch {
	case method == methodDispatch:
		return nil, p.serveDispatch(ctx, params)
	case method == methodReadArtifact && p.host.ReadArtifact != nil:
		return p.serveReadArtifact(params)
	case method == methodDecide && p.host.Decide != nil:
		return p.serveDecide(ctx, params)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
}

func (p *Process) serveDispatch(ctx context.Context, params json.RawMessage) error {
	if p.host.Dispatch == nil {
		return errors.New("extproc: this host has no Dispatch")
	}
	var req sdk.DispatchRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return err
	}
	return p.host.Dispatch(ctx, req)
}

func (p *Process) serveReadArtifact(params json.RawMessage) (any, error) {
	var a readArtifactParams
	if err := json.Unmarshal(params, &a); err != nil {
		return nil, err
	}
	data, ok := p.host.ReadArtifact(a.ChatID, a.User, a.Name)
	return readArtifactResult{Data: data, OK: ok}, nil
}

func (p *Process) serveDecide(ctx context.Context, params json.RawMessage) (any, error) {
	// Decoding into a non-nil pointer keeps State as the child's bytes, so the handler sees its key order.
	req := sdk.DecideRequest{State: &json.RawMessage{}}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	return p.host.Decide(ctx, req)
}

// RegisterRoutes forwards everything under /<name>/ behind quack's session auth. Public
// (webhook) routes are not bridged yet, so an out-of-process extension cannot serve one.
func (p *Process) RegisterRoutes(authed, _ chi.Router) {
	authed.Handle("/", p)
	authed.Handle("/*", p)
}

func (p *Process) UI() sdk.UIDescriptor { return p.ui }

// Close stops the child (stdin EOF, then SIGKILL after a grace period) and its supervisor.
func (p *Process) Close() error {
	p.closeOnce.Do(func() { close(p.stop) })
	<-p.done
	return nil
}

func (p *Process) current() *child {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// ServeHTTP forwards one request; quack's credentials never reach the child.
func (p *Process) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(maxBody)))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "request body unreadable", http.StatusBadGateway)
		return
	}
	h := r.Header.Clone()
	h.Del("Authorization")
	h.Del("Cookie")
	var res httpResult
	if err := p.current().call(r.Context(), methodHTTP, httpParams{Method: r.Method, URL: r.URL.RequestURI(), Header: h, Body: body}, &res); err != nil {
		if r.Context().Err() == nil {
			p.log.Warn("extension request failed", "path", r.URL.Path, "err", err)
		}
		http.Error(w, "extension unavailable", http.StatusBadGateway)
		return
	}
	maps.Copy(w.Header(), res.Header)
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

func (p *Process) spawn() (*child, initResult, error) {
	var res initResult
	start := time.Now()
	c, err := startChild(p.path, p.log, p.serveChild)
	if err != nil {
		return nil, res, fmt.Errorf("extension %s: start %s: %w", p.name, p.path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	err = c.call(ctx, methodInitialize, p.params, &res)
	if err == nil && res.Protocol != ProtocolVersion {
		err = fmt.Errorf("child speaks protocol %d, host %d", res.Protocol, ProtocolVersion)
	}
	if err != nil {
		c.stop()
		return nil, res, fmt.Errorf("extension %s: initialize: %w", p.name, err)
	}
	p.log.Info("extension process ready", "pid", c.cmd.Process.Pid, "startup", time.Since(start))
	return c, res, nil
}

// supervise restarts the child whenever it exits until Close; backoff doubles per crash
// and resets once a child outlives the cap.
func (p *Process) supervise(c *child) {
	defer close(p.done)
	backoff := restartMin
	for {
		select {
		case <-p.stop:
			c.stop()
			return
		case <-c.done:
		}
		if time.Since(c.started) > restartMax {
			backoff = restartMin
		}
		p.log.Warn("extension process exited; restarting", "err", c.err, "backoff", backoff)
		var ok bool
		if c, backoff, ok = p.respawn(backoff); !ok {
			return
		}
	}
}

// respawn retries spawn with growing backoff; ok is false when Close interrupted it.
func (p *Process) respawn(backoff time.Duration) (*child, time.Duration, bool) {
	for {
		select {
		case <-p.stop:
			return nil, backoff, false
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, restartMax)
		c, _, err := p.spawn()
		if err == nil {
			p.mu.Lock()
			p.cur = c
			p.mu.Unlock()
			return c, backoff, true
		}
		p.log.Error("extension process restart failed", "err", err)
	}
}

// child is one running process and the link to it.
type child struct {
	*conn
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time

	done chan struct{}
	err  error // exit status; read only after done
}

func startChild(path string, log *slog.Logger, handle handlerFunc) (*child, error) {
	cmd := exec.Command(path)
	cmd.Env = childEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{conn: newConn(stdin, handle, log), cmd: cmd, stdin: stdin, started: time.Now(), done: make(chan struct{})}
	go c.run(stdout, stderr, log)
	return c, nil
}

func childEnv() []string {
	var env []string
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// run reads both pipes to EOF before Wait, as exec.Cmd requires, then marks the child done.
func (c *child) run(stdout, stderr io.Reader, log *slog.Logger) {
	var wg sync.WaitGroup
	wg.Go(func() { forwardLogs(stderr, log) })
	if err := c.read(stdout); err != nil {
		log.Error("extension process broke the protocol; killing it", "err", err)
		_ = c.cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, stdout) // keep a dying child from blocking on a full pipe
	wg.Wait()
	c.err = c.cmd.Wait()
	close(c.done)
}

// stop closes stdin, the child's shutdown signal, and kills it if it outlives the grace period.
func (c *child) stop() {
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(stopGrace):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// forwardLogs re-emits the child's JSON log lines through log at their own level; anything else
// (a panic trace, or a line truncated at maxLogLine) goes through at Warn.
func forwardLogs(r io.Reader, log *slog.Logger) {
	br := bufio.NewReaderSize(r, maxLogLine)
	for {
		line, more, err := br.ReadLine()
		if err != nil {
			return
		}
		logLine(log, line)
		for more && err == nil {
			_, more, err = br.ReadLine() // drop the rest of an over-long line
		}
	}
}

func logLine(log *slog.Logger, line []byte) {
	var rec map[string]any
	if json.Unmarshal(line, &rec) != nil {
		log.Warn("extension stderr", "line", string(line))
		return
	}
	var level slog.Level
	if s, ok := rec["level"].(string); ok {
		_ = level.UnmarshalText([]byte(s))
	}
	msg, _ := rec["msg"].(string)
	delete(rec, "level")
	delete(rec, "msg")
	delete(rec, "time")
	attrs := make([]slog.Attr, 0, len(rec))
	for _, k := range slices.Sorted(maps.Keys(rec)) {
		attrs = append(attrs, slog.Any(k, rec[k]))
	}
	log.LogAttrs(context.Background(), level, msg, attrs...)
}
