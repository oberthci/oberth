package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/internal/api"
)

// Normalize only declared run ID aliases. Other arguments still pass through
// the existing strict decoder. Multiple spellings must name the same value.
func normalizeRunIDArgument(name string, raw json.RawMessage) (json.RawMessage, error) {
	key := api.RunIDArgument(name)
	if key == "" {
		return raw, nil
	}
	if len(raw) > maximumToolBytes {
		return nil, fmt.Errorf("%w: tool arguments exceed 1 MiB", ErrInvalidInput)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, nil
	}
	alias, hasAlias := fields["run_id"]
	if !hasAlias {
		return raw, nil
	}
	var id string
	if err := json.Unmarshal(alias, &id); err != nil || id == "" {
		return nil, fmt.Errorf("%w: run_id must be a nonempty string", ErrInvalidInput)
	}
	if original, ok := fields[key]; ok {
		var existing string
		if err := json.Unmarshal(original, &existing); err != nil || existing != id {
			return nil, fmt.Errorf("%w: conflicting %s and run_id", ErrInvalidInput, key)
		}
	} else {
		fields[key] = alias
	}
	delete(fields, "run_id")
	// Marshal preserves all other raw values, including exact integers.
	output, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(output), nil
}
