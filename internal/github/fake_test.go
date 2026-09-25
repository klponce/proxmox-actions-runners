package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

const (
	testClientID       = "Iv23liEXAMPLE0000000"
	testInstallationID = 7890123
	testScaleSetID     = 42
	testInstallToken   = "ghs_installation_token"
	testRegToken       = "registration_token"
	testQueueToken     = "message_queue_token"
	testSessionID      = "5a1b2c3d-0000-4000-8000-000000000001"
	testJITConfig      = "ZW5jb2RlZC1qaXQtY29uZmlnLXNlY3JldA=="
	testAppID          = 123456
	testAppSlug        = "par-my-org-a1b2c3"
	testManifestCode   = "a1b2c3d4e5f6a1b2c3d4"
	testClientSecret   = "client_secret_value"
	testWebhookSecret  = "webhook_secret_value"
)

// testKey is an RSA key generated once for all tests.
var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func testKeyPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}))
}

// queueReply is one scripted response from the message queue.
type queueReply struct {
	status  int // 200 with a message, 202 for none
	message map[string]any
}

// fakeGitHub imitates the GitHub REST API and the Actions service closely enough for actions/scaleset. Like GitHub
// Enterprise Server, it serves the REST API under /api/v3 on its own host; the Actions service lives under
// /tenant.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	calls     []string
	scaleSets map[string]map[string]any // by name
	runners   map[string]map[string]any // by name
	queue     []queueReply
	deleted   []int // deleted message IDs
	acquired  [][]int64
	// sessionConflicts is how many session creations fail with a conflict before one succeeds.
	sessionConflicts int
	sessionsOpen     int
	sessionsClosed   int
	// failAccessToken makes GitHub reject the App's JWT.
	failAccessToken bool
	// runnerRelease is the latest actions/runner release; nil means none is published.
	runnerRelease map[string]any
	// installedOn is the type of account the App is installed on: "Organization" (my-org), "User" (octocat, on the
	// repositories hello and world), or "" for not installed.
	installedOn string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, scaleSets: map[string]map[string]any{}, runners: map[string]map[string]any{}}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a Client for the fake organization "my-org" that trusts the fake's certificate and never retries.
func (f *fakeGitHub) client() *Client {
	f.t.Helper()
	c, err := New(Options{
		ConfigURL:      f.srv.URL + "/my-org",
		ClientID:       testClientID,
		InstallationID: testInstallationID,
		PrivateKeyPEM:  testKeyPEM(),
		Version:        "test",
		httpOptions: []scaleset.HTTPOption{
			scaleset.WithRootCAs(f.certPool()),
			scaleset.WithRetryMax(0),
			scaleset.WithTimeout(10 * time.Second),
		},
		httpClient: f.httpClient(),
		apiBaseURL: f.srv.URL,
	})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return c
}

// httpClient returns an HTTP client that trusts the fake, for the REST calls this package makes itself.
func (f *fakeGitHub) httpClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: f.certPool(), MinVersion: tls.VersionTLS12},
	}}
}

func (f *fakeGitHub) certPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return pool
}

