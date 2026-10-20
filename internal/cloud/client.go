// Package cloud sends optional, privacy-conscious telemetry to VPSmon Cloud.
// It never participates in local collection or the local dashboard.
package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vpsmon/vpsmonlib/metrics"
)

const DefaultInterval = 15 * time.Second

const (
	maxRetryDelay     = 5 * time.Minute
	accessPausedRetry = 5 * time.Minute
	maxSampleAge      = time.Minute
)

// Config contains credentials scoped to one server, never a Cloud user.
type Config struct {
	URL               string
	Token             string
	Interval          time.Duration
	AllowInsecure     bool
	EnableSnapshots   bool
	SnapshotThreshold float64
}

type Client struct {
	metricsURL     string
	liveURL        string
	controlURL     string
	token          string
	interval       time.Duration
	httpClient     *http.Client
	logger         *log.Logger
	snapshots      bool
	threshold      float64
	snapshotMu     sync.Mutex
	snapshotActive map[string]bool
	collectLive    func() ([]metrics.TopProcess, []metrics.TopProcess)
	latest         func() (metrics.Metrics, bool)
	ready          chan struct{}
	readyOnce      sync.Once
}

// New validates an enabled Cloud configuration. HTTP is only permitted for an
// explicitly enabled loopback-only development instance.
func New(config Config) (*Client, error) {
	endpoint, err := parseCloudURL(config.URL, config.AllowInsecure)
	if err != nil {
		return nil, err
	}
	if !validCloudToken(config.Token) {
		return nil, fmt.Errorf("VPSAGENT_CLOUD_TOKEN is required when Cloud is enabled")
	}
	basePath := strings.TrimRight(endpoint.Path, "/")
	endpoint.Path = basePath + "/v1/live"
	endpoint.RawQuery = ""
	liveURL := endpoint.String()
	endpoint.Path = basePath + "/v1/agent/control"
	controlURL := endpoint.String()
	endpoint.Path = basePath + "/v1/metrics"
	if config.Interval <= 0 {
		config.Interval = DefaultInterval
	}
	if config.SnapshotThreshold <= 0 {
		config.SnapshotThreshold = 90
	}
	return &Client{
		metricsURL: endpoint.String(), liveURL: liveURL, controlURL: controlURL, token: config.Token, interval: config.Interval,
		httpClient: cloudHTTPClient(10 * time.Second), logger: log.Default(),
		snapshots: config.EnableSnapshots, threshold: config.SnapshotThreshold, snapshotActive: map[string]bool{},
		collectLive: metrics.CollectProcessDetails,
		latest:      metrics.GetLatest,
		ready:       make(chan struct{}),
	}, nil
}

// cloudHTTPClient never follows redirects. Cloud requests carry scoped bearer
// credentials and telemetry, so even same-host redirects could downgrade TLS
// or forward those values to an unintended endpoint.
func cloudHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func parseCloudURL(raw string, allowInsecure bool) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("VPSAGENT_CLOUD_URL must be an absolute https URL")
	}
	if endpoint.Scheme == "https" {
		return endpoint, nil
	}
	if endpoint.Scheme == "http" && allowInsecure && isLoopbackHost(endpoint.Hostname()) {
		return endpoint, nil
	}
	return nil, fmt.Errorf("VPSAGENT_CLOUD_URL must be an absolute https URL (HTTP is allowed only for loopback development with VPSAGENT_ALLOW_INSECURE=true)")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Start uploads a completed local sample every interval. Transient failures use
// bounded backoff; this optional goroutine never blocks local monitoring.
func (c *Client) Start(ctx context.Context) {
	var lastUploaded time.Time
	var stale bool
	go c.run(ctx, func(ctx context.Context) (bool, error) {
		sample, ok := c.latest()
		if !ok {
			return false, nil
		}
		if !freshSample(sample, time.Now()) {
			if !stale {
				c.logger.Print("vpsagent: collection is stale; waiting for a fresh sample")
			}
			stale = true
			return false, nil
		}
		if !sample.Timestamp.After(lastUploaded) {
			return false, nil
		}
		if stale {
			c.logger.Print("vpsagent: fresh collection resumed")
			stale = false
		}
		err := c.Send(ctx, sample)
		if err == nil || sampleRejected(err) {
			lastUploaded = sample.Timestamp
		}
		return true, err
	})
}

