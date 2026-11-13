package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ValidateAdminObject rejects duplicate keys as well as malformed JSON. It is
// shared by HTTP and MCP so neither transport weakens the administrative seam.
func ValidateAdminObject(raw json.RawMessage) error {
	if len(raw) > AdminRequestBytes || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return errors.New("administration requires a bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := readAdminJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("administration requires one JSON object")
	}
	return nil
}

func readAdminJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || keys[key] {
				return errors.New("duplicate or invalid JSON field")
			}
			keys[key] = true
			if err := readAdminJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := readAdminJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
