package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
)

const (
	testCode     = "a1b2c3d4e5f6a1b2c3d4"
	testClientID = "Iv23liEXAMPLE0000000"
	testTarget   = "https://github.com/my-org"
)

var testKeyPEM = func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}()

// runApp runs parcon with stdin and returns its outcome and everything it wrote or returned, which must never hold
// the manifest code or the key.
func runApp(t *testing.T, input string, args ...string) (outcome, string, string) {
	t.Helper()
	old := stdin
	t.Cleanup(func() { stdin = old })
	stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	all := stdout.String() + stderr.String()
	if err != nil {
		all += err.Error()
	}
	if strings.Contains(all, testCode) || strings.Contains(all, "PRIVATE KEY") {
		t.Errorf("output leaks a secret:\n%s", all)
	}
	return outcomeOf(err), stdout.String(), all
}

// fakeConvert replaces convertManifest with one that accepts only testCode, and counts its calls.
func fakeConvert(t *testing.T) *int {
	t.Helper()
	old := convertManifest
	t.Cleanup(func() { convertManifest = old })
	calls := 0
	convertManifest = func(_ context.Context, code string) (github.App, string, error) {
		calls++
		if code != testCode {
			return github.App{}, "", errors.New("github: convert the App manifest code: 404 Not Found: the code is " +
				"invalid, already used, or more than an hour old")
		}
		return github.App{ID: 123456, Slug: "par-my-org-a1b2c3", ClientID: testClientID}, testKeyPEM, nil
	}
	return &calls
}

// checkKeyFile checks that path holds the test key with mode 0600 and that no temporary file was left beside it.
func checkKeyFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %04o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != testKeyPEM {
		t.Errorf("key file doesn't hold the key (%v)", err)
	}
	checkDir(t, filepath.Dir(path), filepath.Base(path))
}

// checkDir checks that dir holds exactly the named files.
func checkDir(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

func TestGitHubAppCreate(t *testing.T) {
	calls := fakeConvert(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "github-app.pem")
	// An older key with a loose mode is replaced.
	if err := os.WriteFile(key, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, stdout, all := runApp(t, "  "+testCode+"\n", "github", "app", "create", "-key-file", key)
	if got != succeeded {
		t.Fatalf("outcome %s:\n%s", got, all)
	}
	if want := `{"clientId":"Iv23liEXAMPLE0000000","appId":123456,"slug":"par-my-org-a1b2c3"}` + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if *calls != 1 {
		t.Errorf("convertManifest called %d times, want 1", *calls)
	}
	checkKeyFile(t, key)
}

func TestGitHubAppCreateFailures(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		keyDir    string // relative to a temporary directory
		want      outcome
		msg       string
		wantCalls int
	}{
		{"invalid or expired code", "0123456789abcdef0123", "", failed, "the code is invalid, already used", 1},
		{"no code", "\n", "", failed, "no manifest code on stdin", 0},
		// The code is single-use, so it isn't spent when the key can't be written.
		{"key directory missing", testCode, "missing", failed, "write ", 0},
		{"extra argument", testCode, "", usageError, "unexpected arguments", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := fakeConvert(t)
			dir := t.TempDir()
			args := []string{"github", "app", "create", "-key-file", filepath.Join(dir, tt.keyDir, "github-app.pem")}
			if tt.want == usageError {
				args = append(args, "extra")
			}
			got, _, all := runApp(t, tt.input, args...)
			if got != tt.want || !strings.Contains(all, tt.msg) {
				t.Errorf("outcome %s, want %s mentioning %q:\n%s", got, tt.want, tt.msg, all)
			}
			if *calls != tt.wantCalls {
				t.Errorf("convertManifest called %d times, want %d", *calls, tt.wantCalls)
			}
			checkDir(t, dir)
		})
	}
}

func TestGitHubAppImport(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "github-app.pem")
	got, stdout, all := runApp(t, testKeyPEM, "github", "app", "import", "-key-file", key)
	if got != succeeded || stdout != "" {
		t.Fatalf("outcome %s, stdout %q:\n%s", got, stdout, all)
	}
	checkKeyFile(t, key)

	other := t.TempDir()
	got, _, all = runApp(t, "not a key", "github", "app", "import", "-key-file", filepath.Join(other, "github-app.pem"))
	if got != failed || !strings.Contains(all, "not PEM-encoded") {
		t.Errorf("import of a bad key: outcome %s:\n%s", got, all)
	}
	checkDir(t, other)
}

