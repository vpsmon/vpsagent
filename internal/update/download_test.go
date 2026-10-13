package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamVerifiedDownloadBoundsAndChecksDigest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		limit    int64
		expected string
		wantErr  bool
		chunked  bool
	}{
		{name: "valid", body: "signed binary", limit: 32, expected: func() string { sum := sha256.Sum256([]byte("signed binary")); return hex.EncodeToString(sum[:]) }()},
		{name: "corrupt", body: "tampered", limit: 32, expected: "bad digest", wantErr: true},
		{name: "oversize with content length", body: "more than limit", limit: 4, expected: "", wantErr: true},
		{name: "oversize chunked", body: "more than limit", limit: 4, expected: "", wantErr: true, chunked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "candidate")
			ctx := context.Background()
			client := server.Client()
			err := streamVerifiedDownloadWithClient(ctx, server.URL, path, tc.limit, tc.expected, client)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tc.wantErr {
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Fatalf("rejected artifact remains: %v", statErr)
				}
				return
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tc.body {
				t.Fatalf("artifact = %q, %v", got, err)
			}
		})
	}
}
