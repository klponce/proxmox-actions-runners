package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestConvertManifest(t *testing.T) {
	f := newFakeGitHub(t)

	app, key, err := convertManifest(context.Background(), f.httpClient(), f.srv.URL, testManifestCode)
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}
	if want := (App{ID: testAppID, Slug: testAppSlug, ClientID: testClientID, Owner: "my-org",
		OwnerType: "Organization"}); app != want {
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
	tokenRoute := fmt.Sprintf("POST /app/installations/%d/access_tokens", testInstallationID)

	tests := []struct {
		name       string
		on         string
		key        string
		want       Installation
		wantErr    string
		wantRoutes []string
	}{
		{name: "organization", on: "Organization",
			want:       Installation{ID: testInstallationID, Account: "my-org", AccountType: "Organization"},
			wantRoutes: []string{"GET /app/installations"}},
		{name: "personal account, with its repositories", on: "User",
			want: Installation{ID: testInstallationID, Account: "octocat", AccountType: "User",
				Repositories: []string{"hello", "world"}},
			wantRoutes: []string{"GET /app/installations", tokenRoute, "GET /installation/repositories"}},
		{name: "not installed yet", wantRoutes: []string{"GET /app/installations"}},
		{name: "wrong key", on: "Organization", key: otherPEM, wantErr: "401",
			wantRoutes: []string{"GET /app/installations"}},
		{name: "key not PEM", key: "not a key", wantErr: "not a valid RSA key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.installedOn = tt.on
			key := tt.key
			if key == "" {
				key = testKeyPEM()
			}
			got, err := findInstallation(context.Background(), f.httpClient(), f.srv.URL, testClientID, key)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("FindInstallation error = %v, want it to mention %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "PRIVATE KEY") {
					t.Errorf("error leaks the key: %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FindInstallation = %+v, %v; want %+v", got, err, tt.want)
			}
			if calls := f.called(); !slices.Equal(calls, tt.wantRoutes) {
				t.Errorf("requests = %v, want %v", calls, tt.wantRoutes)
			}
		})
	}
}
