package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	assets "github.com/vpsmon/vpsagent"
)

func Enable(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	if err := ensureTrustedUpdateDirs(); err != nil {
		return err
	}
	unlock, err := acquireUpdateLock(ctx, ControlDir)
	if err != nil {
		return err
	}
	defer unlock()
	if !ValidVersion(Version) {
		return errors.New("remote updates require a released agent binary")
	}
	if err := trustedPath(InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(filepath.Join(InstallDir, "vpsagent"), false); err != nil {
		return err
	}
	installed, err := executableVersion(ctx, filepath.Join(InstallDir, "vpsagent"))
	if err != nil || installed != Version {
		return errors.New("run the installed release binary to enable updates")
	}
	if err := installRecovery(ctx); err != nil {
		return err
	}
	u, err := user.Lookup("vpsagent")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	for _, dir := range []string{ControlDir, "/var/cache/vpsagent-update"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if err := trustedPath(dir, true); err != nil {
			return err
		}
	}
	requests := filepath.Join(ControlDir, "requests")
	if info, err := os.Lstat(requests); err == nil && !info.IsDir() {
		return errors.New("untrusted request directory")
	}
	if err := os.MkdirAll(requests, 0700); err != nil {
		return err
	}
	if err := os.Chown(requests, uid, gid); err != nil {
		return err
	}
	if err := os.Chmod(requests, 0700); err != nil {
		return err
	}
	for _, name := range []string{"vpsagent-update.service", "vpsagent-update.path"} {
		data, err := assets.Files.ReadFile("scripts/" + name)
		if err != nil {
			return err
		}
		if err := writeSetupFile("/etc/systemd/system/"+name, data, 0644); err != nil {
			return err
		}
	}
	dropIn := "/etc/systemd/system/vpsagent.service.d"
	if err := os.MkdirAll(dropIn, 0755); err != nil {
		return err
	}
	if err := trustedPath(dropIn, true); err != nil {
		return err
	}
	if err := writeSetupFile(filepath.Join(dropIn, "remote-updates.conf"), []byte("[Service]\nReadWritePaths=/var/lib/vpsagent-control/requests\n"), 0644); err != nil {
		return err
	}
	remove, err := assets.Files.ReadFile("scripts/remove.sh")
	if err != nil {
		return err
	}
	if err := writeSetupFile(filepath.Join(InstallDir, "remove.sh"), remove, 0755); err != nil {
		return err
	}
	if err := writeSetupFile(filepath.Join(ControlDir, "enabled"), []byte("enabled\n"), 0644); err != nil {
		return err
	}
	return runAfterUnlock(unlock, func() error {
		if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "vpsagent-recover.service").Run(); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "vpsagent-update.service", "vpsagent-update.path").Run(); err != nil {
			return err
		}
		return exec.CommandContext(ctx, "systemctl", "restart", "vpsagent.service").Run()
	})
}
func Disable(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
	if err := os.Remove(filepath.Join(ControlDir, "enabled")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Stop watching immediately; allow an in-progress verified update to finish.
	return exec.CommandContext(ctx, "systemctl", "disable", "--now", "vpsagent-update.path").Run()
}

// RecoverAtBoot is the stable recovery entry point used by the boot unit.
func RecoverAtBoot(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run boot recovery as root")
	}
	if err := ensureRootDirectory(ControlDir); err != nil {
		return err
	}
	if err := makeDirectoryTraversable(ControlDir, 0); err != nil {
		return err
	}
	e := Production()
	return e.RecoverAtBoot(ctx)
}

func runAfterUnlock(unlock func(), action func() error) error {
	unlock()
	return action()
}
func writeSetupFile(path string, data []byte, mode os.FileMode) error {
	// Atomic replacement avoids following a destination symlink.
	f, err := os.CreateTemp(filepath.Dir(path), ".vpsagent-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func ApplyPending(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run updater as root")
	}
	if err := ensureTrustedUpdateDirs(); err != nil {
		return err
	}
	unlock, err := acquireUpdateLock(ctx, ControlDir)
	if err != nil {
		return err
	}
	defer unlock()
	engine := Production()
	if err := engine.recover(ctx, true); err != nil {
		return err
	}
	path := filepath.Join(ControlDir, "requests", "update.json")
	var request Request
	err = ReadJSON(path, &request)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if !Enabled(ControlDir) {
		return errors.New("remote updates are disabled")
	}
	return engine.applyLocked(ctx, request)
}

