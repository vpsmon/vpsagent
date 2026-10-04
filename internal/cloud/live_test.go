package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsmon/vpsmonlib/metrics"
)

func TestLiveProcessesRequireRequestAndNeverEnterResourcePayload(t *testing.T) {
	var requested atomic.Bool
	var collections, uploads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Error("missing agent credential")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/v1/metrics":
			if r.Header.Get("X-Vpsagent-Live") != "1" {
				t.Error("missing capability header")
			}
			var resources map[string]json.RawMessage
			_ = json.Unmarshal(payload["metrics"], &resources)
			if _, ok := resources["top_cpu"]; ok {
				t.Error("process details entered resource history")
			}
			if _, ok := payload["incident_snapshot"]; ok {
				t.Error("live view enabled durable incident capture")
			}
			if requested.Load() {
				w.Header().Set("X-Vpscloud-Live", "1")
			}
			w.WriteHeader(204)
		case "/v1/live":
			uploads.Add(1)
			for _, key := range []string{"version", "captured_at", "top_cpu", "top_mem"} {
				if _, ok := payload[key]; !ok {
					t.Errorf("missing live field %s", key)
				}
			}
			if len(payload) != 4 {
				t.Errorf("unexpected live fields: %v", payload)
			}
			// Live failures cannot invalidate an already accepted heartbeat.
			w.WriteHeader(503)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, Token: "agent-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = server.Client()
	c.collectLive = func() ([]metrics.TopProcess, []metrics.TopProcess) {
		collections.Add(1)
		return []metrics.TopProcess{{Name: "private-live-only", CPU: 5, Mem: 2}}, nil
	}
	for _, live := range []bool{false, true, false} {
		requested.Store(live)
		if err := c.Send(context.Background(), metrics.Metrics{Timestamp: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if collections.Load() != 1 || uploads.Load() != 1 {
		t.Fatalf("collections=%d uploads=%d; want one on-demand sample", collections.Load(), uploads.Load())
	}
}
