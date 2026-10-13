package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxClaimResponseBytes = 16 << 10
	maxCloudTokenBytes    = 4096
)

// Credentials are stored separately from the environment so the durable token
// can be owner-only and is not accidentally copied into process listings.
type Credentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// Connect exchanges a short-lived setup token for a server token and stores it
// atomically with mode 0600. It never prints or logs the permanent token.
func Connect(ctx context.Context, cloudURL, setupToken, destination string, allowInsecure bool) error {
	endpoint, err := parseCloudURL(cloudURL, allowInsecure)
	if err != nil {
		return err
	}
	if !validCloudToken(setupToken) {
		return fmt.Errorf("setup token is required")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/agents/claim"
	body, _ := json.Marshal(struct {
		Hostname string `json:"hostname"`
	}{Hostname: hostname})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+setupToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := cloudHTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return fmt.Errorf("claim server: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("claim server: %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxClaimResponseBytes+1))
	if err != nil {
		return fmt.Errorf("claim server: read response: %w", err)
	}
	if len(data) > maxClaimResponseBytes {
		return errors.New("claim server: oversized response")
	}
	var claimed struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&claimed); err != nil || !validCloudToken(claimed.Token) {
		return fmt.Errorf("claim server: invalid response")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("claim server: invalid response")
	}
	return writeCredentials(destination, Credentials{URL: strings.TrimRight(cloudURL, "/"), Token: claimed.Token})
}

func validCloudToken(token string) bool {
	if token == "" || len(token) > maxCloudTokenBytes || strings.TrimSpace(token) != token {
		return false
	}
	for _, r := range token {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func writeCredentials(destination string, credentials Credentials) error {
	data, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := destination + fmt.Sprintf(".tmp-%x", suffix)
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(destination, 0600)
}

// LoadCredentials reads credentials previously created by Connect.
func LoadCredentials(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode cloud credentials: %w", err)
	}
	if credentials.URL == "" || credentials.Token == "" {
		return Credentials{}, fmt.Errorf("cloud credentials are incomplete")
	}
	return credentials, nil
}
