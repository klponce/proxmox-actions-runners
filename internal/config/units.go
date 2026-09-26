package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses a memory size in binary units, such as 8GiB, 8 GiB, or 12288MiB, into MiB. The unit is required
// and case-insensitive. Decimal units (GB, MB) and bare numbers are refused rather than guessed, because Proxmox
// sizes memory in MiB and 8GB is not 8GiB.
func ParseSize(s string) (int, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return r < '0' || r > '9' })
	if i <= 0 {
		return 0, fmt.Errorf("%q needs a number and a unit, such as 8GiB or 12288MiB", s)
	}
	n, err := strconv.Atoi(t[:i])
	if err != nil {
		return 0, fmt.Errorf("%q is too large", s)
	}
	unit := strings.ToLower(strings.TrimSpace(t[i:]))
	var perUnit int
	switch unit {
	case "gib":
		perUnit = 1024
	case "mib":
		perUnit = 1
	case "gb", "g", "mb", "m", "tb", "t", "tib":
		return 0, fmt.Errorf("%q: use GiB or MiB, such as 8GiB or 12288MiB", s)
	default:
		return 0, fmt.Errorf("%q has an unknown unit %q: use GiB or MiB", s, strings.TrimSpace(t[i:]))
	}
	if n > (1<<31-1)/perUnit {
		return 0, fmt.Errorf("%q is too large", s)
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
