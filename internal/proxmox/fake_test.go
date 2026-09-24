package proxmox

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testNode        = "pve1"
	testTokenID     = "par@pve!controller"
	testTokenSecret = "00000000-0000-0000-0000-000000000000"
)

// request is what the fake server saw.
type request struct {
	Method string
	Path   string
	Params url.Values
	Auth   string
	Agent  string
}

// reply is a canned response. For a non-2xx status, Data is dropped, Message becomes the HTTP status text, and
// BodyMessage becomes the body's message field.
type reply struct {
	Status      int
	Data        any
	Message     string
	BodyMessage string
	Errors      map[string]string
}

// fakePVE is a minimal Proxmox VE API over TLS. Handlers are keyed by "METHOD /path" relative to /api2/json.
type fakePVE struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	handlers map[string]func(request) reply
	requests []request
	// tasks maps a UPID to the statuses it reports, one per poll; the last one repeats.
	tasks map[string][]taskStatus
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{t: t, handlers: map[string]func(request) reply{}, tasks: map[string][]taskStatus{}}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	// Tests that reject the server's certificate make it log handshake errors; they are expected.
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

// fingerprint returns the test server certificate's SHA-256 fingerprint in Proxmox's AA:BB:... form.
func (f *fakePVE) fingerprint() string {
	sum := sha256.Sum256(f.server.Certificate().Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// client returns a Client that trusts the fake server by its pinned fingerprint and polls quickly.
func (f *fakePVE) client() *Client {
	f.t.Helper()
	c, err := New(Options{
		URL:             f.server.URL + "/api2/json",
		TokenID:         testTokenID,
		TokenSecret:     testTokenSecret,
		TLSFingerprint:  f.fingerprint(),
		Node:            testNode,
		PollInterval:    time.Millisecond,
		MaxPollInterval: 2 * time.Millisecond,
		UserAgent:       "parcon-test",
	})
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return c
}

func (f *fakePVE) handle(method, path string, h func(request) reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = h
}

// reply registers a fixed response.
func (f *fakePVE) reply(method, path string, r reply) {
	f.handle(method, path, func(request) reply { return r })
}

// task registers a task that reports statuses in order, and returns its UPID.
func (f *fakePVE) task(typ string, statuses ...taskStatus) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	upid := fmt.Sprintf("UPID:%s:0000%04X:00000000:66F00000:%s:100:%s:", testNode, len(f.tasks)+1, typ, testTokenID)
	f.tasks[upid] = statuses
	return upid
}

func running() taskStatus { return taskStatus{Status: "running"} }

func stopped(exit string) taskStatus { return taskStatus{Status: "stopped", ExitStatus: exit} }

// taskReply registers an endpoint that starts a task with the given statuses.
func (f *fakePVE) taskReply(method, path, typ string, statuses ...taskStatus) {
	upid := f.task(typ, statuses...)
	f.reply(method, path, reply{Data: upid})
}

func (f *fakePVE) seen() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.requests...)
}

// last returns the last request to method and path, failing the test if there was none.
func (f *fakePVE) last(method, path string) request {
	f.t.Helper()
	reqs := f.seen()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == method && reqs[i].Path == path {
			return reqs[i]
		}
	}
	f.t.Fatalf("no %s %s request; saw %v", method, path, reqs)
	return request{}
}

func (f *fakePVE) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api2/json")
	unescaped, err := url.PathUnescape(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := request{
		Method: r.Method,
		Path:   unescaped,
		Params: r.Form,
		Auth:   r.Header.Get("Authorization"),
		Agent:  r.Header.Get("User-Agent"),
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	h, ok := f.handlers[r.Method+" "+unescaped]
	if !ok {
		h, ok = f.taskStatusHandler(r.Method, unescaped)
	}
	f.mu.Unlock()

	if !ok {
		f.write(w, reply{Status: http.StatusNotImplemented, Message: "no fake for " + r.Method + " " + unescaped})
		return
	}
	f.write(w, h(req))
}

// taskStatusHandler serves GET /nodes/<node>/tasks/<upid>/status. The caller holds f.mu.
func (f *fakePVE) taskStatusHandler(method, path string) (func(request) reply, bool) {
	prefix := "/nodes/" + testNode + "/tasks/"
	if method != http.MethodGet || !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/status") {
		return nil, false
	}
	upid := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/status")
	statuses, ok := f.tasks[upid]
	if !ok {
		return func(request) reply { return reply{Status: http.StatusInternalServerError, Message: "no such task"} }, true
	}
	st := statuses[0]
	if len(statuses) > 1 {
		f.tasks[upid] = statuses[1:]
	}
	return func(request) reply { return reply{Data: st} }, true
}

func (f *fakePVE) write(w http.ResponseWriter, r reply) {
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	body := map[string]any{"data": r.Data}
	if status >= 300 {
		body = map[string]any{"data": nil}
		if r.Errors != nil {
			body["errors"] = r.Errors
		}
		if r.BodyMessage != "" {
			body["message"] = r.BodyMessage
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		f.t.Errorf("encode fake response: %v", err)
		return
	}
	if status < 300 || r.Message == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
		return
	}

	// Proxmox puts the error message in the HTTP status line, which net/http can't customize, so write the
	// response by hand.
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.t.Errorf("hijack: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n"+
		"Connection: close\r\n\r\n%s", status, r.Message, len(data), data)
	_ = buf.Flush()
}