// run owns only the optional upload loop. Permanent credential errors
// stop this goroutine, never the local collector or dashboard.
func (c *Client) run(ctx context.Context, send func(context.Context) (bool, error)) {
	interval := c.interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var failures int
	var nextAttempt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-ticker.C:
			if tick.Before(nextAttempt) {
				continue
			}
			attempted, err := send(ctx)
			if !attempted {
				continue
			}
			if err == nil {
				if failures > 0 {
					c.logger.Printf("vpsagent: telemetry uploads recovered")
				}
				failures = 0
				nextAttempt = time.Time{}
				continue
			}
			if ctx.Err() != nil {
				return
			}

			var statusErr *responseError
			if errors.As(err, &statusErr) {
				switch {
				case statusErr.statusCode == http.StatusUnauthorized || statusErr.statusCode == http.StatusForbidden:
					c.logger.Printf("vpsagent: credential rejected (HTTP %d); uploads paused until restart or reconfiguration", statusErr.statusCode)
					return
				case statusErr.statusCode == http.StatusPaymentRequired:
					if failures == 0 {
						c.logger.Printf("vpsagent: account access is paused; checking again in %s", accessPausedRetry)
					}
					failures++
					nextAttempt = time.Now().Add(retryDelay(interval, failures, statusErr))
					continue
				}
			}
			if failures == 0 {
				c.logger.Printf("vpsagent: metrics upload failed: %v", err)
			}
			if sampleRejected(err) {
				// The next sample can be valid even when this one was not.
				// Do not let repeated rejected measurements build a long
				// transport backoff that delays newly recovered collection.
				failures = 1
				nextAttempt = time.Now().Add(interval)
				continue
			}
			failures++
			delay := retryDelay(interval, failures, statusErr)
			nextAttempt = time.Now().Add(delay)
		}
	}
}

type responseError struct {
	statusCode int
	retryAfter time.Duration
}

// A malformed measurement cannot improve when replayed. The next completed
// measurement is still eligible, and the run loop retains its bounded backoff.
func sampleRejected(err error) bool {
	var status *responseError
	return errors.As(err, &status) && (status.statusCode == http.StatusBadRequest || status.statusCode == http.StatusConflict || status.statusCode == http.StatusRequestEntityTooLarge || status.statusCode == http.StatusUnprocessableEntity)
}

func (e *responseError) Error() string { return fmt.Sprintf("cloud returned HTTP %d", e.statusCode) }

func retryDelay(interval time.Duration, failures int, statusErr *responseError) time.Duration {
	if statusErr != nil && statusErr.statusCode == http.StatusPaymentRequired {
		return accessPausedRetry
	}
	if statusErr != nil && statusErr.statusCode == http.StatusTooManyRequests && statusErr.retryAfter > 0 {
		return min(statusErr.retryAfter, maxRetryDelay)
	}
	delay := interval
	if delay <= 0 {
		delay = DefaultInterval
	}
	for attempt := 1; attempt < failures && delay < maxRetryDelay; attempt++ {
		if delay > maxRetryDelay/2 {
			return maxRetryDelay
		}
		delay *= 2
	}
	return min(delay, maxRetryDelay)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryDelay/time.Second) {
			return maxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		return 0
	}
	return min(delay, maxRetryDelay)
}

// Send uploads resource metrics. Process details use a separate, nonpersistent
// endpoint only when the owner requests a live view. Incident snapshots remain
// a separate explicit opt-in.
func (c *Client) Send(ctx context.Context, sample metrics.Metrics) error {
	if !freshSample(sample, time.Now()) {
		return errors.New("refusing stale or invalid metric timestamp")
	}
	// An unsupported mount must not invalidate the resource heartbeat or make
	// missing disk readings look like a complete recovery.
	sample.Disks, sample.DiskCollectionStatus = telemetryDisks(sample.Disks, sample.DiskCollectionStatus)
	sampleID, err := newSampleID()
	if err != nil {
		return fmt.Errorf("create sample id: %w", err)
	}
	message := envelope{Version: 1, SampleID: sampleID, SentAt: time.Now().UTC(), Metrics: telemetryFrom(sample), IncidentSnapshot: c.snapshotFor(sample)}
	return c.sendEnvelope(ctx, sample, message)
}

