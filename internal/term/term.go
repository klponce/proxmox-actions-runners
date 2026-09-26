// Package term is how parcon's host commands talk to the person running them: questions on the terminal, and the
// step-by-step output of install, update, and uninstall.
//
// Questions go to /dev/tty, not stdin, so they work when stdin is a pipe, as it is for `bash install.sh` run from
// `curl`. Without a terminal, every question fails with ErrNoTerminal, and callers say which flag answers it instead.
package term

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrNoTerminal means there is no terminal to ask on.
var ErrNoTerminal = errors.New("no terminal to ask on")

// Terminal asks the user questions.
type Terminal interface {
	// Confirm asks a yes/no question. Anything but y or yes is no.
	Confirm(question string) (bool, error)
	// Line asks for one line of text, without its newline.
	Line(prompt string) (string, error)
	// PEM asks for a PEM block, read up to and including its -----END line.
	PEM(prompt string) (string, error)
	// Choose asks the user to pick one of options by number, and returns its index.
	Choose(prompt string, options []string) (int, error)
}

// TTY asks on the controlling terminal.
type TTY struct {
	// Path is the terminal device. Empty means /dev/tty.
	Path string
}

func (t TTY) open() (*os.File, error) {
	path := t.Path
	if path == "" {
		path = "/dev/tty"
	}
	// Opening is the only reliable test: /dev/tty exists and is readable even in a process without a terminal.
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: the terminal device.
	if err != nil {
		return nil, ErrNoTerminal
	}
	return f, nil
}

// Confirm implements Terminal.
func (t TTY) Confirm(question string) (bool, error) {
	answer, err := t.Line(question + " [y/N] ")
	if err != nil {
		return false, err
	}
	return isYes(answer), nil
}

// Line implements Terminal.
func (t TTY) Line(prompt string) (string, error) {
	f, err := t.open()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return ask(f, f, prompt)
}

// PEM implements Terminal.
func (t TTY) PEM(prompt string) (string, error) {
	f, err := t.open()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readPEM(f, f, prompt)
}

// Choose implements Terminal.
func (t TTY) Choose(prompt string, options []string) (int, error) {
	f, err := t.open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return choose(f, f, prompt, options)
}

func isYes(answer string) bool {
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes"
}

func ask(in io.Reader, out io.Writer, prompt string) (string, error) {
	if _, err := fmt.Fprint(out, prompt); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", fmt.Errorf("read answer: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readPEM(in io.Reader, out io.Writer, prompt string) (string, error) {
	if _, err := fmt.Fprintln(out, prompt); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	var b strings.Builder
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if b.Len() == 0 && strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.HasPrefix(line, "-----END ") {
			return b.String(), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read PEM: %w", err)
	}
	return "", errors.New("read PEM: no -----END line")
}

func choose(in io.Reader, out io.Writer, prompt string, options []string) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("nothing to choose from")
	}
	r := bufio.NewReader(in)
	for {
		if _, err := fmt.Fprintln(out, prompt); err != nil {
			return 0, fmt.Errorf("write prompt: %w", err)
		}
		for i, o := range options {
			if _, err := fmt.Fprintf(out, "  %d) %s\n", i+1, o); err != nil {
				return 0, fmt.Errorf("write prompt: %w", err)
			}
		}
		answer, err := ask(r, out, "Number: ")
		if err != nil {
			return 0, err
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(answer), "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		if _, err := fmt.Fprintf(out, "Enter a number from 1 to %d.\n", len(options)); err != nil {
			return 0, fmt.Errorf("write prompt: %w", err)
		}
	}
}