// UpdateLatest runs an explicit manual update through the same authenticated
// engine and lock as remote updates. It neither checks nor enables opt-in.
func UpdateLatest(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run updater as root")
	}
	if err := ensureTrustedUpdateDirs(); err != nil {
		return err
	}
	// Provision and start recovery before taking the updater lock. A recovery
	// unit started by the agent restart must never wait on the lock held here.
	if err := installRecovery(ctx); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "enable", "vpsagent-recover.service").Run(); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "start", "vpsagent-recover.service").Run(); err != nil {
		return err
	}
	unlock, err := acquireUpdateLock(ctx, ControlDir)
	if err != nil {
		return err
	}
	defer unlock()
	e := Production()
	if err := e.recover(ctx, true); err != nil {
		return err
	}
	current, err := e.CurrentVersion(ctx)
	if err != nil {
		return err
	}
	target, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	if !Newer(target, current) {
		if target == current {
			return nil
		}
		return fmt.Errorf("latest release %s is older than installed %s", target, current)
	}
	r := Request{ID: fmt.Sprintf("upd_manual_%d", time.Now().UnixNano()), TargetVersion: target, ExpiresAt: time.Now().Add(10 * time.Minute)}
	return e.applyLocked(ctx, r)
}

// installRecovery installs a stable copy of the current authenticated updater
// and a boot unit ordered before the agent. It is independent of remote opt-in.
func installRecovery(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run recovery setup as root")
	}
	if err := trustedPath(InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(filepath.Join(InstallDir, "vpsagent"), false); err != nil {
		return err
	}
	helper := filepath.Join(InstallDir, "vpsagent-recovery")
	if _, err := os.Lstat(helper); errors.Is(err, os.ErrNotExist) {
		if err := linkRecoveryExecutable(filepath.Join(InstallDir, "vpsagent"), helper); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := trustedPath(helper, false); err != nil {
		return err
	}
	unit, err := assets.Files.ReadFile("scripts/vpsagent-recover.service")
	if err != nil {
		return err
	}
	if err := writeSetupFile("/etc/systemd/system/vpsagent-recover.service", unit, 0644); err != nil {
		return err
	}
	dropIn := "/etc/systemd/system/vpsagent.service.d"
	if err := os.MkdirAll(dropIn, 0755); err != nil {
		return err
	}
	if err := trustedPath(dropIn, true); err != nil {
		return err
	}
	capable, err := supportsSystemdNotify(ctx, filepath.Join(InstallDir, "vpsagent"))
	if err != nil {
		return err
	}
	serviceType := "simple"
	if capable {
		serviceType = "notify"
	}
	dropInData := []byte("[Unit]\nRequires=vpsagent-recover.service\nAfter=vpsagent-recover.service\n[Service]\nType=" + serviceType + "\nNotifyAccess=main\nTimeoutStartSec=60\n")
	if err := writeSetupFile(filepath.Join(dropIn, "recovery.conf"), dropInData, 0644); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		return err
	}
	return exec.CommandContext(ctx, "systemctl", "enable", "vpsagent-recover.service").Run()
}

func latestRelease(ctx context.Context) (string, error) {
	data, err := download(ctx, "https://api.github.com/repos/vpsmon/vpsagent/releases/latest", 64<<10)
	if err != nil {
		return "", err
	}
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(data, &release); err != nil || release.Draft || release.Prerelease || !ValidVersion(release.TagName) {
		return "", errors.New("invalid latest release response")
	}
	return release.TagName, nil
}

func ensureTrustedUpdateDirs() error {
	for _, dir := range []string{ControlDir, "/var/cache/vpsagent-update", filepath.Join("/var/cache/vpsagent-update", "sigstore")} {
		if err := ensureRootDirectory(dir); err != nil {
			return err
		}
	}
	// The agent service must traverse ControlDir to read result/enabled state
	// and submit requests. Installer and updater processes use a restrictive
	// umask, so explicitly restore the intended root-owned directory mode.
	return makeDirectoryTraversable(ControlDir, 0)
}

func makeDirectoryTraversable(path string, owner uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&022 != 0 {
		return fmt.Errorf("untrusted updater directory: %s", path)
	}
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0755 {
		return fmt.Errorf("could not set updater directory mode: %s", path)
	}
	return nil
}

func ensureRootDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("unsafe updater directory")
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), current)
	for _, component := range parts {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&022 != 0 {
			return fmt.Errorf("untrusted updater directory: %s", current)
		}
	}
	return nil
}

func acquireUpdateLock(ctx context.Context, controlDir string) (func(), error) {
	if err := os.MkdirAll(controlDir, 0755); err != nil {
		return nil, err
	}
	if err := trustedPath(controlDir, true); err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(filepath.Join(controlDir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&022 != 0 {
		fd.Close()
		return nil, errors.New("untrusted updater lock")
	}
	for {
		err = syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() { _ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN); _ = fd.Close() })
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			fd.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			fd.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