func telemetryDisks(disks []metrics.DiskInfo, status string) ([]metrics.DiskInfo, string) {
	result := make([]metrics.DiskInfo, 0, min(len(disks), 128))
	for _, disk := range disks {
		if len(disk.Mount) > 1024 || len(disk.Device) > 1024 || !validPercent(disk.Percent, 100) || len(result) == 128 {
			status = metrics.DiskCollectionPartial
			continue
		}
		result = append(result, disk)
	}
	if len(result) == 0 && len(disks) > 0 {
		status = metrics.DiskCollectionUnavailable
	}
	return result, status
}

func (c *Client) sendEnvelope(ctx context.Context, sample metrics.Metrics, message envelope) error {
	payload, err := json.Marshal(message)
	// Optional details must never make a valid resource heartbeat unencodable or
	// exceed Cloud's whole-request budget.
	if message.IncidentSnapshot != nil && (err != nil || len(payload) > maxTelemetryBytes) {
		message.IncidentSnapshot = nil
		payload, err = json.Marshal(message)
	}
	if err != nil {
		return fmt.Errorf("encode metrics: %w", err)
	}
	if len(payload) > maxTelemetryBytes {
		return errors.New("resource metrics exceed Cloud payload limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.metricsURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vpsagent-Live", "1")
	req.Header.Set("X-Vpsagent-Disk-Status", sample.DiskCollectionStatus)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.markReady(sample)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode >= 500 || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTooEarly {
		c.markReady(sample)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		// A snapshot rejected by a newer/stricter server must not suppress core
		// telemetry. Retry the same measurement once without optional details.
		if message.IncidentSnapshot != nil && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusUnprocessableEntity) {
			_ = resp.Body.Close()
			message.IncidentSnapshot = nil
			return c.sendEnvelope(ctx, sample, message)
		}
		return &responseError{statusCode: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	c.commitSnapshotState(sample)
	if resp.Header.Get("X-Vpscloud-Live") == "1" {
		// A failed live view must not mark an accepted resource upload as failed,
		// replay an incident capture, or stop the normal uploader.
		_ = c.sendLive(ctx)
	}
	return nil
}

// Ready closes after a fresh, structurally valid core measurement reaches the
// transport. Remote outages and billing pauses do not make a local update bad;
// payload rejection, invalid credentials and absent collection do.
func (c *Client) Ready() <-chan struct{} { return c.ready }

func (c *Client) markReady(sample metrics.Metrics) {
	if c.ready == nil || !freshSample(sample, time.Now()) || sample.MemTotal == 0 || sample.CPUCount <= 0 {
		return
	}
	for _, value := range []float64{sample.CPUUsage, sample.MemPercent, sample.SwapPercent} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return
		}
	}
	c.readyOnce.Do(func() { close(c.ready) })
}

func freshSample(sample metrics.Metrics, now time.Time) bool {
	return !sample.Timestamp.IsZero() && now.Sub(sample.Timestamp) <= maxSampleAge && sample.Timestamp.Sub(now) <= 5*time.Second
}

func (c *Client) sendLive(ctx context.Context) error {
	topCPU, topMem := c.collectLive()
	payload, err := json.Marshal(struct {
		Version    int                  `json:"version"`
		CapturedAt time.Time            `json:"captured_at"`
		TopCPU     []metrics.TopProcess `json:"top_cpu"`
		TopMem     []metrics.TopProcess `json:"top_mem"`
	}{1, time.Now().UTC(), topCPU, topMem})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.liveURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusNoContent {
		return &responseError{statusCode: resp.StatusCode}
	}
	return nil
}

type envelope struct {
	Version          int               `json:"version"`
	SampleID         string            `json:"sample_id"`
	SentAt           time.Time         `json:"sent_at"`
	Metrics          telemetry         `json:"metrics"`
	IncidentSnapshot *incidentSnapshot `json:"incident_snapshot,omitempty"`
}

