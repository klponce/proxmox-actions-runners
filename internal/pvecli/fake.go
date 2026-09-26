package pvecli

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// FakeExec is an Exec for tests. Each call is recorded, and its result comes from the most recently added handler
// whose prefix matches the command line, so a test can override a general handler with a narrower one. A command
// with no handler fails.
type FakeExec struct {
	mu       sync.Mutex
	handlers []fakeHandler
	Calls    []Cmd
}

type fakeHandler struct {
	prefix string
	fn     func(Cmd) ([]byte, error)
}

// On handles commands whose command line starts with prefix.
func (f *FakeExec) On(prefix string, fn func(Cmd) ([]byte, error)) *FakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, fakeHandler{prefix, fn})
	return f
}

// Reply handles commands whose command line starts with prefix with a fixed output.
func (f *FakeExec) Reply(prefix, stdout string) *FakeExec {
	return f.On(prefix, func(Cmd) ([]byte, error) { return []byte(stdout), nil })
}

// Fail makes commands whose command line starts with prefix fail.
func (f *FakeExec) Fail(prefix string) *FakeExec {
	return f.On(prefix, func(c Cmd) ([]byte, error) {
		return nil, &ExitError{Cmd: c.String(), Err: fmt.Errorf("exit status 2")}
	})
}

// Run implements Exec.
func (f *FakeExec) Run(_ context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	handlers := f.handlers
	f.mu.Unlock()
	line := c.String()
	for i := len(handlers) - 1; i >= 0; i-- {
		if strings.HasPrefix(line, handlers[i].prefix) {
			return handlers[i].fn(c)
		}
	}
	return nil, &ExitError{Cmd: line, Err: fmt.Errorf("no fake for this command")}
}

// Lines returns the command lines run so far.
func (f *FakeExec) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		lines[i] = c.String()
	}
	return lines
}
