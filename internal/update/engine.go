package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Engine struct {
	InstallDir     string
	ControlDir     string
	Fetch          func(context.Context, string, string) (string, error)
	Restart        func(context.Context) error
	Healthy        func(context.Context, string) error
	CurrentVersion func(context.Context) (string, error)
}
type journal struct {
	Request     Request `json:"request"`
	OldVersion  string  `json:"old_version"`
	Running     bool    `json:"running"`
	BackupReady bool    `json:"backup_ready"`
}

func Production() Engine {
	e := Engine{InstallDir: InstallDir, ControlDir: ControlDir}
	e.Fetch = func(ctx context.Context, v, dir string) (string, error) {
		return FetchRelease(ctx, v, dir, func(data, proof []byte, version string) error {
			return verifyManifestContext(ctx, data, proof, version)
		})
	}
	e.Restart = func(ctx context.Context) error {
		return exec.CommandContext(ctx, "systemctl", "restart", "vpsagent.service").Run()
	}
	e.CurrentVersion = func(ctx context.Context) (string, error) {
		return executableVersion(ctx, filepath.Join(InstallDir, "vpsagent"))
	}
	e.Healthy = func(ctx context.Context, target string) error {
		return serviceHealthy(ctx, "vpsagent.service", target, e.CurrentVersion)
	}
	return e
}
func serviceHealthy(ctx context.Context, service, target string, version func(context.Context) (string, error)) error {
	stable := 0
	var previousPID string
	for range 15 {
		pid, pidErr := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "--value", service).Output()
		currentPID := strings.TrimSpace(string(pid))
		if pidErr == nil && currentPID != "" && currentPID != "0" && exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", service).Run() == nil {
			actual, err := version(ctx)
			if err == nil && actual == target {
				if currentPID != previousPID {
					stable = 0
				}
				previousPID = currentPID
				stable++
				if stable >= 5 {
					return nil
				}
			} else {
				stable = 0
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("updated agent did not remain running")
}
func executableVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(output))
	if !ValidVersion(version) {
		return "", errors.New("invalid agent executable version")
	}
	return version, nil
}
func trustedPath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&022 != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("untrusted updater path: %s", path)
	}
	return nil
}
func (e Engine) result(r Request, status, stage, version string) error {
	return WriteJSON(filepath.Join(e.ControlDir, "result.json"), Result{ID: r.ID, Status: status, Stage: stage, Version: version}, 0644)
}
func (e Engine) Apply(ctx context.Context, r Request) (applyErr error) {
	if err := trustedPath(e.InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(e.ControlDir, true); err != nil {
		return err
	}
	if err := trustedPath(filepath.Join(e.InstallDir, "vpsagent"), false); err != nil {
		return err
	}
	current, err := e.CurrentVersion(ctx)
	if err != nil {
		return err
	}
	if !idPattern.MatchString(r.ID) || !ValidVersion(r.TargetVersion) || !time.Now().Before(r.ExpiresAt) || r.ExpiresAt.After(time.Now().Add(20*time.Minute)) || (!Newer(r.TargetVersion, current) && r.TargetVersion != current) {
		e.result(r, "failed", "invalid_request", current)
		return errors.New("invalid update request")
	}
	var previous Result
	if ReadJSON(filepath.Join(e.ControlDir, "result.json"), &previous) == nil && previous.ID == r.ID && previous.Status != "running" {
		return nil
	}
	if current == r.TargetVersion {
		return e.result(r, "succeeded", "complete", current)
	}
	j := journal{Request: r, OldVersion: current, Running: true}
	if err := WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600); err != nil {
		return err
	}
	defer func() {
		if applyErr != nil && j.Running {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			applyErr = errors.Join(applyErr, e.Recover(recoveryCtx))
		}
	}()
	finish := func(status, stage, version string) error {
		if err := e.result(r, status, stage, version); err != nil {
			return err
		}
		j.Running = false
		return WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600)
	}
	if err := e.result(r, "running", "downloading", current); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(e.InstallDir, ".update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	candidate, err := e.Fetch(ctx, r.TargetVersion, staging)
	if err != nil {
		finish("failed", "verification_failed", current)
		return err
	}
	if err := e.result(r, "running", "verifying", current); err != nil {
		return err
	}
	if err := e.HealthyCandidate(ctx, candidate, r.TargetVersion); err != nil {
		finish("failed", "verification_failed", current)
		return err
	}
	// Both files are fsynced before replacing the current executable. The journal
	// and previous binary remain available after a crash or reboot.
	backup := filepath.Join(e.InstallDir, "vpsagent.previous")
	if err := copyExecutable(filepath.Join(e.InstallDir, "vpsagent"), backup); err != nil {
		return err
	}
	j.BackupReady = true
	if err := WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600); err != nil {
		return err
	}
	if err := copyExecutable(candidate, filepath.Join(e.InstallDir, "vpsagent")); err != nil {
		return err
	}
	if err := e.result(r, "running", "restarting", current); err != nil {
		return err
	}
	if err = e.Restart(ctx); err == nil {
		err = e.Healthy(ctx, r.TargetVersion)
	}
	if err != nil {
		return err
	}
	return finish("succeeded", "complete", r.TargetVersion)
}
func (e Engine) HealthyCandidate(ctx context.Context, path, expected string) error {
	if filepath.Dir(path) == e.InstallDir {
		return errors.New("candidate must be staged")
	}
	// Production fetches a signed artifact before this check. Tests use shell
	// fixtures; all production targets are root-owned private staging files.
	version, err := executableVersion(ctx, path)
	if err != nil {
		return err
	}
	if version != expected {
		return errors.New("signed binary version mismatch")
	}
	return nil
}
func copyExecutable(source, target string) error {
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("executable is not a regular file")
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".binary-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0755); err != nil {
		file.Close()
		return err
	}
	if _, err := io.Copy(file, input); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (e Engine) Recover(ctx context.Context) error {
	var j journal
	if err := ReadJSON(filepath.Join(e.ControlDir, "journal.json"), &j); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !j.Running {
		return nil
	}
	if err := trustedPath(e.InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(e.ControlDir, true); err != nil {
		return err
	}
	backup := filepath.Join(e.InstallDir, "vpsagent.previous")
	status := "failed"
	if !idPattern.MatchString(j.Request.ID) || !ValidVersion(j.OldVersion) || !ValidVersion(j.Request.TargetVersion) {
		return errors.New("invalid recovery journal")
	}
	if j.BackupReady {
		if err := trustedPath(backup, false); err != nil {
			return err
		}
		version, err := executableVersion(ctx, backup)
		if err != nil || version != j.OldVersion {
			return errors.New("invalid recovery binary")
		}
		if err := copyExecutable(backup, filepath.Join(e.InstallDir, "vpsagent")); err != nil {
			return err
		}
		if err := e.Restart(ctx); err != nil {
			return err
		}
		if err := e.Healthy(ctx, j.OldVersion); err != nil {
			return err
		}
		status = "rolled_back"
	}
	if err := e.result(j.Request, status, "restart_failed", j.OldVersion); err != nil {
		return err
	}
	j.Running = false
	return WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600)
}
