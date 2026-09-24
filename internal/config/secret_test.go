package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretFile(t *testing.T) {
	const secret = "00000000-0000-0000-0000-000000000000"
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{"trims whitespace", write("ok", "  "+secret+"\n", 0o600), secret, ""},
		{"read-only owner", write("ro", secret, 0o400), secret, ""},
		{"group readable", write("group", secret, 0o640), "", "must not be accessible to group or others"},
		{"world readable", write("world", secret, 0o604), "", "mode 0604"},
		{"empty", write("empty", " \n", 0o600), "", "is empty"},
		{"missing", filepath.Join(dir, "missing"), "", "no such file"},
		{"directory", dir, "", "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadSecretFile(tt.path)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("ReadSecretFile = %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ReadSecretFile error = %v, want it to mention %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the secret: %v", err)
			}
		})
	}
}
