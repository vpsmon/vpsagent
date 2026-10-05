package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vpsmon/vpsagent/internal/update"
)

func TestControlWorksWithoutMetricsAndRequiresLocalOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[optIn], func(t *testing.T) {
			request := update.Request{ID: "upd_test", TargetVersion: "v0.0.9", ExpiresAt: time.Now().Add(time.Minute)}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agent/control" || r.Header.Get("Authorization") != "Bearer scoped-token" {
					t.Error("wrong control auth/path")
				}
				var report struct {
					Version       string         `json:"version"`
					RemoteEnabled bool           `json:"remote_enabled"`
					Result        *update.Result `json:"result"`
				}
				json.NewDecoder(r.Body).Decode(&report)
				if report.Version != "v0.0.8" || report.RemoteEnabled != optIn || report.Result == nil || report.Result.Status != "rolled_back" {
					t.Error(report)
				}
				json.NewEncoder(w).Encode(map[string]any{"update": request})
			}))
			defer srv.Close()
			c, err := New(Config{URL: srv.URL, Token: "scoped-token", AllowInsecure: true})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			update.WriteJSON(filepath.Join(dir, "result.json"), update.Result{ID: "upd_old", Status: "rolled_back", Stage: "restart_failed", Version: "v0.0.8"}, 0644)
			submits := 0
			err = c.checkControl(t.Context(), "v0.0.8", dir, func(string) bool { return optIn }, func(_ string, r update.Request) error {
				submits++
				if r.ID != request.ID || r.TargetVersion != request.TargetVersion || !r.ExpiresAt.Equal(request.ExpiresAt) {
					t.Error(r)
				}
				return nil
			})
			if err != nil || submits != map[bool]int{false: 0, true: 1}[optIn] {
				t.Fatal(err, submits)
			}
		})
	}
}
func TestControlRefusesDowngrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"update": update.Request{ID: "upd_test", TargetVersion: "v0.0.7", ExpiresAt: time.Now().Add(time.Minute)}})
	}))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL, Token: "scoped-token", AllowInsecure: true})
	if err := c.checkControl(t.Context(), "v0.0.8", t.TempDir(), func(string) bool { return true }, func(string, update.Request) error { t.Fatal("submitted downgrade"); return nil }); err == nil {
		t.Fatal("accepted downgrade")
	}
}
