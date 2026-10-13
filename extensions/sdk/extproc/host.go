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

const (
	initTimeout = 10 * time.Second
	maxLogLine  = 64 << 10
)

// vars so tests can shrink them. maxBody caps request and response bodies (PiG's RPC command limit).
var (
	stopGrace = 5 * time.Second
	maxBody   = 16 << 20
)

// passEnv is all of quack's environment a child sees: what an outbound HTTP client needs, never secrets.
var passEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// The restart backoff's floor and cap; vars so tests can shrink them.
var restartMin, restartMax = 100 * time.Millisecond, 30 * time.Second

var errExited = errors.New("extension process exited")

// Process is an extension running as a supervised child: started by New, restarted with backoff
// when it exits, stopped by Close. It implements sdk.Extension and sdk.UI for quack's loader.
type Process struct {
	name   string
	path   string
	params initParams
	log    *slog.Logger
	ui     sdk.UIDescriptor

	mu  sync.Mutex
	cur *child

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var (
	_ sdk.Extension = (*Process)(nil)
	_ sdk.UI        = (*Process)(nil)
	_ io.Closer     = (*Process)(nil)
)

// New starts name's extension from the binary at path and initializes it with host's settings and
// the raw extensions.<name> block; an error means the child refused or never answered.
func New(name, path string, host sdk.Host, config []byte) (*Process, error) {
	p := &Process{
		name: name, path: path, log: host.Log,
		params: initParams{Protocol: ProtocolVersion, Name: name, Config: string(config),
			DataDir: host.DataDir, Version: host.Version, PublicURL: host.PublicURL},
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
	if res.UI != nil {
		p.ui = *res.UI
	}
	p.cur = c
	go p.supervise(c)
	return p, nil
}

func (p *Process) Tools() []tool.Tool { return nil }

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
	if err := p.current().call(r.Context(), "http", httpParams{Method: r.Method, URL: r.URL.RequestURI(), Header: h, Body: body}, &res); err != nil {
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
	c, err := startChild(p.path, p.log)
	if err != nil {
		return nil, res, fmt.Errorf("extension %s: start %s: %w", p.name, p.path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	err = c.call(ctx, "initialize", p.params, &res)
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

// child is one running process and its in-flight calls.
type child struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	w       *writer
	started time.Time

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message

	done chan struct{}
	err  error // exit status; read only after done
}

func startChild(path string, log *slog.Logger) (*child, error) {
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
	c := &child{cmd: cmd, stdin: stdin, w: &writer{enc: json.NewEncoder(stdin)}, started: time.Now(),
		pending: map[int64]chan message{}, done: make(chan struct{})}
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
	if err := c.readReplies(stdout); err != nil {
		log.Error("extension process broke the protocol; killing it", "err", err)
		_ = c.cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, stdout) // keep a dying child from blocking on a full pipe
	wg.Wait()
	c.err = c.cmd.Wait()
	close(c.done)
}

// readReplies hands each reply line to its caller until EOF. A line is capped at a maxBody body's
// base64 plus headroom, so an oversized reply costs the child its life rather than quack its memory.
func (c *child) readReplies(stdout io.Reader) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 2*maxBody+64<<10)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return err
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	return sc.Err()
}

func (c *child) call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.w.send(message{ID: id, Method: method, Params: raw}); err != nil {
		return fmt.Errorf("%w: %w", errExited, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		return json.Unmarshal(m.Result, result)
	case <-c.done:
		return errExited
	case <-ctx.Done():
		return ctx.Err()
	}
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