func TestGitHubAppWaitInstallation(t *testing.T) {
	oldFind, oldInterval := findInstallation, installationPollInterval
	t.Cleanup(func() { findInstallation, installationPollInterval = oldFind, oldInterval })
	installationPollInterval = time.Millisecond

	dir := t.TempDir()
	key := filepath.Join(dir, "github-app.pem")
	if err := os.WriteFile(key, []byte(testKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	looseKey := filepath.Join(dir, "loose.pem")
	if err := os.WriteFile(looseKey, []byte(testKeyPEM), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		args     []string
		pollsTo  int   // the poll that finds the installation; 0 means never
		findErr  error // returned by every poll before it
		want     outcome
		wantOut  string
		wantMsgs string
	}{
		{name: "installed after a few polls", pollsTo: 3, want: succeeded, wantOut: "7890123\n"},
		{name: "timeout", args: []string{"-timeout", "20ms"}, want: failed,
			wantMsgs: "the GitHub App isn't installed on https://github.com/my-org after 20ms"},
		{name: "installed after transient errors", pollsTo: 3, findErr: errors.New("github: find the App's " +
			"installation: GET /orgs/my-org/installation: 502 Bad Gateway"), want: succeeded, wantOut: "7890123\n",
			wantMsgs: "502 Bad Gateway; still waiting"},
		{name: "bad credentials until the timeout", args: []string{"-timeout", "20ms"}, findErr: errors.New("github: " +
			"find the App's installation: GET /orgs/my-org/installation: 401 Unauthorized"), want: failed,
			wantMsgs: "after 20ms; last error: github: find the App's installation: GET /orgs/my-org/installation: " +
				"401 Unauthorized"},
		{name: "key readable by others", args: []string{"-key-file", looseKey}, want: failed,
			wantMsgs: "must not be accessible to group or others"},
		{name: "no target", args: []string{"-target", ""}, want: usageError,
			wantMsgs: "-client-id and -target are required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			polls := 0
			findInstallation = func(_ context.Context, clientID, keyPEM, target string) (int64, error) {
				polls++
				if clientID != testClientID || keyPEM != strings.TrimSpace(testKeyPEM) || target != testTarget {
					t.Errorf("findInstallation(%q, <key>, %q) got the wrong arguments", clientID, target)
				}
				if tt.pollsTo != 0 && polls >= tt.pollsTo {
					return 7890123, nil
				}
				return 0, tt.findErr
			}
			args := append([]string{"github", "app", "wait-installation", "-client-id", testClientID, "-target",
				testTarget, "-key-file", key}, tt.args...)
			got, stdout, all := runApp(t, "", args...)
			if got != tt.want || !strings.Contains(all, tt.wantMsgs) || stdout != tt.wantOut {
				t.Errorf("outcome %s, stdout %q, want %s and %q mentioning %q:\n%s", got, stdout, tt.want, tt.wantOut,
					tt.wantMsgs, all)
			}
			if tt.pollsTo != 0 && polls != tt.pollsTo {
				t.Errorf("polled %d times, want %d", polls, tt.pollsTo)
			}
			// The same error is shown once, not on every poll.
			if tt.findErr != nil && strings.Count(all, "still waiting") != 1 {
				t.Errorf("the repeated error was shown %d times, want once:\n%s", strings.Count(all, "still waiting"),
					all)
			}
		})
	}
}

func TestGitHubUsage(t *testing.T) {
	for _, args := range [][]string{{"github"}, {"github", "app"}, {"github", "app", "frobnicate"},
		{"github", "x", "create"}} {
		var stdout, stderr bytes.Buffer
		err := run(args, &stdout, &stderr)
		if outcomeOf(err) != usageError || !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("%v: error = %v, stderr %q", args, err, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"github", "app", "create", "-h"}, &stdout, &stderr); err != nil ||
		!strings.Contains(stderr.String(), "-key-file") {
		t.Errorf("create -h: error = %v, stderr %q", err, stderr.String())
	}
}
