package update

import (
	"context"
	"errors"
	"github.com/theupdateframework/go-tuf/v2/metadata"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Explicit release smoke test: use a Linux host to check the published binary,
// real Sigstore identity, transparency evidence and signed asset digest.
func TestPublishedSignedRelease(t *testing.T) {
	version := os.Getenv("VPSAGENT_VERIFY_RELEASE")
	if version == "" {
		t.Skip("requires published release")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	manifest, err := download(ctx, releaseBase+version+"/release.json", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := download(ctx, releaseBase+version+"/release.json.sigstore.json", 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyManifestContext(ctx, manifest, signature, version); err != nil {
		t.Fatal("manifest verification", err)
	}
	path, err := FetchRelease(ctx, version, t.TempDir(), func(data, proof []byte, v string) error { return verifyManifestContext(ctx, data, proof, v) })
	if err != nil {
		t.Fatal(err)
	}
	actual, err := executableVersion(ctx, path)
	if err != nil || actual != version {
		t.Fatal("signed executable version", actual, err)
	}
	data, err := download(ctx, releaseBase+version+"/release.json", 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := download(ctx, releaseBase+version+"/release.json.sigstore.json", 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, ' ')
	data[0] = '['
	if err := verifyManifestContext(ctx, data, proof, version); err == nil {
		t.Fatal("accepted tampered manifest")
	}
	t.Log("published release signature, executable version and tamper rejection verified")
}

func TestTUFFetcherPreservesMissingMetadataStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer server.Close()
	_, err := tufFetcher(t.Context()).DownloadFile(server.URL+"/next-root.json", 1024, time.Second)
	var status *metadata.ErrDownloadHTTP
	if !errors.As(err, &status) || status.StatusCode != 404 {
		t.Fatalf("TUF needs typed missing-root status, got %v", err)
	}
}
