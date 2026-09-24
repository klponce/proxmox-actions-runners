package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ReadSecretFile reads a secret, such as the Proxmox token secret, from a file the config names. The file must not
// be accessible to group or others, like an SSH private key. Surrounding whitespace, such as a trailing newline, is
// removed. Errors name the file but never include its contents.
func ReadSecretFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("secret file %s has mode %04o; it must not be accessible to group or others (use 0600)",
			path, perm)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path comes from the operator's config.
	if err != nil {
		return "", fmt.Errorf("secret file: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", errors.New("secret file " + path + " is empty")
	}
	return secret, nil
}
