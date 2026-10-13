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
	"slices"
	"sync/atomic"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var errNotInitialized = errors.New("not initialized")

// Main is an extension binary's whole main: it serves the registered extension name on stdio. needs are the
// Host callbacks it cannot run without: initialize refuses a quack that does not serve one rather than leave it nil.
func Main(name string, needs ...Callback) {
	factory, ok := sdk.Registered()[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "extproc: extension %q is not registered in this binary\n", name)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)
	out := os.Stdout
	os.Stdout = os.Stderr // a stray fmt.Print must not corrupt the protocol stream
	if err := Serve(os.Stdin, out, factory, log, needs...); err != nil {
		log.Error("extproc: serve failed", "err", err)
		os.Exit(1)
	}
}

// Serve answers host calls from in on out until in reaches EOF, then drains in-flight ones.
func Serve(in io.Reader, out io.Writer, factory sdk.Factory, log *slog.Logger, needs ...Callback) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // after the drain: a Starter's resources outlive every in-flight request
	s := &server{ctx: ctx, factory: factory, log: log, needs: needs}
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
	needs   []Callback
	conn    *conn
	ready   atomic.Pointer[mounted] // the host orders initialize first, but the race detector cannot see that
}

// mounted is what initialize built.
type mounted struct {
	ext      sdk.Extension
	handler  http.Handler // the authed routes
	public   http.Handler
	observer sdk.RunObserver
	tools    map[string]functionTool
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodInitialize:
		return s.initialize(params)
	case methodHTTP, methodPublicHTTP:
		return s.serveHTTP(ctx, params, method == methodPublicHTTP)
	case methodTool:
		return s.callTool(ctx, params)
	case methodRunEnded:
		return nil, s.runEnded(params)
	case methodDeliver:
		return s.deliver(ctx, params)
	case methodGitCredential:
		return s.gitCredential(ctx, params)
	case methodOnAssignment:
		return s.onAssignment(ctx, params)
	case methodBeforeAssignment:
		return s.beforeAssignment(ctx, params)
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
	for _, n := range s.needs {
		if !slices.Contains(p.Serves, string(n)) {
			return nil, fmt.Errorf("this quack does not serve the %s callback this extension needs; upgrade it or drop exec", n)
		}
	}
	ext, err := s.factory(s.hostFor(p), []byte(p.Config))
	if err != nil {
		return nil, err
	}
	m, res, err := describe(ext, p.Serves)
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
	root, authed := mount(p.Name)
	publicRoot, public := mount(p.Name)
	ext.RegisterRoutes(authed, public)
	if res.PublicRoutes, err = publicRoutes(public, p.Serves); err != nil {
		return nil, err
	}
	m.handler, m.public = root, publicRoot
	s.ready.Store(m)
	return res, nil
}

// publicRoutes lists what r mounted, for quack to serve without its session auth.
func publicRoutes(r chi.Routes, serves []string) ([]routeSpec, error) {
	var routes []routeSpec
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, routeSpec{Method: method, Pattern: route})
		return nil
	})
	if err == nil && len(routes) > 0 && !slices.Contains(serves, methodPublicHTTP) {
		err = errors.New("this quack serves no public routes from a child; upgrade it or drop exec")
	}
	return routes, err
}

