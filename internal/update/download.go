package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

const releaseBase = "https://github.com/vpsmon/vpsagent/releases/download/"
const maxBinarySize = int64(128 << 20)

type Manifest struct {
	Version string            `json:"version"`
	Assets  map[string]string `json:"assets"`
}

func VerifyManifest(artifact, proof []byte, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return verifyManifestContext(ctx, artifact, proof, version)
}

type contextHTTPClient struct {
	ctx    context.Context
	client *http.Client
}

func (c contextHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return c.client.Do(req.WithContext(c.ctx))
}
func tufFetcher(ctx context.Context) *fetcher.DefaultFetcher {
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPClient(contextHTTPClient{ctx: ctx, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("unsafe TUF redirect")
		}
		return nil
	}}})
	return f
}
func verifyManifestContext(ctx context.Context, artifact, proof []byte, version string) error {
	if !ValidVersion(version) {
		return errors.New("invalid release")
	}
	var signed bundle.Bundle
	if err := signed.UnmarshalJSON(proof); err != nil {
		return err
	}
	opts := tuf.DefaultOptions()
	opts.Fetcher = tufFetcher(ctx)
	opts.CachePath = "/var/cache/vpsagent-update/sigstore"
	trusted, err := root.FetchTrustedRootWithOptions(opts)
	if err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithSignedCertificateTimestamps(1), verify.WithObserverTimestamps(1), verify.WithTransparencyLog(1))
	if err != nil {
		return err
	}
	identity, err := verify.NewShortCertificateIdentity("https://token.actions.githubusercontent.com", "", "", `^https://github\.com/vpsmon/vpsagent/\.github/workflows/build\.yml@refs/(heads/main|tags/`+regexp.QuoteMeta(version)+`)$`)
	if err != nil {
		return err
	}
	_, err = verifier.Verify(&signed, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(artifact)), verify.WithCertificateIdentity(identity)))
	return err
}
func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("unsafe update redirect")
		}
		return nil
	}}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("release download failed")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release download exceeds limit")
	}
	return data, nil
}

// streamDownload writes a bounded response directly to a newly created private
// file while hashing it. It never retains release binary bytes in memory.
func streamDownload(ctx context.Context, url, path string, limit int64) (string, int64, error) {
	return streamDownloadWithClient(ctx, url, path, limit, updateHTTPClient())
}

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("unsafe update redirect")
		}
		return nil
	}}
}

func streamDownloadWithClient(ctx context.Context, url, path string, limit int64, client *http.Client) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	if req.URL.Scheme != "https" {
		return "", 0, errors.New("unsafe update URL")
	}
	res, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", 0, errors.New("release download failed")
	}
	if res.ContentLength > limit {
		return "", 0, errors.New("release download exceeds limit")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", 0, err
	}
	keep := false
	defer func() {
		file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", count, err
	}
	if count > limit {
		return "", count, errors.New("release download exceeds limit")
	}
	if err := file.Sync(); err != nil {
		return "", count, err
	}
	if err := file.Chmod(0700); err != nil {
		return "", count, err
	}
	if err := file.Sync(); err != nil {
		return "", count, err
	}
	if err := file.Close(); err != nil {
		return "", count, err
	}
	keep = true
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func streamVerifiedDownload(ctx context.Context, url, path string, limit int64, expected string) error {
	return streamVerifiedDownloadWithClient(ctx, url, path, limit, expected, updateHTTPClient())
}

func streamVerifiedDownloadWithClient(ctx context.Context, url, path string, limit int64, expected string, client *http.Client) error {
	digest, _, err := streamDownloadWithClient(ctx, url, path, limit, client)
	if err != nil {
		return err
	}
	if digest != expected {
		_ = os.Remove(path)
		return errors.New("release checksum mismatch")
	}
	return nil
}
func FetchRelease(ctx context.Context, version, directory string, verifyManifest func([]byte, []byte, string) error) (string, error) {
	if !ValidVersion(version) || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" && runtime.GOARCH != "arm" && runtime.GOARCH != "386") {
		return "", errors.New("unsupported agent release")
	}
	data, err := download(ctx, releaseBase+version+"/release.json", 64<<10)
	if err != nil {
		return "", err
	}
	proof, err := download(ctx, releaseBase+version+"/release.json.sigstore.json", 256<<10)
	if err != nil {
		return "", err
	}
	if err := verifyManifest(data, proof, version); err != nil {
		return "", errors.New("release signature verification failed")
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version != version {
		return "", errors.New("release version mismatch")
	}
	name := "vpsagent-linux-" + runtime.GOARCH
	expected := manifest.Assets[name]
	if len(expected) != 64 || strings.Trim(expected, "0123456789abcdef") != "" {
		return "", errors.New("invalid release checksum")
	}
	path := filepath.Join(directory, "vpsagent")
	// Fetch into a private staging directory; a corrupt or oversized artifact never
	// replaces the executable. The manifest signature authenticates this checksum.
	if err := streamVerifiedDownload(ctx, releaseBase+version+"/"+name, path, maxBinarySize, expected); err != nil {
		return "", err
	}
	return path, nil
}
