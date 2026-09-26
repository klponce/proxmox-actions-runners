package term

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAsk(t *testing.T) {
	var out strings.Builder
	got, err := ask(strings.NewReader("Iv23liEXAMPLE\r\nrest\n"), &out, "Client ID: ")
	if err != nil || got != "Iv23liEXAMPLE" || out.String() != "Client ID: " {
		t.Errorf("ask = %q, %v; prompt %q", got, err, out.String())
	}
	// A last line without a newline still counts.
	if got, err := ask(strings.NewReader("yes"), &out, "? "); err != nil || got != "yes" {
		t.Errorf("ask without newline = %q, %v", got, err)
	}
	if _, err := ask(strings.NewReader(""), &out, "? "); err == nil {
		t.Error("ask at EOF: no error")
	}
}

func TestIsYes(t *testing.T) {
	tests := []struct {
		answer string
		want   bool
	}{{"y", true}, {"YES", true}, {" yes ", true}, {"", false}, {"n", false}, {"yep", false}}
	for _, tt := range tests {
		if got := isYes(tt.answer); got != tt.want {
			t.Errorf("isYes(%q) = %v", tt.answer, got)
		}
	}
}

func TestReadPEM(t *testing.T) {
	var out strings.Builder
	in := "\n-----BEGIN RSA PRIVATE KEY-----\r\nAAAA\n-----END RSA PRIVATE KEY-----\nextra\n"
	got, err := readPEM(strings.NewReader(in), &out, "Paste the key:")
	if want := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"; err != nil || got != want {
		t.Errorf("readPEM = %q, %v", got, err)
	}
	if _, err := readPEM(strings.NewReader("-----BEGIN X-----\nAAAA\n"), &out, ""); err == nil {
		t.Error("PEM without END: no error")
	}
}

func TestChooseRetriesUntilValid(t *testing.T) {
	var out strings.Builder
	got, err := choose(strings.NewReader("x\n3\n2\n"), &out, "Which repository?", []string{"a", "b"})
	if err != nil || got != 1 {
		t.Errorf("choose = %d, %v", got, err)
	}
	if strings.Count(out.String(), "Enter a number from 1 to 2.") != 2 {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestTTYWithoutTerminal(t *testing.T) {
	tty := TTY{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := tty.Confirm("Go?"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("Confirm = %v, want ErrNoTerminal", err)
	}
	if _, err := tty.PEM("Key:"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("PEM = %v, want ErrNoTerminal", err)
	}
}

func TestScripted(t *testing.T) {
	s := &Scripted{Answers: []string{"y", "2"}}
	if ok, err := s.Confirm("Install?"); !ok || err != nil {
		t.Errorf("Confirm = %v, %v", ok, err)
	}
	if i, err := s.Choose("Which?", []string{"a", "b"}); i != 1 || err != nil {
		t.Errorf("Choose = %d, %v", i, err)
	}
	if _, err := s.Line("More?"); err == nil {
		t.Error("an unexpected question: no error")
	}
	if _, err := (&Scripted{NoTTY: true}).Confirm("Install?"); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("NoTTY Confirm = %v", err)
	}
}