func newSampleID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// incidentSnapshot is sent only once per threshold crossing when explicitly
// enabled. It deliberately excludes logs and listeners, which require a more
// specific future consent model.
type incidentSnapshot struct {
	Triggers   []string                  `json:"triggers"`
	CapturedAt time.Time                 `json:"captured_at"`
	TopCPU     []metrics.TopProcess      `json:"top_cpu"`
	TopMem     []metrics.TopProcess      `json:"top_mem"`
	Containers []metrics.DockerContainer `json:"containers"`
}

type telemetry struct {
	Hostname    string             `json:"hostname"`
	Uptime      string             `json:"uptime"`
	LoadAvg     string             `json:"load_avg"`
	CPUCount    int                `json:"cpu_count"`
	CPUUsage    float64            `json:"cpu_usage"`
	MemTotal    uint64             `json:"mem_total"`
	MemUsed     uint64             `json:"mem_used"`
	MemPercent  float64            `json:"mem_percent"`
	SwapTotal   uint64             `json:"swap_total"`
	SwapUsed    uint64             `json:"swap_used"`
	SwapPercent float64            `json:"swap_percent"`
	Disks       []metrics.DiskInfo `json:"disks"`
	NetRx       uint64             `json:"net_rx"`
	NetTx       uint64             `json:"net_tx"`
	NetRxSpeed  float64            `json:"net_rx_speed"`
	NetTxSpeed  float64            `json:"net_tx_speed"`
	Processes   int                `json:"processes"`
	Timestamp   time.Time          `json:"timestamp"`
}

func telemetryFrom(m metrics.Metrics) telemetry {
	disks := m.Disks
	if disks == nil {
		disks = []metrics.DiskInfo{}
	}
	return telemetry{
		Hostname: m.Hostname, Uptime: m.Uptime, LoadAvg: m.LoadAvg,
		CPUCount: m.CPUCount, CPUUsage: m.CPUUsage,
		MemTotal: m.MemTotal, MemUsed: m.MemUsed, MemPercent: m.MemPercent,
		SwapTotal: m.SwapTotal, SwapUsed: m.SwapUsed, SwapPercent: m.SwapPercent,
		Disks: disks, NetRx: m.NetRx, NetTx: m.NetTx,
		NetRxSpeed: m.NetRxSpeed, NetTxSpeed: m.NetTxSpeed,
		Processes: m.Processes, Timestamp: m.Timestamp.UTC(),
	}
}

func (c *Client) snapshotFor(m metrics.Metrics) *incidentSnapshot {
	if !c.snapshots {
		return nil
	}
	diskPercent := 0.0
	for _, disk := range m.Disks {
		if disk.Percent > diskPercent {
			diskPercent = disk.Percent
		}
	}
	values := map[string]float64{"cpu": m.CPUUsage, "memory": m.MemPercent, "disk": diskPercent}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	triggers := make([]string, 0, len(values))
	for rule, value := range values {
		if value >= c.threshold && !c.snapshotActive[rule] {
			triggers = append(triggers, rule)
		}
	}
	if len(triggers) == 0 {
		return nil
	}
	return &incidentSnapshot{Triggers: triggers, CapturedAt: time.Now().UTC(), TopCPU: boundedProcesses(m.TopCPU), TopMem: boundedProcesses(m.TopMem), Containers: boundedContainers(m.Containers)}
}

func (c *Client) commitSnapshotState(m metrics.Metrics) {
	if !c.snapshots {
		return
	}
	diskPercent := 0.0
	for _, disk := range m.Disks {
		if disk.Percent > diskPercent {
			diskPercent = disk.Percent
		}
	}
	c.snapshotMu.Lock()
	c.snapshotActive["cpu"] = m.CPUUsage >= c.threshold
	c.snapshotActive["memory"] = m.MemPercent >= c.threshold
	// A missing disk must not rearm an incident capture as if pressure cleared.
	if m.DiskCollectionStatus == metrics.DiskCollectionComplete || m.DiskCollectionStatus == "" || diskPercent >= c.threshold {
		c.snapshotActive["disk"] = diskPercent >= c.threshold
	}
	c.snapshotMu.Unlock()
}
