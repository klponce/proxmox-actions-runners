package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses a memory size in binary units, such as 8GiB, 8 GiB, or 12288MiB, into MiB. The unit is required
// and case-insensitive. Decimal units (GB, MB) and bare numbers are refused rather than guessed, because Proxmox
// sizes memory in MiB and 8GB is not 8GiB. Errors say what is wrong without repeating s, which callers quote.
func ParseSize(s string) (int, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return r < '0' || r > '9' })
	if i <= 0 {
		return 0, errors.New("needs a number and a unit, such as 8GiB or 12288MiB")
	}
	n, err := strconv.Atoi(t[:i])
	if err != nil {
		return 0, errors.New("is too large")
	}
	unit := strings.ToLower(strings.TrimSpace(t[i:]))
	var perUnit int
	switch unit {
	case "gib":
		perUnit = 1024
	case "mib":
		perUnit = 1
	case "gb", "g", "mb", "m", "tb", "t", "tib":
		return 0, errors.New("use GiB or MiB, such as 8GiB or 12288MiB")
	default:
		return 0, fmt.Errorf("has an unknown unit %q: use GiB or MiB", strings.TrimSpace(t[i:]))
	}
	if n > (1<<31-1)/perUnit {
		return 0, errors.New("is too large")
	}
	return n * perUnit, nil
}

// FormatMiB prints a size in MiB the way ParseSize reads it: in GiB when it is a whole number of GiB, otherwise in
// MiB.
func FormatMiB(mib int) string {
	if mib != 0 && mib%1024 == 0 {
		return strconv.Itoa(mib/1024) + "GiB"
	}
	return strconv.Itoa(mib) + "MiB"
}

var days = regexp.MustCompile(`^(\d+)d`)

// ParseLifetime parses a duration in whole minutes, such as 12h, 90m, 1h30m, or 2d (d is 24h). A unit is required.
// Errors say what is wrong without repeating s, which callers quote.
func ParseLifetime(s string) (time.Duration, error) {
	t := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "")
	if t == "" {
		return 0, errors.New("needs a number and a unit, such as 12h, 90m, 1h30m, or 2d")
	}
	var d time.Duration
	if m := days.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 1000 {
			return 0, errors.New("is too long")
		}
		d = time.Duration(n) * 24 * time.Hour
		t = t[len(m[0]):]
	}
	if t != "" {
		rest, err := time.ParseDuration(t)
		if err != nil || rest < 0 {
			return 0, errors.New("needs a number and a unit, such as 12h, 90m, 1h30m, or 2d")
		}
		d += rest
	}
	if d%time.Minute != 0 {
		return 0, errors.New("must be whole minutes")
	}
	return d, nil
}

// FormatLifetime prints a duration the way ParseLifetime reads it: in whole days when it is some, such as 2d,
// otherwise in hours and minutes, such as 12h, 1h30m, or 45m.
func FormatLifetime(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}
