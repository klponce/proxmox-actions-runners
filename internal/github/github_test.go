package github

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRejectsBadOptions(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8RSA, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}

	valid := Options{ConfigURL: "https://github.com/my-org", ClientID: testClientID, InstallationID: 1,
		PrivateKeyPEM: testKeyPEM()}
	tests := []struct {
		name   string
		modify func(*Options)
		want   string // empty means New succeeds
	}{
		{"valid PKCS #1 key", func(*Options) {}, ""},
		{"valid PKCS #8 RSA key", func(o *Options) {
			o.PrivateKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8RSA}))
		}, ""},
		{"no config URL", func(o *Options) { o.ConfigURL = "" }, "config URL is required"},
		{"bad config URL", func(o *Options) { o.ConfigURL = "https://github.com/" }, "invalid config URL"},
		{"no client ID", func(o *Options) { o.ClientID = "" }, "client ID is required"},
		{"no installation ID", func(o *Options) { o.InstallationID = 0 }, "installation ID is required"},
		{"key not PEM", func(o *Options) { o.PrivateKeyPEM = "not a key" }, "not PEM-encoded"},
		{"key corrupt", func(o *Options) {
			o.PrivateKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("junk")}))
		}, "not a valid RSA key"},
		{"EC key", func(o *Options) {
			o.PrivateKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
		}, "must be an RSA key"},
		{"public key", func(o *Options) {
			o.PrivateKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}))
		}, `PEM type "PUBLIC KEY"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.modify(&opts)
			_, err := New(opts)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY-----") {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}

func TestEnsureScaleSetCreates(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	got, err := c.EnsureScaleSet(context.Background(), ScaleSetSpec{Name: "proxmox-ubuntu-26.04", Labels: []string{"proxmox-ubuntu-26.04"},
		RunnerGroup: "default"})
	if err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}
	want := ScaleSet{ID: testScaleSetID, Name: "proxmox-ubuntu-26.04", RunnerGroupID: 1,
		Labels: []string{"proxmox-ubuntu-26.04"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnsureScaleSet = %+v, want %+v", got, want)
	}
	stored := f.scaleSets["proxmox-ubuntu-26.04"]
	if setting, _ := stored["RunnerSetting"].(map[string]any); setting["disableUpdate"] != true {
		t.Errorf("runner updates aren't disabled: %v", stored["RunnerSetting"])
	}

	// The token flow ran once: App JWT, then registration token, then the Actions service connection.
	calls := strings.Join(f.called(), "\n")
	for _, want := range []string{"access_tokens", "registration-token", "runner-registration"} {
		if strings.Count(calls, want) != 1 {
			t.Errorf("%s called %d times, want once:\n%s", want, strings.Count(calls, want), calls)
		}
	}
}

func TestEnsureScaleSetUpdatesOnlyWhenNeeded(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	ctx := context.Background()
	spec := ScaleSetSpec{Name: "proxmox", Labels: []string{"proxmox", "x64"}, RunnerGroup: "default"}
	if _, err := c.EnsureScaleSet(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}

	countPatches := func() int {
		n := 0
		for _, call := range f.called() {
			if strings.HasPrefix(call, "PATCH ") {
				n++
			}
		}
		return n
	}

	// Same labels in another order and case: nothing to do.
	if _, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "proxmox", Labels: []string{"X64", "proxmox"}, RunnerGroup: "default"}); err != nil {
		t.Fatalf("ensure unchanged: %v", err)
	}
	if n := countPatches(); n != 0 {
		t.Errorf("unchanged scale set was patched %d times", n)
	}

	got, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "proxmox", Labels: []string{"proxmox", "arm64"}, RunnerGroup: "default"})
	if err != nil {
		t.Fatalf("ensure changed: %v", err)
	}
	if n := countPatches(); n != 1 {
		t.Errorf("changed scale set was patched %d times, want once", n)
	}
	if !reflect.DeepEqual(got.Labels, []string{"proxmox", "arm64"}) {
		t.Errorf("labels = %v", got.Labels)
	}
}

func TestRunnerGroups(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	ctx := context.Background()

	s, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "trusted", Labels: []string{"trusted"}, RunnerGroup: "builds"})
	if err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}
	if s.RunnerGroupID != 3 {
		t.Errorf("runner group ID = %d, want 3", s.RunnerGroupID)
	}
	found, err := c.FindScaleSet(ctx, ScaleSetSpec{Name: "trusted", RunnerGroup: "builds"})
	if err != nil || found == nil || found.ID != testScaleSetID {
		t.Errorf("FindScaleSet = %+v, %v", found, err)
	}
	missing, err := c.FindScaleSet(ctx, ScaleSetSpec{Name: "trusted", RunnerGroup: "default"})
	if err != nil || missing != nil {
		t.Errorf("FindScaleSet in the default group = %+v, %v; want nil, nil", missing, err)
	}
	if _, err := c.EnsureScaleSet(ctx, ScaleSetSpec{Name: "x", RunnerGroup: "no-such-group"}); err == nil ||
		!strings.Contains(err.Error(), `runner group "no-such-group"`) {
		t.Errorf("unknown runner group error = %v", err)
	}
}

func TestDeleteScaleSet(t *testing.T) {
	f := newFakeGitHub(t)
	if err := f.client().DeleteScaleSet(context.Background(), testScaleSetID); err != nil {
		t.Fatalf("DeleteScaleSet: %v", err)
	}
}

func TestBadAppCredentials(t *testing.T) {
	f := newFakeGitHub(t)
	f.failAccessToken = true
	_, err := f.client().FindScaleSet(context.Background(), ScaleSetSpec{Name: "proxmox", RunnerGroup: "default"})
	if err == nil || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("FindScaleSet error = %v, want an access token failure", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("error leaks the key: %v", err)
	}
}

func TestGenerateJITConfig(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	ctx := context.Background()

	jit, err := c.GenerateJITConfig(ctx, testScaleSetID, "par-w-10005")
	if err != nil {
		t.Fatalf("GenerateJITConfig: %v", err)
	}
	if jit.RunnerID != 100 || jit.RunnerName != "par-w-10005" || jit.Encoded() != testJITConfig {
		t.Errorf("JITConfig = %v, encoded %q", jit, jit.Encoded())
	}
}

func TestJITConfigNeverPrintsTheSecret(t *testing.T) {
	jit := JITConfig{RunnerID: 7, RunnerName: "par-w-1", encoded: testJITConfig}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("generated", slog.Any("jit", jit))
	logger.Info("generated", "jit", jit) //nolint:sloglint // key-value logging must redact too.

	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", jit),
		"%+v":  fmt.Sprintf("%+v", jit),
		"%#v":  fmt.Sprintf("%#v", jit),
		"%s":   fmt.Sprintf("%s", jit), //nolint:staticcheck // S1025: the %s verb itself is under test.
		"slog": logs.String(),
	} {
		if strings.Contains(out, testJITConfig) {
			t.Errorf("%s output leaks the JIT config: %s", name, out)
		}
		if !strings.Contains(out, "par-w-1") {
			t.Errorf("%s output lost the runner name: %s", name, out)
		}
	}
}

func TestRunners(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client()
	ctx := context.Background()
	if _, err := c.GenerateJITConfig(ctx, testScaleSetID, "par-w-1"); err != nil {
		t.Fatal(err)
	}

	r, err := c.RunnerByName(ctx, "par-w-1")
	if err != nil || r == nil || *r != (Runner{ID: 100, Name: "par-w-1", ScaleSetID: testScaleSetID}) {
		t.Errorf("RunnerByName = %+v, %v", r, err)
	}
	if r, err := c.RunnerByName(ctx, "par-w-2"); err != nil || r != nil {
		t.Errorf("RunnerByName(missing) = %+v, %v; want nil, nil", r, err)
	}

	tests := []struct {
		id      int64
		wantErr error
	}{
		{100, nil},
		{404, nil}, // already gone
		{409, ErrJobStillRunning},
	}
	for _, tt := range tests {
		err := c.RemoveRunner(ctx, tt.id)
		switch {
		case tt.wantErr == nil && err != nil:
			t.Errorf("RemoveRunner(%d): %v", tt.id, err)
		case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
			t.Errorf("RemoveRunner(%d) error = %v, want %v", tt.id, err, tt.wantErr)
		}
	}
}

// recordingHandler records what the listener reports and cancels the listen context once done says so.
type recordingHandler struct {
	mu        sync.Mutex
	desired   []int
	started   []Job
	completed []Job
	stats     []Stats
	cancel    context.CancelFunc
	done      func(*recordingHandler) bool
}

func (h *recordingHandler) check() {
	if h.done(h) {
		h.cancel()
	}
}

func (h *recordingHandler) DesiredRunners(_ context.Context, assigned int) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.desired = append(h.desired, assigned)
	h.check()
	return assigned, nil
}

func (h *recordingHandler) JobStarted(_ context.Context, job Job) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, job)
	return nil
}

func (h *recordingHandler) JobCompleted(_ context.Context, job Job) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completed = append(h.completed, job)
	return nil
}

func (h *recordingHandler) RecordStats(s Stats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stats = append(h.stats, s)
}

func TestListen(t *testing.T) {
	f := newFakeGitHub(t)
	base := map[string]any{"runnerRequestId": 9001, "jobId": "job-1", "ownerName": "my-org",
		"repositoryName": "my-repo", "jobWorkflowRef": "my-org/my-repo/.github/workflows/ci.yml@refs/heads/main",
		"jobDisplayName": "build"}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	f.queue = []queueReply{
		{status: http.StatusOK, message: jobMessage(1, 1,
			with(map[string]any{"messageType": "JobAvailable", "acquireJobUrl": "https://example.com/acquire"}),
			with(map[string]any{"messageType": "JobStarted", "runnerId": 100, "runnerName": "par-w-1"}),
			with(map[string]any{"messageType": "JobCompleted", "runnerId": 100, "runnerName": "par-w-1",
				"result": "succeeded"}),
		)},
		{status: http.StatusAccepted},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := &recordingHandler{cancel: cancel, done: func(h *recordingHandler) bool { return len(h.desired) >= 3 }}
	f.client().Listen(ctx, ListenOptions{ScaleSetID: testScaleSetID, MaxRunners: 3, Owner: "controller"}, h)
	if ctx.Err() != context.Canceled {
		t.Fatalf("Listen returned before the handler finished: %v", ctx.Err())
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// The session's own statistics (2 assigned), then the message's (1), then the empty poll's (still 1).
	if !reflect.DeepEqual(h.desired[:3], []int{2, 1, 1}) {
		t.Errorf("desired = %v, want [2 1 1 ...]", h.desired)
	}
	wantJob := Job{RunnerRequestID: 9001, JobID: "job-1", RunnerName: "par-w-1", RunnerID: 100, Owner: "my-org",
		Repository: "my-repo", WorkflowRef: "my-org/my-repo/.github/workflows/ci.yml@refs/heads/main",
		DisplayName: "build"}
	if len(h.started) != 1 || h.started[0] != wantJob {
		t.Errorf("started = %+v, want [%+v]", h.started, wantJob)
	}
	wantJob.Result = "succeeded"
	if len(h.completed) != 1 || h.completed[0] != wantJob {
		t.Errorf("completed = %+v, want [%+v]", h.completed, wantJob)
	}
	if len(h.stats) < 2 || h.stats[1].RunningJobs != 1 || h.stats[1].BusyRunners != 1 {
		t.Errorf("stats = %+v", h.stats)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.deleted, []int{1}) {
		t.Errorf("deleted messages = %v, want [1]", f.deleted)
	}
	if !reflect.DeepEqual(f.acquired, [][]int64{{9001}}) {
		t.Errorf("acquired = %v, want [[9001]]", f.acquired)
	}
	if f.sessionsOpen != 1 || f.sessionsClosed != 1 {
		t.Errorf("sessions opened %d, closed %d; want 1 and 1", f.sessionsOpen, f.sessionsClosed)
	}
}

func TestListenReconnects(t *testing.T) {
	f := newFakeGitHub(t)
	f.sessionConflicts = 2 // a stale session from before a crash, say

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := &recordingHandler{cancel: cancel, done: func(h *recordingHandler) bool { return len(h.desired) >= 1 }}
	f.client().Listen(ctx, ListenOptions{ScaleSetID: testScaleSetID, MaxRunners: 1, Owner: "controller",
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}, h)
	if ctx.Err() != context.Canceled {
		t.Fatalf("Listen gave up before connecting: %v", ctx.Err())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionConflicts != 0 || f.sessionsOpen != 1 {
		t.Errorf("conflicts left %d, sessions opened %d; want 0 and 1", f.sessionConflicts, f.sessionsOpen)
	}
}

func TestListenStopsDuringBackoff(t *testing.T) {
	f := newFakeGitHub(t)
	f.sessionConflicts = 1000

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h := &recordingHandler{cancel: cancel, done: func(*recordingHandler) bool { return false }}
	start := time.Now()
	f.client().Listen(ctx, ListenOptions{ScaleSetID: testScaleSetID, Owner: "controller", MinBackoff: time.Hour}, h)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Listen took %s to stop", elapsed)
	}
}
