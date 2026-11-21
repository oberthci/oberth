package sourcecontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Digest binds exact bytes with lowercase SHA-256.
func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// CanonicalJSON sorts object keys, preserves array order and numeric tokens,
// parses raw snapshots and emits UTF-8 without insignificant whitespace.
// The standard decode/encode round trip also sorts fields of Go structs.
// Callers apply their schema/size bounds before canonical encoding.
func CanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}

	var result bytes.Buffer
	encoder := json.NewEncoder(&result)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, fmt.Errorf("canonical JSON: %w", err)
	}
	return bytes.TrimSuffix(result.Bytes(), []byte{'\n'}), nil
}
