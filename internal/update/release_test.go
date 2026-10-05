package update

import (
	"context"
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
