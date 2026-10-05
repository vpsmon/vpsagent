package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/vpsmon/vpsagent/internal/update"
)

// StartControl operates independently of the collector and billing-gated
// telemetry. Remote updates still require the local root-owned opt-in marker.
func (c *Client) StartControl(ctx context.Context, version string) {
	if runtime.GOOS != "linux" {
		return
	}
	go func() {
		delay := time.Duration(0)
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			err := c.checkControl(ctx, version, update.ControlDir, update.Enabled, update.Submit)
			delay = 30 * time.Second
			if err != nil {
				c.logger.Printf("vpsagent: control check failed: %v", err)
				delay = 5 * time.Minute
			}
		}
	}()
}
func (c *Client) checkControl(ctx context.Context, version, dir string, enabled func(string) bool, submit func(string, update.Request) error) error {
	report := struct {
		Version       string         `json:"version"`
		Platform      string         `json:"platform"`
		RemoteEnabled bool           `json:"remote_enabled"`
		Result        *update.Result `json:"result,omitempty"`
	}{Version: version, Platform: runtime.GOOS + "-" + runtime.GOARCH, RemoteEnabled: enabled(dir)}
	var result update.Result
	if update.ReadJSON(filepath.Join(dir, "result.json"), &result) == nil {
		report.Result = &result
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.controlURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return &responseError{statusCode: res.StatusCode}
	}
	// Cloud may include timestamps/status for the UI. Only these three fixed
	// request fields reach the privileged helper.
	var response struct {
		Update *struct {
			ID            string    `json:"id"`
			TargetVersion string    `json:"target_version"`
			ExpiresAt     time.Time `json:"expires_at"`
		} `json:"update"`
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, 4097))
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return errors.New("oversized control response")
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}
	if response.Update == nil || !report.RemoteEnabled {
		return nil
	}
	r := update.Request{ID: response.Update.ID, TargetVersion: response.Update.TargetVersion, ExpiresAt: response.Update.ExpiresAt}
	if !update.Newer(r.TargetVersion, version) && r.TargetVersion != version {
		return errors.New("refusing downgrade request")
	}
	return submit(dir, r)
}
