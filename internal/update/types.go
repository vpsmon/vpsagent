// Package update implements a fixed-purpose, signed release updater. It accepts
// only a release version and request ID, never a command, URL or filesystem path.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

const ControlDir = "/var/lib/vpsagent-control"
const InstallDir = "/opt/vpsagent"

var Version = "dev"
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)
var idPattern = regexp.MustCompile(`^upd_[A-Za-z0-9_-]{1,60}$`)

func ValidVersion(v string) bool { return versionPattern.MatchString(v) }
func Newer(a, b string) bool {
	if !ValidVersion(a) || !ValidVersion(b) {
		return false
	}
	var av, bv [3]int
	fmt.Sscanf(a, "v%d.%d.%d", &av[0], &av[1], &av[2])
	fmt.Sscanf(b, "v%d.%d.%d", &bv[0], &bv[1], &bv[2])
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

type Request struct {
	ID            string    `json:"id"`
	TargetVersion string    `json:"target_version"`
	ExpiresAt     time.Time `json:"expires_at"`
}
type Result struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Stage   string `json:"stage,omitempty"`
	Version string `json:"version"`
}

func Enabled(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, "enabled"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}
func ReadJSON(path string, out any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return errors.New("invalid update state file")
	}
	dec := json.NewDecoder(io.LimitReader(file, 4097))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing update state")
	}
	return nil
}
func WriteJSON(path string, value any, mode os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// Submit runs as the unprivileged agent. The systemd path unit observes the
// rename; only the separate root service replaces binaries and restarts services.
func Submit(dir string, request Request) error {
	if !idPattern.MatchString(request.ID) || !ValidVersion(request.TargetVersion) || !time.Now().Before(request.ExpiresAt) {
		return errors.New("invalid update request")
	}
	var previous Request
	path := filepath.Join(dir, "requests", "update.json")
	if ReadJSON(path, &previous) == nil && previous.ID == request.ID {
		return nil
	}
	return WriteJSON(path, request, 0600)
}