// hostFor is the child's sdk.Host: settings from p, and a call into quack for each callback quack serves.
func (s *server) hostFor(p initParams) sdk.Host {
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
	served := func(m string) bool { return slices.Contains(p.Serves, m) }
	if served(methodReadArtifact) {
		host.ReadArtifact = s.readArtifact
	}
	if served(methodWriteArtifact) {
		host.WriteArtifact = s.writeArtifact
	}
	if served(methodChatUser) {
		host.ChatUser = s.chatUser
	}
	if served(methodArchiveChat) {
		host.ArchiveChat = func(chatID string) error {
			return s.conn.call(s.ctx, methodArchiveChat, chatParams{ChatID: chatID}, nil)
		}
	}
	if served(methodInvalidateSetup) {
		host.InvalidateSetup = func(chatID string) error {
			return s.conn.call(s.ctx, methodInvalidateSetup, chatParams{ChatID: chatID}, nil)
		}
	}
	if served(methodUpdateChatOrigin) {
		host.UpdateChatOrigin = func(localID string, origin sdk.ChatOrigin) error {
			return s.conn.call(s.ctx, methodUpdateChatOrigin, updateChatOriginParams{LocalID: localID, Origin: origin}, nil)
		}
	}
	if served(methodClassify) {
		host.Classify = func(ctx context.Context, prompt string) (out string, err error) {
			return out, s.conn.call(ctx, methodClassify, classifyParams{Prompt: prompt}, &out)
		}
	}
	if served(methodDecide) {
		host.Decide = func(ctx context.Context, req sdk.DecideRequest) (d sdk.Decision, err error) {
			return d, s.conn.call(ctx, methodDecide, req, &d)
		}
	}
	return host
}

// readArtifact has no ctx to cancel by; a failed call reads as absent, as quack's own read failure does.
func (s *server) readArtifact(chatID, user, name string) ([]byte, bool) {
	var res readArtifactResult
	if err := s.conn.call(s.ctx, methodReadArtifact, readArtifactParams{ChatID: chatID, User: user, Name: name}, &res); err != nil {
		s.log.Warn("extproc: readArtifact failed", "chat", chatID, "artifact", name, "err", err)
		return nil, false
	}
	return res.Data, res.OK
}

func (s *server) writeArtifact(chatID, user, name, mimeType string, data []byte) (int64, bool, error) {
	var res writeArtifactResult
	err := s.conn.call(s.ctx, methodWriteArtifact, writeArtifactParams{ChatID: chatID, User: user, Name: name, MIMEType: mimeType, Data: data}, &res)
	return res.Revision, res.Changed, err
}

// chatUser reads a failed call as an unknown chat, the only miss the in-process func reports.
func (s *server) chatUser(chatID string) (string, bool) {
	var res chatUserResult
	if err := s.conn.call(s.ctx, methodChatUser, chatParams{ChatID: chatID}, &res); err != nil {
		s.log.Warn("extproc: chatUser failed", "chat", chatID, "err", err)
		return "", false
	}
	return res.User, res.OK
}

// inverses pairs each sdk interface quack calls into with the method that carries it.
var inverses = []struct {
	iface, method string
	is            func(sdk.Extension) bool
}{
	{"Deliverer", methodDeliver, is[sdk.Deliverer]},
	{"GitCredentialSource", methodGitCredential, is[sdk.GitCredentialSource]},
	{"AssignmentMetaExtension", methodOnAssignment, is[sdk.AssignmentMetaExtension]},
	{"AssignmentFreshnessChecker", methodBeforeAssignment, is[sdk.AssignmentFreshnessChecker]},
}

func is[I any](ext sdk.Extension) bool {
	_, ok := any(ext).(I)
	return ok
}

// implements names the inverses ext answers, refusing one this quack would never call.
func implements(ext sdk.Extension, serves []string) ([]string, error) {
	var out []string
	for _, in := range inverses {
		if !in.is(ext) {
			continue
		}
		if !slices.Contains(serves, in.method) {
			return nil, fmt.Errorf("this quack does not call %s on a child; upgrade it or drop exec", in.iface)
		}
		out = append(out, in.iface)
	}
	return out, nil
}

