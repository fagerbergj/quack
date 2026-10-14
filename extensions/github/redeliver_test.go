package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFailedDeliveries: only events that never reached quack, inside the window, under the retry cap.
func TestFailedDeliveries(t *testing.T) {
	now := time.Now()
	at := func(minAgo int) time.Time { return now.Add(-time.Duration(minAgo) * time.Minute) }
	ds := []hookDelivery{
		{ID: 1, GUID: "label", DeliveredAt: at(5), StatusCode: 502},                     // dropped label: retry
		{ID: 2, GUID: "ok", DeliveredAt: at(5), StatusCode: 202},                        // reached
		{ID: 3, GUID: "healed", DeliveredAt: at(6), StatusCode: 502},                    // failed once...
		{ID: 4, GUID: "healed", DeliveredAt: at(4), StatusCode: 202, Redelivery: true},  // ...then reached
		{ID: 5, GUID: "rejected", DeliveredAt: at(5), StatusCode: 401},                  // reached and refused: never retry
		{ID: 6, GUID: "stale", DeliveredAt: at(45), StatusCode: 502},                    // outside the window
		{ID: 7, GUID: "capped", DeliveredAt: at(9), StatusCode: 0},                      // never connected
		{ID: 8, GUID: "capped", DeliveredAt: at(8), StatusCode: 502, Redelivery: true},  // retry 1
		{ID: 9, GUID: "capped", DeliveredAt: at(7), StatusCode: 502, Redelivery: true},  // retry 2
		{ID: 10, GUID: "capped", DeliveredAt: at(6), StatusCode: 504, Redelivery: true}, // retry 3: capped
		{ID: 11, GUID: "older", DeliveredAt: at(10), StatusCode: 503},                   // retry, sorted first
	}
	var got []int64
	for _, d := range failedDeliveries(ds, now.Add(-redeliverWindow)) {
		got = append(got, d.ID)
	}
	if want := []int64{11, 1}; !slices.Equal(got, want) {
		t.Fatalf("failedDeliveries = %v, want %v", got, want)
	}
}

// TestRedeliverFailedPostsAttempts: the sweep lists deliveries with the App JWT and posts one attempt per failure.
func TestRedeliverFailedPostsAttempts(t *testing.T) {
	redeliverGap = 0
	keyPEM, _ := testKeyPEM(t)
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("%s %s without an App JWT", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries":
			_ = json.NewEncoder(w).Encode([]hookDelivery{
				{ID: 41, GUID: "a", DeliveredAt: time.Now().Add(-time.Minute), StatusCode: 502},
				{ID: 42, GUID: "b", DeliveredAt: time.Now().Add(-time.Minute), StatusCode: 202},
			})
		case r.Method == http.MethodPost:
			mu.Lock()
			posted = append(posted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	app.SetAPIBase(srv.URL)
	sent := map[string]int{}
	n, err := app.RedeliverFailed(context.Background(), time.Now().Add(-redeliverWindow), sent)
	if err != nil || n != 1 {
		t.Fatalf("RedeliverFailed = %d, %v; want 1, nil", n, err)
	}
	if want := []string{"/app/hook/deliveries/41/attempts"}; !slices.Equal(posted, want) {
		t.Fatalf("posted %v, want %v", posted, want)
	}
	// The fake keeps listing the same failure: the in-process count stops it at the cap.
	for range maxRedeliveries {
		_, _ = app.RedeliverFailed(context.Background(), time.Now().Add(-redeliverWindow), sent)
	}
	if len(posted) != maxRedeliveries {
		t.Fatalf("posted %d attempts for one GUID, want the cap %d", len(posted), maxRedeliveries)
	}
}