func (f *fakeGitHub) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, route)
	auth := r.Header.Get("Authorization")
	body, _ := io.ReadAll(r.Body)

	switch {
	// GitHub REST API.
	case route == fmt.Sprintf("POST /api/v3/app/installations/%d/access_tokens", testInstallationID):
		if f.failAccessToken || !f.validAppJWT(strings.TrimPrefix(auth, "Bearer ")) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "A JSON web token could not be decoded"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": testInstallToken,
			"expires_at": time.Now().Add(time.Hour)})
	case route == "GET /repos/actions/runner/releases/latest":
		if f.runnerRelease == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, f.runnerRelease)
	case route == "POST /app-manifests/"+testManifestCode+"/conversions":
		writeJSON(w, http.StatusCreated, map[string]any{"id": testAppID, "slug": testAppSlug, "client_id": testClientID,
			"pem": testKeyPEM(), "client_secret": testClientSecret, "webhook_secret": testWebhookSecret,
			"owner": map[string]any{"login": "my-org", "type": "Organization"}})
	case strings.HasPrefix(route, "POST /app-manifests/"):
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	case route == "GET /app/installations":
		if !f.validAppJWT(strings.TrimPrefix(auth, "Bearer ")) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "A JSON web token could not be decoded"})
			return
		}
		login := map[string]string{"Organization": "my-org", "User": "octocat"}[f.installedOn]
		installations := []map[string]any{}
		if f.installedOn != "" {
			installations = append(installations, map[string]any{"id": testInstallationID,
				"account": map[string]any{"login": login, "type": f.installedOn}})
		}
		writeJSON(w, http.StatusOK, installations)
	case route == fmt.Sprintf("POST /app/installations/%d/access_tokens", testInstallationID):
		if !f.validAppJWT(strings.TrimPrefix(auth, "Bearer ")) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "A JSON web token could not be decoded"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": testInstallToken})
	case route == "GET /installation/repositories":
		if auth != "Bearer "+testInstallToken {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": 2,
			"repositories": []map[string]any{{"name": "hello"}, {"name": "world"}}})
	case route == "POST /api/v3/orgs/my-org/actions/runners/registration-token":
		if auth != "Bearer "+testInstallToken {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": testRegToken, "expires_at": time.Now().Add(time.Hour)})
	case route == "POST /api/v3/actions/runner-registration":
		if auth != "RemoteAuth "+testRegToken {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"url": f.srv.URL + "/tenant/", "token": adminToken()})

	// Actions service: runner groups, scale sets, runners.
	case route == "GET /tenant/_apis/runtime/runnergroups/":
		name := r.URL.Query().Get("groupName")
		if name != "builds" {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []any{map[string]any{"id": 3, "name": name}}})
	case route == "GET /tenant/_apis/runtime/runnerscalesets":
		s, ok := f.scaleSets[r.URL.Query().Get("name")]
		if !ok || strconv.Itoa(intOf(s["runnerGroupId"])) != r.URL.Query().Get("runnerGroupId") {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []any{s}})
	case route == "POST /tenant/_apis/runtime/runnerscalesets":
		var s map[string]any
		_ = json.Unmarshal(body, &s)
		s["id"] = testScaleSetID
		f.scaleSets[s["name"].(string)] = s
		writeJSON(w, http.StatusOK, s)
	case route == fmt.Sprintf("PATCH /tenant/_apis/runtime/runnerscalesets/%d", testScaleSetID):
		var s map[string]any
		_ = json.Unmarshal(body, &s)
		s["id"] = testScaleSetID
		f.scaleSets[s["name"].(string)] = s
		writeJSON(w, http.StatusOK, s)
	case route == fmt.Sprintf("DELETE /tenant/_apis/runtime/runnerscalesets/%d", testScaleSetID):
		w.WriteHeader(http.StatusNoContent)
	case route == fmt.Sprintf("POST /tenant/_apis/runtime/runnerscalesets/%d/generatejitconfig", testScaleSetID):
		var setting map[string]any
		_ = json.Unmarshal(body, &setting)
		name := setting["name"].(string)
		if setting["workFolder"] != WorkFolder {
			f.t.Errorf("JIT work folder = %v, want %s", setting["workFolder"], WorkFolder)
		}
		if _, ok := f.runners[name]; ok {
			writeJSON(w, http.StatusConflict, map[string]any{"typeName": "GitHub.DistributedTask.WebApi.AgentExistsException",
				"message": "A runner named " + name + " already exists"})
			return
		}
		runner := map[string]any{"id": 100 + len(f.runners), "name": name, "runnerScaleSetId": testScaleSetID}
		f.runners[name] = runner
		writeJSON(w, http.StatusOK, map[string]any{"runner": runner, "encodedJITConfig": testJITConfig})
	case route == "GET /tenant/_apis/distributedtask/pools/0/agents":
		runner, ok := f.runners[r.URL.Query().Get("agentName")]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []any{runner}})
	case strings.HasPrefix(route, "DELETE /tenant/_apis/distributedtask/pools/0/agents/"):
		switch strings.TrimPrefix(route, "DELETE /tenant/_apis/distributedtask/pools/0/agents/") {
		case "404":
			writeJSON(w, http.StatusNotFound, map[string]any{"typeName": "AgentNotFoundException", "message": "gone"})
		case "409":
			writeJSON(w, http.StatusBadRequest, map[string]any{"typeName": "JobStillRunningException", "message": "busy"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}

	// Actions service: message sessions and the queue.
	case route == fmt.Sprintf("POST /tenant/_apis/runtime/runnerscalesets/%d/sessions", testScaleSetID):
		if f.sessionConflicts > 0 {
			f.sessionConflicts--
			writeJSON(w, http.StatusConflict, map[string]any{"typeName": "SessionConflictException",
				"message": "session already exists"})
			return
		}
		f.sessionsOpen++
		writeJSON(w, http.StatusOK, map[string]any{
			"sessionId": testSessionID, "ownerName": "controller", "messageQueueUrl": f.srv.URL + "/queue",
			"messageQueueAccessToken": testQueueToken,
			"statistics":              map[string]any{"totalAssignedJobs": 2, "totalRegisteredRunners": 1},
		})
	case route == fmt.Sprintf("DELETE /tenant/_apis/runtime/runnerscalesets/%d/sessions/%s", testScaleSetID, testSessionID):
		f.sessionsClosed++
		w.WriteHeader(http.StatusNoContent)
	case route == fmt.Sprintf("POST /tenant/_apis/runtime/runnerscalesets/%d/acquirejobs", testScaleSetID):
		var ids []int64
		_ = json.Unmarshal(body, &ids)
		f.acquired = append(f.acquired, ids)
		writeJSON(w, http.StatusOK, map[string]any{"count": len(ids), "value": ids})
	case route == "GET /queue":
		if auth != "Bearer "+testQueueToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if len(f.queue) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		next := f.queue[0]
		f.queue = f.queue[1:]
		if next.status == http.StatusAccepted {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, next.message)
	case strings.HasPrefix(route, "DELETE /queue/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(route, "DELETE /queue/"))
		f.deleted = append(f.deleted, id)
		w.WriteHeader(http.StatusNoContent)

	default:
		f.t.Errorf("unexpected request %s", route)
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
	}
}

// validAppJWT checks that token is an RS256 JWT signed with the test key, issued by the test App, and valid now
// for at most ten minutes, as GitHub requires.
func (f *fakeGitHub) validAppJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	now := time.Now().Unix()
	return json.Unmarshal(payload, &claims) == nil && claims.Iss == testClientID && claims.Iat <= now &&
		claims.Exp > now && claims.Exp <= now+600
}

// adminToken returns an unsigned JWT with an expiry; actions/scaleset only reads its claims.
func adminToken() string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := enc([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return header + "." + payload + ".sig"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// jobMessage returns a queue message carrying the given job messages, the way the Actions service batches them.
func jobMessage(id, assigned int, jobs ...map[string]any) map[string]any {
	body, _ := json.Marshal(jobs)
	return map[string]any{
		"messageId":   id,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(body),
		"statistics":  map[string]any{"totalAssignedJobs": assigned, "totalRunningJobs": 1, "totalBusyRunners": 1},
	}
}
