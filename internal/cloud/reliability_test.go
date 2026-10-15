package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsmon/vpsmonlib/metrics"
)

func readySample() metrics.Metrics {
	return metrics.Metrics{Timestamp: time.Now(), CPUCount: 2, MemTotal: 1024, DiskCollectionStatus: metrics.DiskCollectionUnavailable}
}

func TestUnavailableDisksRemainAnArrayAndUseCompatibleHeader(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metrics map[string]json.RawMessage `json:"metrics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body.Metrics["disks"]) != "[]" {
			t.Errorf("disks = %s", body.Metrics["disks"])
		}
		if _, exists := body.Metrics["disk_collection_status"]; exists {
			t.Error("new JSON field would break older Cloud servers")
		}
		if r.Header.Get("X-Vpsagent-Disk-Status") != "unavailable" {
			t.Error("missing disk availability")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, Token: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = server.Client()
	if err := c.Send(t.Context(), readySample()); err != nil {
		t.Fatal(err)
	}
}

func TestRunContinuesAfterRejectedSamples(t *testing.T) {
	for _, status := range []int{400, 404, 413, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := &Client{interval: time.Millisecond, logger: log.New(io.Discard, "", 0)}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			attempts := 0
			c.run(ctx, func(context.Context) (bool, error) {
				attempts++
				if attempts == 1 {
					return true, &responseError{statusCode: status}
				}
				cancel()
				return true, nil
			})
			if attempts != 2 {
				t.Fatalf("uploader stopped after %d attempts", attempts)
			}
		})
	}
}

func TestRejectedFrozenSampleIsSkippedAndNextSampleRecovers(t *testing.T) {
	var calls atomic.Int32
	received := make(chan struct{}, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(204)
		}
		received <- struct{}{}
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, Token: "test", Interval: 3 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = server.Client()
	c.logger = log.New(io.Discard, "", 0)
	var latest atomic.Pointer[metrics.Metrics]
	first := readySample()
	latest.Store(&first)
	c.latest = func() (metrics.Metrics, bool) { return *latest.Load(), true }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.Start(ctx)
	wait := func() {
		t.Helper()
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("upload did not progress")
		}
	}
	wait()
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("replayed a rejected frozen measurement")
	}
	next := readySample()
	latest.Store(&next)
	wait()
}

func TestSnapshotBoundsAndFallbackPreserveResourceUploads(t *testing.T) {
	for _, mode := range []string{"count-and-fields", "byte-budget", "server-rejection"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			var firstID string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if len(data) > maxTelemetryBytes {
					t.Error("oversized request reached Cloud")
				}
				var body envelope
				if err := json.Unmarshal(data, &body); err != nil {
					t.Error(err)
				}
				if calls == 1 {
					firstID = body.SampleID
				} else if body.SampleID != firstID {
					t.Error("fallback changed sample identity")
				}
				if mode == "byte-budget" || calls > 1 {
					if body.IncidentSnapshot != nil {
						t.Error("resource fallback still contains a snapshot")
					}
				} else if s := body.IncidentSnapshot; s == nil || len(s.Containers) != 100 || len(s.TopCPU) != 1 || len(s.TopCPU[0].Name) > 255 {
					t.Errorf("invalid bounded snapshot: %+v", s)
				}
				if mode == "server-rejection" && calls == 1 {
					w.WriteHeader(400)
				} else {
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			c, err := New(Config{URL: server.URL, Token: "test", EnableSnapshots: true})
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient = server.Client()
			s := readySample()
			s.CPUUsage = 95
			s.TopCPU = []metrics.TopProcess{{Name: strings.Repeat("é", 200)}, {CPU: math.NaN()}}
			s.Containers = make([]metrics.DockerContainer, 101)
			if mode == "byte-budget" {
				for i := range s.Containers {
					s.Containers[i].Image = strings.Repeat("\x00", 1024)
					s.Containers[i].Ports = strings.Repeat("\x00", 1024)
				}
			}
			if err := c.Send(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			wantCalls := 1
			if mode == "server-rejection" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("requests=%d want=%d", calls, wantCalls)
			}
			select {
			case <-c.Ready():
			default:
				t.Error("accepted fallback didn't become ready")
			}
		})
	}
}

type failedTransport struct{}

func (failedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network unavailable")
}

func TestReadinessRequiresFreshCoreCollectionAndWorkingUploader(t *testing.T) {
	for _, status := range []int{204, 402, 409, 429, 503, 400, 401, 413, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			c, err := New(Config{URL: server.URL, Token: "test"})
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient = server.Client()
			_ = c.Send(t.Context(), readySample())
			ready := false
			select {
			case <-c.Ready():
				ready = true
			default:
			}
			want := status == 204 || status == 402 || status == 409 || status == 429 || status == 503
			if ready != want {
				t.Fatalf("ready=%t want=%t", ready, want)
			}
		})
	}
	for _, mode := range []string{"outage", "missing-memory", "stale", "invalid-numeric", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			c, err := New(Config{URL: "https://cloud.example", Token: "test"})
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient = &http.Client{Transport: failedTransport{}}
			s := readySample()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "missing-memory":
				s.MemTotal = 0
			case "stale":
				s.Timestamp = time.Now().Add(-2 * time.Minute)
			case "invalid-numeric":
				s.CPUUsage = math.NaN()
			case "cancelled":
				cancel()
			}
			_ = c.Send(ctx, s)
			ready := false
			select {
			case <-c.Ready():
				ready = true
			default:
			}
			if ready != (mode == "outage") {
				t.Fatalf("ready=%t for %s", ready, mode)
			}
		})
	}
}
