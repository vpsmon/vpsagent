package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vpsmon/vpsmonlib/metrics"
)

func TestNewRequiresHTTPSAndCredentials(t *testing.T) {
	if _, err := New(Config{URL: "http://cloud.example.com", Token: "token"}); err == nil {
		t.Fatal("accepted an insecure Cloud URL")
	}
	if _, err := New(Config{URL: "http://cloud.example.com", Token: "token", AllowInsecure: true}); err == nil {
		t.Fatal("accepted an insecure non-loopback Cloud URL")
	}
	if _, err := New(Config{URL: "http://127.0.0.1:18080", Token: "token", AllowInsecure: true}); err != nil {
		t.Fatalf("rejected a loopback development Cloud URL: %v", err)
	}
	if _, err := New(Config{URL: "https://cloud.example.com"}); err == nil {
		t.Fatal("accepted an empty Cloud token")
	}
	if _, err := New(Config{URL: "https://cloud.example.com/tenant", Token: "token"}); err != nil {
		t.Fatalf("rejected a valid Cloud config: %v", err)
	}
}

func TestSendUploadsOnlyTelemetry(t *testing.T) {
	var received map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("bad authorization header")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, Token: "agent-token"})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	sample := metrics.Metrics{
		Hostname: "edge-01", CPUUsage: 30, MemPercent: 40, Timestamp: time.Now(),
		TopCPU:     []metrics.TopProcess{{Name: "secret-process"}},
		Containers: []metrics.DockerContainer{{Name: "private-container"}},
		Listeners:  []metrics.ListeningSocket{{Address: "0.0.0.0:8088"}},
	}
	if err := client.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	metricsPayload, ok := received["metrics"].(map[string]any)
	if !ok {
		t.Fatal("missing metrics payload")
	}
	sampleID, ok := received["sample_id"].(string)
	if !ok || len(sampleID) != 32 {
		t.Fatalf("sample_id = %v, want 128-bit hexadecimal request id", received["sample_id"])
	}
	if _, ok := received["sent_at"].(string); !ok {
		t.Fatalf("missing sent_at in Cloud envelope: %#v", received)
	}
	for _, unwanted := range []string{"top_cpu", "top_mem", "containers", "listeners", "gpus"} {
		if _, exists := metricsPayload[unwanted]; exists {
			t.Fatalf("uploaded %s", unwanted)
		}
	}
}

func TestSendReturnsRateLimitAndRetryAfter(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, Token: "agent-token"})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	err = client.Send(context.Background(), metrics.Metrics{Timestamp: time.Now()})
	var responseErr *responseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Send error = %v, want typed HTTP response", err)
	}
	if responseErr.statusCode != http.StatusTooManyRequests || responseErr.retryAfter != 3*time.Second {
		t.Fatalf("response error = %#v", responseErr)
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	var forwarded atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization forwarded to redirect target")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("request body forwarded to redirect target: %d bytes", len(body))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Error("source did not receive token")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := New(Config{URL: source.URL, Token: "agent-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = cloudHTTPClient(10 * time.Second)
	c.httpClient.Transport = source.Client().Transport
	err = c.Send(context.Background(), metrics.Metrics{Timestamp: time.Now()})
	var responseErr *responseError
	if !errors.As(err, &responseErr) || responseErr.statusCode != http.StatusTemporaryRedirect {
		t.Fatalf("Send error = %v, want redirect response", err)
	}
	if forwarded.Load() {
		t.Fatal("request reached redirect target")
	}
}

