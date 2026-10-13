// Package extproc runs an sdk extension as a child of quack: newline-delimited JSON-RPC 2.0 on stdio,
// calls in both directions, JSON log lines on stderr, and stdin EOF as shutdown.
package extproc

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/fagerbergj/quack/extensions/sdk"
)

// ProtocolVersion is bumped on any incompatible wire change; initialize refuses a mismatch.
const ProtocolVersion = 2

// Methods: the host calls initialize and http and notifies runEnded; the child calls dispatch;
// either side notifies cancel for a call it gave up on.
const (
	methodInitialize = "initialize"
	methodHTTP       = "http"
	methodRunEnded   = "runEnded"
	methodDispatch   = "dispatch"
	methodCancel     = "cancel"
)

// message is a JSON-RPC 2.0 request, notification (ID 0: ids start at 1) or response (no Method).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Unwrap keeps errors.Is(err, sdk.ErrRunInFlight) true on the far side of the wire.
func (e *rpcError) Unwrap() error {
	if e.Code == codeRunInFlight {
		return sdk.ErrRunInFlight
	}
	return nil
}

const (
	codeMethodNotFound = -32601
	codeServerError    = -32000
	codeRunInFlight    = -32001
)

// initParams carries what the in-process path puts in sdk.Host, plus the raw extensions.<name> block.
type initParams struct {
	Protocol  int    `json:"protocol"`
	Name      string `json:"name"`
	Config    string `json:"config"`
	DataDir   string `json:"dataDir"`
	Version   string `json:"version"`
	PublicURL string `json:"publicURL"`
	Location  string `json:"location,omitempty"`
}

type initResult struct {
	Protocol     int               `json:"protocol"`
	UI           *sdk.UIDescriptor `json:"ui,omitempty"`
	ObservesRuns bool              `json:"observesRuns,omitempty"`
}

type runEndedParams struct {
	ChatID  string         `json:"chatID"`
	Outcome sdk.RunOutcome `json:"outcome"`
}

type cancelParams struct {
	ID int64 `json:"id"`
}

// httpParams is one request quack already authenticated; URL is the full request URI (/<name>/...).
type httpParams struct {
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

type httpResult struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// writer serializes messages onto one stream; json.Encoder ends each with the newline framing needs.
type writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *writer) send(m message) error {
	m.JSONRPC = "2.0"
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(m)
}
