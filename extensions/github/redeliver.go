package github

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// GitHub never retries an App webhook it failed to connect for, and a push's burst of check_run
// deliveries can take a quack:review label down with it (prod 2026-10-14: 33 of 100 failed in 37 min).
const (
	redeliverEvery  = 2 * time.Minute
	redeliverWindow = 30 * time.Minute // older failures are stale; a missed review is re-requested by hand
	maxRedeliveries = 3
)

var redeliverGap = 2 * time.Second // paced, so the retries are not a burst of their own; tests zero it

type hookDelivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Redelivery  bool      `json:"redelivery"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
	Action      string    `json:"action"`
}

// neverReached: an edge failure (no connection, or a gateway with quack down), so quack never processed it
// and a redelivery cannot double-apply it.
func neverReached(code int) bool { return code == 0 || code == 502 || code == 503 || code == 504 }

// failedDeliveries returns, oldest first, each event (GUID) inside the window whose latest attempt never
// reached quack, with no attempt that did, and fewer than maxRedeliveries retries so far.
func failedDeliveries(ds []hookDelivery, since time.Time) []hookDelivery {
	reached, tries, latest := map[string]bool{}, map[string]int{}, map[string]hookDelivery{}
	for _, d := range ds {
		if !neverReached(d.StatusCode) {
			reached[d.GUID] = true
		}
		if d.Redelivery {
			tries[d.GUID]++
		}
		if l, ok := latest[d.GUID]; !ok || d.DeliveredAt.After(l.DeliveredAt) {
			latest[d.GUID] = d
		}
	}
	var out []hookDelivery
	for guid, d := range latest {
		if !reached[guid] && d.DeliveredAt.After(since) && tries[guid] < maxRedeliveries {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeliveredAt.Before(out[j].DeliveredAt) })
	return out
}

// RedeliverFailed re-sends recent deliveries that never reached quack and returns how many it re-sent.
// ponytail: reads only the newest 100 deliveries; paginate if a 30-minute window ever holds more.
func (a *App) RedeliverFailed(ctx context.Context, since time.Time) (int, error) {
	jwtStr, err := a.appJWT()
	if err != nil {
		return 0, err
	}
	var ds []hookDelivery
	if err := a.doJSON(ctx, http.MethodGet, "/app/hook/deliveries?per_page=100", "Bearer "+jwtStr, nil, &ds); err != nil {
		return 0, err
	}
	n := 0
	for _, d := range failedDeliveries(ds, since) {
		path := fmt.Sprintf("/app/hook/deliveries/%d/attempts", d.ID)
		if err := a.doJSON(ctx, http.MethodPost, path, "Bearer "+jwtStr, nil, nil); err != nil {
			return n, err
		}
		n++
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(redeliverGap):
		}
	}
	return n, nil
}

// redeliverLoop sweeps for failed webhook deliveries until ctx (the extension's lifetime) ends.
func (e *Extension) redeliverLoop(ctx context.Context) {
	t := time.NewTicker(redeliverEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := e.app.RedeliverFailed(ctx, time.Now().Add(-redeliverWindow))
		switch {
		case err != nil && ctx.Err() == nil:
			e.host.Log.Warn("github: webhook redelivery sweep failed", "redelivered", n, "err", err)
		case n > 0:
			e.host.Log.Info("github: redelivered webhook deliveries that never reached quack", "count", n)
		}
	}
}
