package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
)

func TestConvertManifest(t *testing.T) {
	f := newFakeGitHub(t)

	app, key, err := convertManifest(context.Background(), f.httpClient(), f.srv.URL, testManifestCode)
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}
	if want := (App{ID: testAppID, Slug: testAppSlug, ClientID: testClientID}); app != want {
		t.Errorf("App = %+v, want %+v", app, want)
	}
	if key != testKeyPEM() {
		t.Error("ConvertManifest didn't return the App's private key")
	}
	want := "POST /app-manifests/" + testManifestCode + "/conversions"
	if got := f.called(); len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v, want %q", got, want)
	}
}

func TestConvertManifestErrors(t *testing.T) {
	const code = "0123456789abcdef0123"
	tests := []struct {
		name   string
		client func(f *fakeGitHub) *http.Client
		want   string
	}{
		{"invalid or expired code", (*fakeGitHub).httpClient,
			"the code is invalid, already used, or more than an hour old"},
		// A transport error quotes the URL, which holds the code.
		{"unreachable", func(*fakeGitHub) *http.Client { return &http.Client{} }, "certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			_, key, err := convertManifest(context.Background(), tt.client(f), f.srv.URL, code)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ConvertManifest error = %v, want it to mention %q", err, tt.want)
			}
			if key != "" {
				t.Error("ConvertManifest returned a key with an error")
			}
			if strings.Contains(err.Error(), code) {
				t.Errorf("error leaks the code: %v", err)
			}
		})
	}
}

func TestFindInstallation(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(otherKey)}))

	// Which URLs name an organization or a repository is config.ParseGitHubURL's job, tested there.
	tests := []struct {
		name        string
		owner, repo string
		key         string
		installed   bool
		want        int64
		wantErr     string
		wantRoute   string
	}{
		{name: "organization", owner: "my-org", installed: true, want: testInstallationID,
			wantRoute: "GET /orgs/my-org/installation"},
		{name: "repository", owner: "my-org", repo: "my-repo", installed: true, want: testInstallationID,
			wantRoute: "GET /repos/my-org/my-repo/installation"},
		{name: "not installed yet", owner: "my-org", wantRoute: "GET /orgs/my-org/installation"},
		{name: "wrong key", owner: "my-org", key: otherPEM, installed: true, wantErr: "401",
			wantRoute: "GET /orgs/my-org/installation"},
		{name: "key not PEM", owner: "my-org", key: "not a key", wantErr: "not a valid RSA key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.installed = tt.installed
			key := tt.key
			if key == "" {
				key = testKeyPEM()
			}
			got, err := findInstallation(context.Background(), f.httpClient(), f.srv.URL, testClientID, key, tt.owner,
				tt.repo)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("FindInstallation error = %v, want it to mention %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "PRIVATE KEY") {
					t.Errorf("error leaks the key: %v", err)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("FindInstallation = %d, %v; want %d", got, err, tt.want)
			}
			if calls := f.called(); tt.wantRoute == "" && len(calls) > 0 ||
				tt.wantRoute != "" && (len(calls) != 1 || calls[0] != tt.wantRoute) {
				t.Errorf("requests = %v, want %q", calls, tt.wantRoute)
			}
		})
	}
}