// describe reports what quack's loader reads off an in-process extension. A host that serves no tool
// calls predates all of it, so a child that has any refuses rather than run without it.
func describe(ext sdk.Extension, serves []string) (*mounted, initResult, error) {
	m := &mounted{ext: ext}
	res := initResult{Protocol: ProtocolVersion}
	var err error
	if m.tools, res.Tools, err = describeTools(ext.Tools()); err != nil {
		return nil, res, err
	}
	if as, ok := ext.(sdk.ArtifactSchemas); ok {
		res.ArtifactSchemas = as.ArtifactSchemas()
	}
	if dp, ok := ext.(sdk.DecisionPoints); ok {
		res.DecisionPoints = dp.DecisionPoints()
	}
	if !slices.Contains(serves, methodTool) && (len(res.Tools) > 0 || len(res.ArtifactSchemas) > 0 || len(res.DecisionPoints) > 0) {
		return nil, res, errors.New("this quack takes no tools, artifact schemas or decision points from a child; upgrade it or drop exec")
	}
	if res.Implements, err = implements(ext, serves); err != nil {
		return nil, res, err
	}
	m.observer, _ = ext.(sdk.RunObserver)
	res.ObservesRuns = m.observer != nil
	if ui, ok := ext.(sdk.UI); ok {
		d := ui.UI()
		res.UI = &d
	}
	return m, res, nil
}

func (s *server) callTool(ctx context.Context, raw json.RawMessage) (any, error) {
	m := s.ready.Load()
	if m == nil {
		return nil, errNotInitialized
	}
	var p toolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	t, ok := m.tools[p.Name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", p.Name)
	}
	return runTool(ctx, t, p)
}

func mount(name string) (http.Handler, chi.Router) {
	root := chi.NewRouter()
	root.Use(middleware.Recoverer)
	ext := chi.NewRouter()
	root.Mount("/"+name, ext)
	return root, ext
}

// serveHTTP runs one request under ctx, which the host cancels when its client goes away; public picks the
// routes the extension mounted on its public router.
func (s *server) serveHTTP(ctx context.Context, raw json.RawMessage, public bool) (any, error) {
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
	h := m.handler
	if public {
		h = m.public
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
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

// inverseCall finds the mounted extension's I and decodes raw into P, for one call quack makes into an sdk interface.
func inverseCall[I, P any](s *server, raw json.RawMessage) (impl I, p P, err error) {
	m := s.ready.Load()
	if m == nil {
		return impl, p, errNotInitialized
	}
	impl, ok := any(m.ext).(I)
	if !ok {
		return impl, p, fmt.Errorf("extension does not implement %T", (*I)(nil))
	}
	return impl, p, json.Unmarshal(raw, &p)
}

func (s *server) deliver(ctx context.Context, raw json.RawMessage) (any, error) {
	d, dc, err := inverseCall[sdk.Deliverer, sdk.DeliveryContext](s, raw)
	if err != nil {
		return nil, err
	}
	outcomes, err := d.Deliver(ctx, dc)
	res := deliverResult{Outcomes: outcomes}
	if err != nil {
		res.Error = err.Error()
	}
	return res, nil
}

func (s *server) gitCredential(ctx context.Context, raw json.RawMessage) (any, error) {
	src, p, err := inverseCall[sdk.GitCredentialSource, gitCredentialParams](s, raw)
	if err != nil {
		return nil, err
	}
	return src.GitCredential(ctx, p.URL)
}

func (s *server) onAssignment(ctx context.Context, raw json.RawMessage) (any, error) {
	x, p, err := inverseCall[sdk.AssignmentMetaExtension, assignmentParams](s, raw)
	if err != nil {
		return nil, err
	}
	return x.OnAssignment(withSession(ctx, p.SessionID), p.Assignment), nil
}

func (s *server) beforeAssignment(ctx context.Context, raw json.RawMessage) (any, error) {
	x, p, err := inverseCall[sdk.AssignmentFreshnessChecker, assignmentParams](s, raw)
	if err != nil {
		return nil, err
	}
	fresh, reason := x.BeforeAssignment(ctx, p.Assignment)
	return freshnessResult{Fresh: fresh, Reason: reason}, nil
}

// sessionCtx has the SessionID method of the agent.Context quack hands OnAssignment, which github asserts for.
type sessionCtx struct {
	context.Context
	id string
}

func (c sessionCtx) SessionID() string { return c.id }

func withSession(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return sessionCtx{ctx, id}
}
