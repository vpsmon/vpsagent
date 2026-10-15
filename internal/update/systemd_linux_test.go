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
	for _, command := range []string{"systemctl", "systemd-notify", "curl", "nsenter", "runuser"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("systemd integration requires %s", command)
		}
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
	recovery := service + "-recover"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{service + ".service", worker + ".service", worker + ".path", recovery + ".service"}
	dropIn := "/etc/systemd/system/" + service + ".service.d"
	if err := os.MkdirAll(dropIn, 0755); err != nil {
		t.Fatal(err)
	}
	defer func() {
		exec.Command("systemctl", "disable", "--now", worker+".path", worker+".service", recovery+".service", service+".service").Run()
		for _, name := range names {
			os.Remove("/etc/systemd/system/" + name)
		}
		os.RemoveAll(dropIn)
		exec.Command("systemctl", "daemon-reload").Run()
	}()
	script := func(version string, announceReady bool) []byte {
		normal := "curl --connect-timeout 1 --max-time 1 http://127.0.0.1:1/ >/dev/null 2>&1 || true\nsystemd-notify --ready\nwhile :; do sleep 1; done"
		if !announceReady {
			normal = "while :; do sleep 1; done"
		}
		return []byte("#!/bin/sh\nif [ \"${1:-}\" = version ]; then echo " + version + "; exit 0; fi\nif [ \"${1:-}\" = capabilities ]; then echo systemd-notify-v1; exit 0; fi\n" + normal + "\n")
	}
	legacy := []byte("#!/bin/sh\nif [ \"${1:-}\" = version ]; then echo v0.0.8; exit 0; fi\nif [ $# -gt 0 ]; then exit 1; fi\nwhile :; do sleep 1; done\n")
	os.WriteFile(filepath.Join(install, "vpsagent"), legacy, 0755)
	os.WriteFile(filepath.Join(root, "v0.0.9"), script("v0.0.9", false), 0755)
	os.WriteFile(filepath.Join(root, "v0.0.10"), script("v0.0.10", true), 0755)
	units := []string{
		fmt.Sprintf("[Unit]\nRequires=%s.service\nAfter=%s.service\n[Service]\nType=simple\nNotifyAccess=all\nTimeoutStartSec=3\nUser=nobody\nExecStart=%s/vpsagent\nProtectSystem=strict\nReadWritePaths=%s\nNoNewPrivileges=true\n", recovery, recovery, install, requests),
		fmt.Sprintf("[Service]\nType=oneshot\nExecStart=%s -test.run=^TestSystemdUpdateHelper$\nEnvironment=VPSAGENT_UPDATER_TEST_ROOT=%s\nEnvironment=VPSAGENT_UPDATER_TEST_SERVICE=%s.service\nTimeoutStartSec=90\nProtectSystem=strict\nReadWritePaths=%s %s %s\nNoNewPrivileges=true\n", binary, root, service, install, control, dropIn),
		fmt.Sprintf("[Path]\nPathChanged=%s\nPathExists=%s/update.json\nUnit=%s.service\n", requests, requests, worker),
		fmt.Sprintf("[Unit]\nBefore=%s.service\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=%s -test.run=^TestSystemdRecoveryHelper$\nEnvironment=VPSAGENT_RECOVERY_TEST_ROOT=%s\nEnvironment=VPSAGENT_RECOVERY_TEST_SERVICE=%s.service\nTimeoutStartSec=30\nProtectSystem=strict\nReadWritePaths=%s %s %s\nNoNewPrivileges=true\n", service, binary, root, service, install, control, dropIn),
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
		expected := []string{"rolled_back", "succeeded"}[i]
		if result.ID != r.ID || result.Status != expected {
			log, _ := exec.Command("journalctl", "-u", worker+".service", "-n", "20", "--no-pager").Output()
			t.Fatalf("result=%+v\n%s", result, log)
		}
		version, err := executableVersion(t.Context(), filepath.Join(install, "vpsagent"))
		want := []string{"v0.0.8", "v0.0.10"}[i]
		if err != nil || version != want {
			t.Fatal(version, err)
		}
	}
	state, err := exec.Command("systemctl", "show", "--property=ActiveState", "--value", recovery+".service").Output()
	if err != nil || strings.TrimSpace(string(state)) != "active" {
		t.Fatalf("no-journal recovery did not remain active: %q (%v)", state, err)
	}
	// A simulated reboot clears the oneshot's active state. A running journal
	// must restore the old binary before the agent starts, without restarting
	// that not-yet-started service.
	run("stop", service+".service", recovery+".service")
	if err := os.WriteFile(filepath.Join(install, "vpsagent.previous"), script("v0.0.8", true), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "vpsagent"), script("v0.0.9", true), 0755); err != nil {
		t.Fatal(err)
	}
	bootRequest := Request{ID: "upd_boot_test", TargetVersion: "v0.0.10", ExpiresAt: time.Now().Add(time.Minute)}
	if err := WriteJSON(filepath.Join(control, "journal.json"), journal{Request: bootRequest, OldVersion: "v0.0.8", Running: true, BackupReady: true}, 0600); err != nil {
		t.Fatal(err)
	}
	run("start", recovery+".service")
	version, err := executableVersion(t.Context(), filepath.Join(install, "vpsagent"))
	if err != nil || version != "v0.0.8" {
		t.Fatalf("boot recovery left version %s, err=%v", version, err)
	}
	run("start", service+".service")
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
	e.PrepareRestart = func(ctx context.Context, _ string) error {
		capable, err := supportsSystemdNotify(ctx, filepath.Join(e.InstallDir, "vpsagent"))
		if err != nil {
			return err
		}
		serviceType := "simple"
		if capable {
			serviceType = "notify"
		}
		config := filepath.Join("/etc/systemd/system", strings.TrimSuffix(service, ".service")+".service.d", "readiness.conf")
		if err := os.WriteFile(config, []byte("[Service]\nType="+serviceType+"\n"), 0644); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	}
	e.Healthy = func(ctx context.Context, v string) error {
		capable, err := supportsSystemdNotify(ctx, filepath.Join(e.InstallDir, "vpsagent"))
		if err != nil {
			return err
		}
		return serviceHealthyMode(ctx, service, v, e.CurrentVersion, capable)
	}
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

func TestSystemdRecoveryHelper(t *testing.T) {
	root := os.Getenv("VPSAGENT_RECOVERY_TEST_ROOT")
	if root == "" {
		t.Skip("integration helper")
	}
	if !strings.HasPrefix(root, "/tmp/vpsagent-updater-test-") || os.Geteuid() != 0 {
		t.Fatal("invalid integration recovery environment")
	}
	e := Engine{InstallDir: filepath.Join(root, "install"), ControlDir: filepath.Join(root, "control")}
	service := os.Getenv("VPSAGENT_RECOVERY_TEST_SERVICE")
	e.CurrentVersion = func(ctx context.Context) (string, error) {
		return executableVersion(ctx, filepath.Join(e.InstallDir, "vpsagent"))
	}
	e.PrepareRestart = func(ctx context.Context, _ string) error {
		capable, err := supportsSystemdNotify(ctx, filepath.Join(e.InstallDir, "vpsagent"))
		if err != nil {
			return err
		}
		serviceType := "simple"
		if capable {
			serviceType = "notify"
		}
		config := filepath.Join("/etc/systemd/system", strings.TrimSuffix(service, ".service")+".service.d", "readiness.conf")
		if err := os.WriteFile(config, []byte("[Service]\nType="+serviceType+"\n"), 0644); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	}
	if err := e.RecoverAtBoot(t.Context()); err != nil {
		t.Fatal(err)
	}
}
