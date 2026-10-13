package update

import (
	"bytes"
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
	FreeBytes      func(string) (uint64, error)
	RefreshHelpers func(context.Context) error
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
	e.RefreshHelpers = refreshInstalledHelpers
	return e
}

func refreshInstalledHelpers(ctx context.Context) error {
	binary := filepath.Join(InstallDir, "vpsagent")
	updateScript, err := readHelper(ctx, binary, "update")
	if err != nil {
		return err
	}
	removeScript, err := readHelper(ctx, binary, "remove")
	if err != nil {
		return err
	}
	updatePath := filepath.Join(InstallDir, "update.sh")
	removePath := filepath.Join(InstallDir, "remove.sh")
	oldUpdate, updateExists, err := readOptionalTrustedFile(updatePath)
	if err != nil {
		return err
	}
	oldRemove, removeExists, err := readOptionalTrustedFile(removePath)
	if err != nil {
		return err
	}
	if err := writeSetupFile(updatePath, updateScript, 0755); err != nil {
		return err
	}
	if err := writeSetupFile(removePath, removeScript, 0755); err != nil {
		var restoreUpdateErr, restoreRemoveErr error
		if updateExists {
			restoreUpdateErr = writeSetupFile(updatePath, oldUpdate, 0755)
		} else {
			restoreUpdateErr = os.Remove(updatePath)
		}
		if removeExists {
			restoreRemoveErr = writeSetupFile(removePath, oldRemove, 0755)
		} else {
			restoreRemoveErr = os.Remove(removePath)
		}
		return errors.Join(err, restoreUpdateErr, restoreRemoveErr)
	}
	return nil
}

func readOptionalTrustedFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := trustedPath(path, false); err != nil {
		return nil, false, err
	}
	if info.Size() > 1<<20 {
		return nil, false, errors.New("updater helper exceeds limit")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, false, errors.New("invalid updater helper")
	}
	return data, true, nil
}

