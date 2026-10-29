package perm

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Permissions travel as decimal strings in JSON: a uint64 does not fit in a
// JavaScript number once bits above 2^53 are used.
func (p Permissions) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(p), 10))
}

func (p *Permissions) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("permissions must be a decimal string: %w", err)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("permissions must be a decimal string: %w", err)
	}
	*p = Permissions(v)
	return nil
}
