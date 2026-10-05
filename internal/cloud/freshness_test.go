package cloud

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsmon/vpsmonlib/metrics"
)

func TestSendRejectsOldMissingAndFutureCollectionsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer provider.Close()
	client, err := New(Config{URL: provider.URL, Token: "test"})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = provider.Client()
	for _, stamp := range []time.Time{{}, time.Now().Add(-2 * time.Minute), time.Now().Add(time.Minute)} {
		if err := client.Send(t.Context(), metrics.Metrics{Timestamp: stamp}); err == nil {
			t.Fatalf("accepted %s", stamp)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid sample reached provider")
	}
}

func TestUploaderSkipsFrozenSamplesAndRecoversAfterConflict(t *testing.T) {
	var calls atomic.Int32
	received := make(chan struct{}, 8)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		received <- struct{}{}
	}))
	defer provider.Close()
	client, err := New(Config{URL: provider.URL, Token: "test", Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = provider.Client()
	client.logger = log.New(io.Discard, "", 0)
	var stamp atomic.Pointer[time.Time]
	fresh := time.Now()
	stamp.Store(&fresh)
	client.latest = func() (metrics.Metrics, bool) { return metrics.Metrics{Timestamp: *stamp.Load()}, true }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.Start(ctx)
	for range 2 {
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("conflict permanently stopped uploader")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("frozen sample uploaded repeatedly: %d", calls.Load())
	}
	old := time.Now().Add(-2 * time.Minute)
	stamp.Store(&old)
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatal("stale collection uploaded")
	}
	next := time.Now()
	stamp.Store(&next)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("fresh collection did not recover")
	}
}
