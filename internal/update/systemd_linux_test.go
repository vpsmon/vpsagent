//go:build linux

package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run explicitly on a disposable Linux/systemd host, or with isolated test
// units on a host without touching the production vpsagent unit.
func TestSystemdUpdateAndRollback(t *testing.T) {
	if os.Getenv("VPSAGENT_SYSTEMD_TEST") != "1" {
		t.Skip("explicit Linux systemd integration test")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root for temporary systemd units")
	}
	root, err := os.MkdirTemp("/tmp", "vpsagent-updater-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.Chmod(root, 0755)
	install := filepath.Join(root, "install")
	control := filepath.Join(root, "control")
	requests := filepath.Join(control, "requests")
	for _, dir := range []string{install, control, requests} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(nobody.Uid)
	gid, _ := strconv.Atoi(nobody.Gid)
	os.Chown(requests, uid, gid)
	os.Chmod(requests, 0700)
	service := "vpsagent-test-" + strconv.Itoa(os.Getpid())
	worker := service + "-update"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{service + ".service", worker + ".service", worker + ".path"}
	defer func() {
		exec.Command("systemctl", "disable", "--now", worker+".path", worker+".service", service+".service").Run()
		for _, name := range names {
			os.Remove("/etc/systemd/system/" + name)
		}
		exec.Command("systemctl", "daemon-reload").Run()
	}()
	script := func(version string, fail bool) []byte {
		normal := "while :; do sleep 1; done"
		if fail {
			normal = "exit 1"
		}
		return []byte("#!/bin/sh\nif [ \"${1:-}\" = version ]; then echo " + version + "; exit 0; fi\n" + normal + "\n")
	}
	os.WriteFile(filepath.Join(install, "vpsagent"), script("v0.0.8", false), 0755)
	os.WriteFile(filepath.Join(root, "v0.0.9"), script("v0.0.9", false), 0755)
	os.WriteFile(filepath.Join(root, "v0.0.10"), script("v0.0.10", true), 0755)
	units := []string{
		fmt.Sprintf("[Service]\nUser=nobody\nExecStart=%s/vpsagent\nProtectSystem=strict\nReadWritePaths=%s\nNoNewPrivileges=true\n", install, requests),
		fmt.Sprintf("[Service]\nType=oneshot\nExecStart=%s -test.run=^TestSystemdUpdateHelper$\nEnvironment=VPSAGENT_UPDATER_TEST_ROOT=%s\nEnvironment=VPSAGENT_UPDATER_TEST_SERVICE=%s.service\nTimeoutStartSec=90\nProtectSystem=strict\nReadWritePaths=%s %s\nNoNewPrivileges=true\n", binary, root, service, install, control),
		fmt.Sprintf("[Path]\nPathChanged=%s\nPathExists=%s/update.json\nUnit=%s.service\n", requests, requests, worker),
	}
	for i, name := range names {
		if err := os.WriteFile("/etc/systemd/system/"+name, []byte(units[i]), 0644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	run("daemon-reload")
	run("start", service+".service", worker+".path")
	for i, target := range []string{"v0.0.9", "v0.0.10"} {
		r := Request{ID: fmt.Sprintf("upd_test%d", i), TargetVersion: target, ExpiresAt: time.Now().Add(time.Minute)}
		incoming := filepath.Join(root, "incoming.json")
		WriteJSON(incoming, r, 0644)
		// Write as the unprivileged user inside the agent's read-only mount namespace.
		pid, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", service+".service").Output()
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("nsenter", "-t", strings.TrimSpace(string(pid)), "-m", "runuser", "-u", "nobody", "--", "cp", incoming, filepath.Join(requests, "update.json")).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		deadline := time.Now().Add(45 * time.Second)
		var result Result
		for time.Now().Before(deadline) {
			ReadJSON(filepath.Join(control, "result.json"), &result)
			if result.ID == r.ID && result.Status != "running" {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		expected := []string{"succeeded", "rolled_back"}[i]
		if result.ID != r.ID || result.Status != expected {
			log, _ := exec.Command("journalctl", "-u", worker+".service", "-n", "20", "--no-pager").Output()
			t.Fatalf("result=%+v\n%s", result, log)
		}
		version, err := executableVersion(t.Context(), filepath.Join(install, "vpsagent"))
		if err != nil || version != "v0.0.9" {
			t.Fatal(version, err)
		}
	}
}
func TestSystemdUpdateHelper(t *testing.T) {
	root := os.Getenv("VPSAGENT_UPDATER_TEST_ROOT")
	if root == "" {
		t.Skip("integration helper")
	}
	if !strings.HasPrefix(root, "/tmp/vpsagent-updater-test-") || os.Geteuid() != 0 {
		t.Fatal("invalid integration helper environment")
	}
	e := Engine{InstallDir: filepath.Join(root, "install"), ControlDir: filepath.Join(root, "control")}
	service := os.Getenv("VPSAGENT_UPDATER_TEST_SERVICE")
	e.CurrentVersion = func(ctx context.Context) (string, error) {
		return executableVersion(ctx, filepath.Join(e.InstallDir, "vpsagent"))
	}
	e.Fetch = func(_ context.Context, v, dir string) (string, error) {
		candidate := filepath.Join(dir, "vpsagent")
		return candidate, copyExecutable(filepath.Join(root, v), candidate)
	}
	e.Restart = func(ctx context.Context) error {
		return exec.CommandContext(ctx, "systemctl", "restart", service).Run()
	}
	e.Healthy = func(ctx context.Context, v string) error { return serviceHealthy(ctx, service, v, e.CurrentVersion) }
	var r Request
	path := filepath.Join(e.ControlDir, "requests", "update.json")
	if err := ReadJSON(path, &r); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	if err := e.Apply(ctx, r); err != nil {
		var result Result
		ReadJSON(filepath.Join(e.ControlDir, "result.json"), &result)
		if result.Status != "rolled_back" {
			t.Fatal(err)
		}
	}
}
