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
	"sync"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// errNoDispatch: phase 1 carries no host callbacks; sdk.Host promises a non-nil Dispatch.
var errNoDispatch = errors.New("extproc: Host.Dispatch is not available to an out-of-process extension yet")

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

// Serve answers host requests from in on out until in reaches EOF, then drains in-flight requests.
func Serve(in io.Reader, out io.Writer, factory sdk.Factory, log *slog.Logger) error {
	s := &server{factory: factory, log: log, w: &writer{enc: json.NewEncoder(out)}}
	var wg sync.WaitGroup
	defer wg.Wait()
	dec := json.NewDecoder(in)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("extproc: read request: %w", err)
		}
		// initialize runs inline so no http request can observe a half-built handler.
		if m.Method == "initialize" {
			res, err := s.initialize(m.Params)
			s.reply(m.ID, res, err)
			continue
		}
		wg.Go(func() {
			res, err := s.handle(m)
			s.reply(m.ID, res, err)
		})
	}
}

type server struct {
	factory sdk.Factory
	log     *slog.Logger
	w       *writer
	handler http.Handler
}

func (s *server) reply(id int64, result any, err error) {
	m := message{ID: id}
	if err == nil {
		m.Result, err = json.Marshal(result)
	}
	if err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			re = &rpcError{Code: codeServerError, Message: err.Error()}
		}
		m.Result, m.Error = nil, re
	}
	if err := s.w.send(m); err != nil {
		s.log.Error("extproc: write response", "err", err)
	}
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
		Dispatch:  func(context.Context, sdk.DispatchRequest) error { return errNoDispatch },
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
	// The same shape as quack's own mount, so handlers see the full /<name>/... path they would in-process.
	r := s.mount(p.Name)
	ext.RegisterRoutes(r, r)
	res := initResult{Protocol: ProtocolVersion}
	if ui, ok := ext.(sdk.UI); ok {
		d := ui.UI()
		res.UI = &d
	}
	return res, nil
}

func (s *server) mount(name string) chi.Router {
	root := chi.NewRouter()
	root.Use(middleware.Recoverer)
	ext := chi.NewRouter()
	root.Mount("/"+name, ext)
	s.handler = root
	return ext
}

func (s *server) handle(m message) (any, error) {
	if m.Method != "http" {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown method " + m.Method}
	}
	if s.handler == nil {
		return nil, errors.New("not initialized")
	}
	var p httpParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(p.Method, p.URL, bytes.NewReader(p.Body))
	if err != nil {
		return nil, err
	}
	req.RequestURI = p.URL
	if p.Header != nil {
		req.Header = p.Header
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return httpResult{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}, nil
}
