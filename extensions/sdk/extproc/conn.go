package extproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/fagerbergj/quack/extensions/sdk"
)

// errClosed fails a call once the peer's stream ended: the child exited, or quack closed its stdin.
var errClosed = errors.New("extproc: connection closed")

// handlerFunc serves one inbound request, or a notification (whose result is dropped).
type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

// conn is one end of the stdio link, the same on both sides: it matches replies to its own calls and
// serves the peer's calls on their own goroutines, so either side may call the other mid-request.
type conn struct {
	w      *writer
	handle handlerFunc
	log    *slog.Logger

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan message
	inflight map[int64]context.CancelFunc

	wg     sync.WaitGroup // inbound requests and notifications being served
	closed chan struct{}  // read returned
}

func newConn(out io.Writer, handle handlerFunc, log *slog.Logger) *conn {
	return &conn{w: &writer{enc: json.NewEncoder(out)}, handle: handle, log: log,
		pending: map[int64]chan message{}, inflight: map[int64]context.CancelFunc{}, closed: make(chan struct{})}
}

// read routes in's lines until EOF, then fails every pending call. A line is capped at a maxBody
// body's base64 plus headroom, so an oversized message costs the sender its link, not us our memory.
func (c *conn) read(in io.Reader) error {
	defer close(c.closed)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 2*maxBody+64<<10)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return err
		}
		c.route(m)
	}
	return sc.Err()
}

func (c *conn) route(m message) {
	switch m.Method {
	case "":
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	case methodCancel:
		var p cancelParams
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		cancel := c.inflight[p.ID]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	default:
		c.serve(m)
	}
}

// serve registers a request's cancel before its goroutine starts, so a cancel read next always finds it.
// ponytail: notifications run concurrently and unordered; queue them per conn if an observer needs order.
func (c *conn) serve(m message) {
	ctx, cancel := context.WithCancel(context.Background())
	if m.ID != 0 {
		c.mu.Lock()
		c.inflight[m.ID] = cancel
		c.mu.Unlock()
	}
	c.wg.Go(func() {
		res, err := c.handle(ctx, m.Method, m.Params)
		c.mu.Lock()
		delete(c.inflight, m.ID)
		c.mu.Unlock()
		cancel()
		if m.ID != 0 {
			c.reply(m.ID, res, err)
		} else if err != nil {
			c.log.Warn("extproc: notification failed", "method", m.Method, "err", err)
		}
	})
}

func (c *conn) reply(id int64, result any, err error) {
	m := message{ID: id}
	if err == nil {
		m.Result, err = json.Marshal(result)
	}
	if err != nil {
		m.Result, m.Error = nil, toRPCError(err)
	}
	if err := c.w.send(m); err != nil {
		c.log.Error("extproc: write response", "err", err)
	}
}

func toRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	code := codeServerError
	if errors.Is(err, sdk.ErrRunInFlight) {
		code = codeRunInFlight
	}
	return &rpcError{Code: code, Message: err.Error()}
}

// call sends one request and waits for its reply; giving up on ctx tells the peer to cancel it.
// A nil result discards the reply's.
func (c *conn) call(ctx context.Context, method string, params, result any) error {
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
		return fmt.Errorf("%w: %w", errClosed, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	case <-c.closed:
		return errClosed
	case <-ctx.Done():
		_ = c.notify(methodCancel, cancelParams{ID: id})
		return ctx.Err()
	}
}

func (c *conn) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.w.send(message{Method: method, Params: raw})
}
