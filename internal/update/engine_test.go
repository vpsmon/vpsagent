package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, path, version string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+version+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}
func testEngine(t *testing.T) Engine {
	t.Helper()
	e := Engine{InstallDir: t.TempDir(), ControlDir: t.TempDir()}
	fixture(t, filepath.Join(e.InstallDir, "vpsagent"), "v0.0.8")
	e.CurrentVersion = func(ctx context.Context) (string, error) {
		return executableVersion(ctx, filepath.Join(e.InstallDir, "vpsagent"))
	}
	e.Fetch = func(_ context.Context, v, dir string) (string, error) {
		path := filepath.Join(dir, "vpsagent")
		fixture(t, path, v)
		return path, nil
	}
	e.Restart = func(context.Context) error { return nil }
	e.Healthy = func(ctx context.Context, v string) error {
		actual, err := e.CurrentVersion(ctx)
		if err != nil {
			return err
		}
		if actual != v {
			return errors.New("wrong version")
		}
		return nil
	}
	return e
}
func request() Request {
	return Request{ID: "upd_test", TargetVersion: "v0.0.9", ExpiresAt: time.Now().Add(time.Minute)}
}
func readResult(t *testing.T, e Engine) Result {
	t.Helper()
	var r Result
	if err := ReadJSON(filepath.Join(e.ControlDir, "result.json"), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestApplySuccessAndDuplicate(t *testing.T) {
	e := testEngine(t)
	calls := 0
	e.Restart = func(context.Context) error { calls++; return nil }
	if err := e.Apply(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if r := readResult(t, e); r.Status != "succeeded" || r.Version != "v0.0.9" {
		t.Fatal(r)
	}
	if err := e.Apply(t.Context(), request()); err != nil || calls != 1 {
		t.Fatalf("duplicate: %v restarts=%d", err, calls)
	}
	v, err := executableVersion(t.Context(), filepath.Join(e.InstallDir, "vpsagent.previous"))
	if err != nil || v != "v0.0.8" {
		t.Fatal(v, err)
	}
}
func TestApplyFailedHealthRollsBack(t *testing.T) {
	e := testEngine(t)
	calls := 0
	e.Restart = func(context.Context) error { calls++; return nil }
	healthy := e.Healthy
	e.Healthy = func(ctx context.Context, v string) error {
		if v == "v0.0.9" {
			return errors.New("crash")
		}
		return healthy(ctx, v)
	}
	if err := e.Apply(t.Context(), request()); err == nil {
		t.Fatal("accepted crashed update")
	}
	v, _ := e.CurrentVersion(t.Context())
	r := readResult(t, e)
	if v != "v0.0.8" || r.Status != "rolled_back" || calls != 2 {
		t.Fatal(v, r, calls)
	}
}
func TestVerificationFailureDoesNotInstallOrRestart(t *testing.T) {
	for _, wrongBinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "signature", true: "binary version"}[wrongBinary], func(t *testing.T) {
			e := testEngine(t)
			e.Restart = func(context.Context) error { t.Fatal("restarted rejected release"); return nil }
			e.Fetch = func(_ context.Context, _ string, dir string) (string, error) {
				if !wrongBinary {
					return "", errors.New("bad signature")
				}
				path := filepath.Join(dir, "vpsagent")
				fixture(t, path, "v9.0.0")
				return path, nil
			}
			if err := e.Apply(t.Context(), request()); err == nil {
				t.Fatal("accepted invalid release")
			}
			v, _ := e.CurrentVersion(t.Context())
			if v != "v0.0.8" || readResult(t, e).Status != "failed" {
				t.Fatal(v)
			}
		})
	}
}
func TestRecoveryBeforeBackupKeepsCurrentBinary(t *testing.T) {
	e := testEngine(t)
	fixture(t, filepath.Join(e.InstallDir, "vpsagent.previous"), "v0.0.7")
	e.Restart = func(context.Context) error { t.Fatal("restarted during download recovery"); return nil }
	WriteJSON(filepath.Join(e.ControlDir, "journal.json"), journal{Request: request(), OldVersion: "v0.0.8", Running: true}, 0600)
	if err := e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, _ := e.CurrentVersion(t.Context())
	if v != "v0.0.8" || readResult(t, e).Status != "failed" {
		t.Fatal(v)
	}
}
func TestRecoveryAfterReplacementRestoresKnownBackup(t *testing.T) {
	e := testEngine(t)
	fixture(t, filepath.Join(e.InstallDir, "vpsagent.previous"), "v0.0.8")
	fixture(t, filepath.Join(e.InstallDir, "vpsagent"), "v0.0.9")
	WriteJSON(filepath.Join(e.ControlDir, "journal.json"), journal{Request: request(), OldVersion: "v0.0.8", Running: true, BackupReady: true}, 0600)
	if err := e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, _ := e.CurrentVersion(t.Context())
	if v != "v0.0.8" || readResult(t, e).Status != "rolled_back" {
		t.Fatal(v)
	}
}

