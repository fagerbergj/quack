package extproc

import (
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
	"sync/atomic"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var errNotInitialized = errors.New("not initialized")

// Main is an extension binary's whole main: it serves the registered extension name on stdio.
func Main(name string) {
	factory, ok := sdk.Registered()[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "extproc: extension %q is not registered in this binary\n", name)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)
	out := os.Stdout
	os.Stdout = os.Stderr // a stray fmt.Print must not corrupt the protocol stream
	if err := Serve(os.Stdin, out, factory, log); err != nil {
		log.Error("extproc: serve failed", "err", err)
		os.Exit(1)
	}
}

// Serve answers host calls from in on out until in reaches EOF, then drains in-flight ones.
func Serve(in io.Reader, out io.Writer, factory sdk.Factory, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // after the drain: a Starter's resources outlive every in-flight request
	s := &server{ctx: ctx, factory: factory, log: log}
	s.conn = newConn(out, s.handle, log)
	err := s.conn.read(in)
	s.conn.wg.Wait()
	if err != nil {
		return fmt.Errorf("extproc: read request: %w", err)
	}
	return nil
}

type server struct {
	ctx     context.Context
	factory sdk.Factory
	log     *slog.Logger
	conn    *conn
	ready   atomic.Pointer[mounted] // the host orders initialize first, but the race detector cannot see that
}

// mounted is what initialize built.
type mounted struct {
	handler  http.Handler
	observer sdk.RunObserver
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodInitialize:
		return s.initialize(params)
	case methodHTTP:
		return s.serveHTTP(ctx, params)
	case methodRunEnded:
		return nil, s.runEnded(params)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + method}
}

func (s *server) initialize(raw json.RawMessage) (any, error) {
	var p initParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("protocol %d unsupported, this extension speaks %d", p.Protocol, ProtocolVersion)
	}
	host := sdk.Host{
		Dispatch: func(ctx context.Context, req sdk.DispatchRequest) error {
			return s.conn.call(ctx, methodDispatch, req, nil)
		},
		Log:       s.log,
		DataDir:   p.DataDir,
		Version:   p.Version,
		PublicURL: p.PublicURL,
	}
	if p.Location != "" {
		host.Location, _ = time.LoadLocation(p.Location) // nil on failure: the sdk's "unconfigured" fallback
	}
	ext, err := s.factory(host, []byte(p.Config))
	if err != nil {
		return nil, err
	}
	// Before the routes, as in-process boot does; s.ctx ends with Serve, as quack's ends with the server.
	if st, ok := ext.(sdk.Starter); ok {
		if err := st.Start(s.ctx); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
	}
	// The same shape as quack's own mount, so handlers see the full /<name>/... path they would in-process.
	root, r := mount(p.Name)
	ext.RegisterRoutes(r, r)
	m := &mounted{handler: root}
	m.observer, _ = ext.(sdk.RunObserver)
	s.ready.Store(m)
	res := initResult{Protocol: ProtocolVersion, ObservesRuns: m.observer != nil}
	if ui, ok := ext.(sdk.UI); ok {
		d := ui.UI()
		res.UI = &d
	}
	return res, nil
}

func mount(name string) (http.Handler, chi.Router) {
	root := chi.NewRouter()
	root.Use(middleware.Recoverer)
	ext := chi.NewRouter()
	root.Mount("/"+name, ext)
	return root, ext
}

// serveHTTP runs one request under ctx, which the host cancels when its client goes away.
func (s *server) serveHTTP(ctx context.Context, raw json.RawMessage) (any, error) {
	m := s.ready.Load()
	if m == nil {
		return nil, errNotInitialized
	}
	var p httpParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, bytes.NewReader(p.Body))
	if err != nil {
		return nil, err
	}
	req.RequestURI = p.URL
	if p.Header != nil {
		req.Header = p.Header
	}
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	if n := rec.Body.Len(); n > maxBody {
		return nil, fmt.Errorf("response too large: %d bytes, limit %d", n, maxBody)
	}
	return httpResult{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}, nil
}

func (s *server) runEnded(raw json.RawMessage) error {
	m := s.ready.Load()
	if m == nil {
		return errNotInitialized
	}
	var p runEndedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if m.observer != nil {
		m.observer.RunEnded(p.ChatID, p.Outcome)
	}
	return nil
}
