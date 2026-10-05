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