func TestRunHonorsRetryAfterAndLogsOnlyFailureRecovery(t *testing.T) {
	var logs bytes.Buffer
	client := &Client{interval: 5 * time.Millisecond, logger: log.New(&logs, "", 0)}
	var attempts atomic.Int32
	var firstAttempt time.Time
	secondAttempt := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.run(ctx, func(context.Context) (bool, error) {
			switch n := attempts.Add(1); n {
			case 1:
				firstAttempt = time.Now()
				return true, &responseError{statusCode: http.StatusTooManyRequests, retryAfter: 35 * time.Millisecond}
			case 2:
				secondAttempt <- time.Now()
				return true, nil
			default:
				return false, nil
			}
		})
	}()
	select {
	case second := <-secondAttempt:
		if elapsed := second.Sub(firstAttempt); elapsed < 30*time.Millisecond {
			cancel()
			<-done
			t.Fatalf("retried after %s, want it to honor Retry-After", elapsed)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("uploader did not retry after the rate-limit delay")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upload loop did not stop after cancellation")
	}
	if attempts.Load() < 2 {
		t.Fatalf("attempt count = %d, want a retry", attempts.Load())
	}
	if got := strings.Count(logs.String(), "metrics upload failed"); got != 1 {
		t.Fatalf("failure log count = %d, want 1: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "telemetry uploads recovered"); got != 1 {
		t.Fatalf("recovery log count = %d, want 1: %s", got, logs.String())
	}
}

func TestRunPausesOnRevokedCredentialWithoutRetrying(t *testing.T) {
	var logs bytes.Buffer
	client := &Client{interval: time.Millisecond, logger: log.New(&logs, "", 0)}
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.run(ctx, func(context.Context) (bool, error) {
			attempts.Add(1)
			return true, &responseError{statusCode: http.StatusUnauthorized}
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("uploader did not pause after credential revocation")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts.Load())
	}
	if !strings.Contains(logs.String(), "uploads paused until restart or reconfiguration") {
		t.Fatalf("missing credential guidance: %s", logs.String())
	}
}

func TestRetryDelayIsBoundedAndBillingPauseIsPolled(t *testing.T) {
	if got := retryDelay(10*time.Second, 20, nil); got != maxRetryDelay {
		t.Fatalf("exponential retry delay = %s, want cap %s", got, maxRetryDelay)
	}
	if got := retryDelay(10*time.Second, 1, &responseError{statusCode: http.StatusPaymentRequired}); got != accessPausedRetry {
		t.Fatalf("billing pause retry = %s, want %s", got, accessPausedRetry)
	}
	if got := parseRetryAfter("3600", time.Now()); got != maxRetryDelay {
		t.Fatalf("Retry-After cap = %s, want %s", got, maxRetryDelay)
	}
}

func TestCredentialsAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "cloud.json")
	if err := writeCredentials(path, Credentials{URL: "https://cloud.example.com", Token: "agent-token"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials mode = %o, want 0600", info.Mode().Perm())
	}
	credentials, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Token != "agent-token" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
}

func TestConnectRejectsRedirectWithoutForwardingSetupToken(t *testing.T) {
	var forwarded atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		if r.Header.Get("Authorization") != "" {
			t.Error("setup token forwarded to redirect target")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("claim body forwarded to redirect target: %d bytes", len(body))
		}
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer setup-secret" {
			t.Error("source did not receive setup token")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	destination := filepath.Join(t.TempDir(), "credentials.json")
	err := Connect(context.Background(), source.URL, "setup-secret", destination, true)
	if err == nil || forwarded.Load() {
		t.Fatalf("Connect err = %v, forwarded = %v", err, forwarded.Load())
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("credentials written after redirect: %v", err)
	}
}

func TestConnectRejectsOversizedAndMalformedClaimBodies(t *testing.T) {
	cases := map[string]string{
		"unknown oversized field": `{"token":"server-token","unused":"` + strings.Repeat("x", maxClaimResponseBytes) + `"}`,
		"trailing JSON":           `{"token":"server-token"} {}`,
		"invalid token":           `{"token":"bad token"}`,
		"oversized token":         `{"token":"` + strings.Repeat("x", maxCloudTokenBytes+1) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "credentials.json")
			if err := Connect(context.Background(), server.URL, "setup-secret", destination, true); err == nil {
				t.Fatal("Connect accepted invalid claim response")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatalf("credentials written for invalid response: %v", err)
			}
		})
	}
}

func TestIncidentSnapshotIsOptInAndOnlySentOnThresholdCrossing(t *testing.T) {
	var payloads []map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		payloads = append(payloads, payload)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, Token: "agent-token", EnableSnapshots: true})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient = server.Client()
	high := metrics.Metrics{Timestamp: time.Now(), CPUUsage: 96, MemPercent: 40, TopCPU: []metrics.TopProcess{{Name: "busy", CPU: 96}}, Containers: []metrics.DockerContainer{{Name: "app"}}}
	low := metrics.Metrics{Timestamp: time.Now(), CPUUsage: 12, MemPercent: 40}
	for _, sample := range []metrics.Metrics{high, high, low, high} {
		if err := client.Send(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
	}
	if len(payloads) != 4 {
		t.Fatalf("payload count = %d", len(payloads))
	}
	if _, ok := payloads[0]["incident_snapshot"]; !ok {
		t.Fatal("first threshold crossing did not include snapshot")
	}
	if _, ok := payloads[1]["incident_snapshot"]; ok {
		t.Fatal("repeated high metric included another snapshot")
	}
	if _, ok := payloads[2]["incident_snapshot"]; ok {
		t.Fatal("recovery metric included a snapshot")
	}
	if _, ok := payloads[3]["incident_snapshot"]; !ok {
		t.Fatal("new threshold crossing did not include snapshot")
	}
}
