package term

import (
	"errors"
	"fmt"
	"strings"
)

// Scripted is a Terminal for tests: it answers questions from Answers in order and records every prompt. With
// NoTTY, it behaves like a process without a terminal.
type Scripted struct {
	Answers []string
	NoTTY   bool
	Prompts []string
}

var _ Terminal = (*Scripted)(nil)

func (s *Scripted) next(prompt string) (string, error) {
	s.Prompts = append(s.Prompts, prompt)
	if s.NoTTY {
		return "", ErrNoTerminal
	}
	if len(s.Answers) == 0 {
		return "", fmt.Errorf("unexpected question %q", prompt)
	}
	a := s.Answers[0]
	s.Answers = s.Answers[1:]
	return a, nil
}

// Confirm implements Terminal.
func (s *Scripted) Confirm(question string) (bool, error) {
	a, err := s.next(question)
	return isYes(a), err
}

// Line implements Terminal.
func (s *Scripted) Line(prompt string) (string, error) { return s.next(prompt) }

// PEM implements Terminal.
func (s *Scripted) PEM(prompt string) (string, error) {
	a, err := s.next(prompt)
	if err == nil && !strings.Contains(a, "-----END ") {
		return "", errors.New("read PEM: no -----END line")
	}
	return a, err
}

// Choose implements Terminal. The answer is the option's 1-based number, as the user would type it.
func (s *Scripted) Choose(prompt string, options []string) (int, error) {
	a, err := s.next(prompt)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(a, "%d", &n); err != nil || n < 1 || n > len(options) {
		return 0, fmt.Errorf("scripted answer %q is not an option", a)
	}
	return n - 1, nil
}