func TestRecoveryHandlesBackupIntentBeforeLink(t *testing.T) {
	e := testEngine(t)
	WriteJSON(filepath.Join(e.ControlDir, "journal.json"), journal{Request: request(), OldVersion: "v0.0.8", Running: true, BackupReady: true}, 0600)
	if err := e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	version, err := e.CurrentVersion(t.Context())
	if err != nil || version != "v0.0.8" || readResult(t, e).Status != "failed" {
		t.Fatal(version, err)
	}
}

func TestLowDiskFailsBeforeFetch(t *testing.T) {
	e := testEngine(t)
	e.FreeBytes = func(string) (uint64, error) { return 1, nil }
	e.Fetch = func(context.Context, string, string) (string, error) {
		t.Fatal("downloaded with insufficient free space")
		return "", nil
	}
	if err := e.Apply(t.Context(), request()); err == nil {
		t.Fatal("accepted low disk space")
	}
	version, _ := e.CurrentVersion(t.Context())
	if version != "v0.0.8" {
		t.Fatal("changed installed binary", version)
	}
}

func TestCleanupAbandonedRootOwnedArtifacts(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".update-stale")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, ".binary-stale")
	if err := os.WriteFile(binary, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanAbandoned(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stage, binary} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact remains: %s (%v)", path, err)
		}
	}
}

func TestRecoveryCleansAbandonedStagingWithoutJournal(t *testing.T) {
	e := testEngine(t)
	stage := filepath.Join(e.InstallDir, ".update-abandoned")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned stage remains: %v", err)
	}
}

func TestDownloadSpaceIsRecheckedBeforeReplacement(t *testing.T) {
	e := testEngine(t)
	checks := 0
	e.FreeBytes = func(string) (uint64, error) {
		checks++
		if checks == 1 {
			return uint64(maxBinarySize) + uint64(64<<20), nil
		}
		return 1, nil
	}
	e.Restart = func(context.Context) error { t.Fatal("restarted with no reserve"); return nil }
	if err := e.Apply(t.Context(), request()); err == nil {
		t.Fatal("accepted lost disk reserve")
	}
	version, _ := e.CurrentVersion(t.Context())
	if version != "v0.0.8" || checks != 2 {
		t.Fatal(version, checks)
	}
}

func TestApplySharesManualUpdateLock(t *testing.T) {
	e := testEngine(t)
	unlock, err := acquireUpdateLock(t.Context(), e.ControlDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.Apply(ctx, request()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply did not wait on shared lock: %v", err)
	}
	unlock()
}

func TestSystemdActivationCanAcquireSharedLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireUpdateLock(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	activated := false
	err = runAfterUnlock(unlock, func() error {
		activationUnlock, err := acquireUpdateLock(t.Context(), dir)
		if err != nil {
			return err
		}
		activationUnlock()
		activated = true
		return nil
	})
	unlock() // Enable's deferred cleanup must be harmless after early release.
	if err != nil || !activated {
		t.Fatalf("activation could not acquire updater lock: %v", err)
	}
}

func TestLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireUpdateLock(t.Context(), dir); err == nil {
		t.Fatal("accepted symlink lock")
	}
}

func TestControlDirectoryBecomesTraversableAfterValidation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	owner := uint32(os.Geteuid())
	if err := makeDirectoryTraversable(dir, owner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("mode = %v, err = %v", info.Mode().Perm(), err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := makeDirectoryTraversable(dir, owner); err == nil {
		t.Fatal("made an unsafe writable directory traversable")
	}
	info, err = os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0777 {
		t.Fatalf("unsafe directory mode was changed: %v, %v", info.Mode().Perm(), err)
	}
}
func TestInvalidRequestsNeverFetch(t *testing.T) {
	for _, r := range []Request{{ID: "upd_test", TargetVersion: "../../bin/sh", ExpiresAt: time.Now().Add(time.Minute)}, {ID: "upd_test", TargetVersion: "v0.0.7", ExpiresAt: time.Now().Add(time.Minute)}, {ID: "upd_test", TargetVersion: "v0.0.9", ExpiresAt: time.Now().Add(-time.Minute)}, {ID: "not-an-id", TargetVersion: "v0.0.9", ExpiresAt: time.Now().Add(time.Minute)}} {
		e := testEngine(t)
		e.Fetch = func(context.Context, string, string) (string, error) {
			t.Fatal("fetched invalid request")
			return "", nil
		}
		if err := e.Apply(t.Context(), r); err == nil {
			t.Fatal(r)
		}
	}
}
func TestReadStateRejectsSymlinksLargeFilesAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	WriteJSON(target, request(), 0600)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	var r Request
	if err := ReadJSON(link, &r); err == nil {
		t.Fatal("followed symlink")
	}
	os.WriteFile(target, make([]byte, 4097), 0600)
	if err := ReadJSON(target, &r); err == nil {
		t.Fatal("oversized state")
	}
	os.WriteFile(target, []byte(`{"command":"sh"}`), 0600)
	if err := ReadJSON(target, &r); err == nil {
		t.Fatal("accepted command")
	}
}
func TestSignatureRejectsInvalidBundle(t *testing.T) {
	if err := VerifyManifest([]byte(`{}`), []byte(`{}`), "v0.0.9"); err == nil {
		t.Fatal("accepted unsigned manifest")
	}
}
