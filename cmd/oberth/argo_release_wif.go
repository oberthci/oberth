package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/argojob"
)

// Read once while building the engine. Runtime Builds retain this snapshot;
// repository files and mid-run config changes cannot widen a capability.
func readArgoReleaseWIFConfig(name string) (*argojob.ReleaseWIFConfig, error) {
	if name == "" {
		return nil, nil
	}
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("serve: --argo-release-wif-config must be a clean absolute path")
	}
	// #nosec G304 -- trusted administrator flag, constrained to a clean absolute path above; repository inputs cannot select this file.
	f, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("serve: open release WIF capability file: %w", err)
	}
	defer func() { _ = f.Close() }()
	const maxBytes = 1 << 20
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("serve: read release WIF capability file: %w", err)
	}
	if len(b) > maxBytes {
		return nil, errors.New("serve: release WIF capability file exceeds 1 MiB")
	}
	if trimmed := bytes.TrimSpace(b); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("serve: release WIF capability file must be a JSON object")
	}
	if err := checkReleaseWIFJSON(json.NewDecoder(bytes.NewReader(b)), 0); err != nil {
		return nil, fmt.Errorf("serve: invalid release WIF capability JSON: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var config argojob.ReleaseWIFConfig
	if err := d.Decode(&config); err != nil {
		return nil, fmt.Errorf("serve: decode release WIF capability file: %w", err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("serve: release WIF capability file must contain one JSON object")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

// Encoding/json otherwise accepts duplicate properties and case-folded struct
// fields. Neither is an unambiguous administrator authorization document.
// This closed schema has only objects and strings, at four object depths.
func checkReleaseWIFJSON(d *json.Decoder, depth int) error {
	if depth > 4 {
		return errors.New("capability document is too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if _, ok := token.(string); ok {
		return nil
	}
	if token != json.Delim('{') {
		return errors.New("capability values must be objects or strings")
	}
	seen := map[string]bool{}
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return errors.New("duplicate capability property")
		}
		seen[key] = true
		if depth == 0 && key != "namespace" && key != "roles" && key != "repositories" {
			return errors.New("unknown capability property")
		}
		if depth == 2 && key != "provider" && key != "service_account" && key != "service_account_name" && key != "templates" {
			return errors.New("unknown capability binding property")
		}
		if err := checkReleaseWIFJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
