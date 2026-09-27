package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr string
	}{
		{in: "8GiB", want: 8192},
		{in: "8 GiB", want: 8192},
		{in: " 16gib ", want: 16384},
		{in: "12288MiB", want: 12288},
		{in: "12288mib", want: 12288},
		{in: "8GB", wantErr: "use GiB or MiB"},
		{in: "8G", wantErr: "use GiB or MiB"},
		{in: "8192MB", wantErr: "use GiB or MiB"},
		{in: "8192", wantErr: "needs a number and a unit"},
		{in: "GiB", wantErr: "needs a number and a unit"},
		{in: "", wantErr: "needs a number and a unit"},
		{in: "-8GiB", wantErr: "needs a number and a unit"},
		{in: "8.5GiB", wantErr: "unknown unit"},
		{in: "8 bytes", wantErr: "unknown unit"},
		{in: "99999999999GiB", wantErr: "too large"},
		{in: "3000000GiB", wantErr: "too large"},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseSize(%q) = %d, %v; want an error with %q", tt.in, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
}

func TestFormatMiB(t *testing.T) {
	for mib, want := range map[int]string{8192: "8GiB", 1024: "1GiB", 8704: "8704MiB", 1000: "1000MiB", 0: "0MiB"} {
		if got := FormatMiB(mib); got != want {
			t.Errorf("FormatMiB(%d) = %q, want %q", mib, got, want)
		}
		if mib > 0 {
			if back, err := ParseSize(FormatMiB(mib)); err != nil || back != mib {
				t.Errorf("ParseSize(FormatMiB(%d)) = %d, %v", mib, back, err)
			}
		}
	}
}

func TestParseLifetime(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr string
	}{
		{in: "12h", want: 12 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "2d", want: 48 * time.Hour},
		{in: "1d12h", want: 36 * time.Hour},
		{in: " 12H ", want: 12 * time.Hour},
		{in: "12", wantErr: "needs a number and a unit"},
		{in: "", wantErr: "needs a number and a unit"},
		{in: "twelve hours", wantErr: "needs a number and a unit"},
		{in: "-1h", wantErr: "needs a number and a unit"},
		{in: "1h30s", wantErr: "whole minutes"},
		{in: "9999d", wantErr: "too long"},
	}
	for _, tt := range tests {
		got, err := ParseLifetime(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseLifetime(%q) = %s, %v; want an error with %q", tt.in, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseLifetime(%q) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
}

func TestFormatLifetime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		6 * time.Hour: "6h", 12 * time.Hour: "12h", 90 * time.Minute: "1h30m", 45 * time.Minute: "45m",
		48 * time.Hour: "2d", 36 * time.Hour: "36h", 120 * time.Hour: "5d",
	} {
		if got := FormatLifetime(d); got != want {
			t.Errorf("FormatLifetime(%s) = %q, want %q", d, got, want)
		}
		if back, err := ParseLifetime(want); err != nil || back != d {
			t.Errorf("ParseLifetime(%q) = %s, %v", want, back, err)
		}
	}
}
