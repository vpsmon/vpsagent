package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	assets "github.com/vpsmon/vpsagent"
)

func Enable(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo")
	}
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
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "vpsagent-update.service", "vpsagent-update.path").Run(); err != nil {
		return err
	}
	return exec.CommandContext(ctx, "systemctl", "restart", "vpsagent.service").Run()
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
	if err := trustedPath(ControlDir, true); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(ControlDir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	engine := Production()
	if err := engine.Recover(ctx); err != nil {
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
	return engine.Apply(ctx, request)
}