func readHelper(ctx context.Context, binary, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "print-helper", name)
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if output.overflow || output.Len() == 0 {
		return nil, errors.New("invalid embedded updater helper")
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	const max = 1 << 20
	if b.Len()+len(p) > max {
		b.overflow = true
		return 0, errors.New("helper output exceeds limit")
	}
	return b.Buffer.Write(p)
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
	unlock, err := acquireUpdateLock(ctx, e.ControlDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.recover(ctx); err != nil {
		return err
	}
	return e.applyLocked(ctx, r)
}

func (e Engine) applyLocked(ctx context.Context, r Request) (applyErr error) {
	if err := trustedPath(e.InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(e.ControlDir, true); err != nil {
		return err
	}
	if err := trustedPath(filepath.Join(e.InstallDir, "vpsagent"), false); err != nil {
		return err
	}
	if err := cleanAbandoned(e.InstallDir); err != nil {
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
	currentInfo, err := os.Lstat(filepath.Join(e.InstallDir, "vpsagent"))
	if err != nil || !currentInfo.Mode().IsRegular() {
		return errors.New("invalid installed executable")
	}
	backup := filepath.Join(e.InstallDir, "vpsagent.previous")
	if _, err := os.Lstat(backup); err == nil {
		if err := trustedPath(backup, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	freeBytes := e.FreeBytes
	if freeBytes == nil {
		freeBytes = availableBytes
	}
	free, err := freeBytes(e.InstallDir)
	if err != nil {
		return fmt.Errorf("check updater disk space: %w", err)
	}
	// The signed artifact is capped at 128 MiB. Rollback uses a hard link to the
	// current inode, so it allocates metadata only; keep 64 MiB free for that
	// metadata and journal/recovery work if the filesystem fills.
	const reserve = uint64(64 << 20)
	need := uint64(maxBinarySize) + reserve
	if free < need {
		return fmt.Errorf("insufficient disk space for safe update: need %d bytes, have %d", need, free)
	}
	j := journal{Request: r, OldVersion: current, Running: true}
	if err := WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600); err != nil {
		return err
	}
	defer func() {
		if applyErr != nil && j.Running {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			recoveryErr := e.recover(recoveryCtx)
			applyErr = errors.Join(applyErr, recoveryErr)
			if recoveryErr == nil && e.RefreshHelpers != nil {
				applyErr = errors.Join(applyErr, e.RefreshHelpers(recoveryCtx))
			}
		}
	}()
	finish := func(status, stage, version string) error {
		if err := e.result(r, status, stage, version); err != nil {
			return err
		}
		completed := j
		completed.Running = false
		if err := WriteJSON(filepath.Join(e.ControlDir, "journal.json"), completed, 0600); err != nil {
			return err
		}
		j = completed
		return nil
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
	// Retain the previous fallback until the new candidate is authenticated and
	// locally runnable. The current binary becomes the rollback link next.
	if _, err := os.Lstat(backup); err == nil {
		if err := os.Remove(backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	free, err = freeBytes(e.InstallDir)
	if err != nil {
		return fmt.Errorf("recheck updater disk space: %w", err)
	}
	if free < reserve {
		return fmt.Errorf("insufficient disk reserve after download: need %d bytes, have %d", reserve, free)
	}
	// Both files are fsynced before replacing the current executable. The journal
	// and previous binary remain available after a crash or reboot.
	// Persist intent before the rename. Recovery accepts a missing backup in
	// this narrow window (the current executable is still untouched).
	j.BackupReady = true
	if err := WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600); err != nil {
		return err
	}
	if err := os.Link(filepath.Join(e.InstallDir, "vpsagent"), backup); err != nil {
		j.BackupReady = false
		_ = WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600)
		return err
	}
	if err := syncDirectory(e.InstallDir); err != nil {
		return err
	}
	if err := replaceExecutable(candidate, filepath.Join(e.InstallDir, "vpsagent")); err != nil {
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
	if e.RefreshHelpers != nil {
		if err := e.RefreshHelpers(ctx); err != nil {
			return err
		}
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

func replaceExecutable(source, target string) error {
	if err := trustedPath(source, false); err != nil {
		return err
	}
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return errors.New("executable is not a regular file")
	}
	if err := file.Chmod(0755); err != nil {
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
	if err := os.Rename(source, target); err != nil {
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

func availableBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func cleanAbandoned(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".update-") && !strings.HasPrefix(name, ".binary-") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || (info.IsDir() && info.Mode().Perm()&0077 != 0) || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe abandoned update file: %s", path)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
func (e Engine) Recover(ctx context.Context) error {
	unlock, err := acquireUpdateLock(ctx, e.ControlDir)
	if err != nil {
		return err
	}
	defer unlock()
	return e.recover(ctx)
}

func (e Engine) recover(ctx context.Context) error {
	if err := trustedPath(e.InstallDir, true); err != nil {
		return err
	}
	if err := trustedPath(e.ControlDir, true); err != nil {
		return err
	}
	if err := cleanAbandoned(e.InstallDir); err != nil {
		return err
	}
	var j journal
	if err := ReadJSON(filepath.Join(e.ControlDir, "journal.json"), &j); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !j.Running {
		return nil
	}
	backup := filepath.Join(e.InstallDir, "vpsagent.previous")
	status := "failed"
	if !idPattern.MatchString(j.Request.ID) || !ValidVersion(j.OldVersion) || !ValidVersion(j.Request.TargetVersion) {
		return errors.New("invalid recovery journal")
	}
	if j.BackupReady {
		hadBackup := false
		if _, err := os.Lstat(backup); err == nil {
			hadBackup = true
			if err := trustedPath(backup, false); err != nil {
				return err
			}
			version, err := executableVersion(ctx, backup)
			if err != nil || version != j.OldVersion {
				return errors.New("invalid recovery binary")
			}
			if err := replaceExecutable(backup, filepath.Join(e.InstallDir, "vpsagent")); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			// Backup intent was journaled but the rename had not happened yet.
			version, versionErr := e.CurrentVersion(ctx)
			if versionErr != nil || version != j.OldVersion {
				return errors.New("missing recovery binary")
			}
		}
		if err := e.Restart(ctx); err != nil {
			return err
		}
		if err := e.Healthy(ctx, j.OldVersion); err != nil {
			return err
		}
		if hadBackup {
			status = "rolled_back"
		}
	}
	if err := e.result(j.Request, status, "restart_failed", j.OldVersion); err != nil {
		return err
	}
	j.Running = false
	return WriteJSON(filepath.Join(e.ControlDir, "journal.json"), j, 0600)
}
