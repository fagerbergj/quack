// Package noop proves the register -> route -> dispatch -> run-as-chat loop end to end:
// /dispatch fires a run and /status counts the runs that completed.
package noop

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"
)

func init() {
	sdk.Register("noop", factory)
}

type config struct {
	Greeting string `yaml:"greeting"`
}

func factory(host sdk.Host, raw []byte) (sdk.Extension, error) {
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("noop: parse config: %w", err)
	}
	if cfg.Greeting == "" {
		cfg.Greeting = "noop"
	}

	e := &extension{host: host, greeting: cfg.Greeting}
	// seeded from wall time so ids stay unique across restarts, not just within one process
	e.chatCounter.Store(time.Now().Unix())
	return e, nil
}

type extension struct {
	host     sdk.Host
	greeting string

	chatCounter atomic.Int64
	runEnded    atomic.Int64
}

var (
	_ sdk.Extension   = (*extension)(nil)
	_ sdk.RunObserver = (*extension)(nil)
)

func (e *extension) Tools() []tool.Tool { return nil }

func (e *extension) RegisterRoutes(authed chi.Router, public chi.Router) {
	authed.Get("/status", e.handleStatus)
	authed.Post("/dispatch", e.handleDispatch)
}

// RunEnded counts finished runs, so /status proves the whole loop ran back to this callback.
func (e *extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	e.runEnded.Add(1)
}

type statusResponse struct {
	Extension  string `json:"extension"`
	Greeting   string `json:"greeting"`
	Dispatches int64  `json:"dispatches"`
}

func (e *extension) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statusResponse{
		Extension:  "noop",
		Greeting:   e.greeting,
		Dispatches: e.runEnded.Load(),
	})
}

func (e *extension) handleDispatch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id := e.chatCounter.Add(1)
	req := sdk.DispatchRequest{
		Chat: sdk.ChatRef{
			LocalID: fmt.Sprintf("noop-%d", id),
			Origin: &sdk.ChatOrigin{
				Extension: "noop",
				Label:     "noop test",
				Kind:      "test",
			},
		},
		Ask: sdk.Ask{
			Message: string(body),
		},
	}

	if err := e.host.Dispatch(r.Context(), req); err != nil {
		e.host.Log.Error("noop: dispatch failed", "err", err, "chat_id", req.Chat.LocalID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
