package proxmox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// pveBool decodes the booleans Proxmox returns, which may be JSON booleans, the numbers 0 and 1, or those numbers as
// strings.
type pveBool bool

func (b *pveBool) UnmarshalJSON(data []byte) error {
	switch string(bytes.Trim(data, `"`)) {
	case "true", "1":
		*b = true
	case "false", "0", "", "null":
		*b = false
	default:
		return fmt.Errorf("invalid boolean %s", data)
	}
	return nil
}

// pveInt decodes integers that Proxmox may return as JSON numbers or as strings.
type pveInt int64

func (n *pveInt) UnmarshalJSON(data []byte) error {
	s := string(bytes.Trim(data, `"`))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Some fields are floats in practice, such as sizes computed by storage plugins.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return fmt.Errorf("invalid integer %s", data)
		}
		v = int64(f)
	}
	*n = pveInt(v)
	return nil
}

var (
	_ json.Unmarshaler = (*pveBool)(nil)
	_ json.Unmarshaler = (*pveInt)(nil)
)
