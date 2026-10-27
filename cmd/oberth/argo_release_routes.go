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

func readArgoReleaseTagRoutes(name string) (argojob.ReleaseTagRoutes, error) {
	if name == "" {
		return nil, nil
	}
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("serve: --argo-release-tag-routes-config must be a clean absolute path")
	}
	f, err := os.Open(name) // #nosec G304 G703 -- clean absolute administrator flag, never selected by repository input.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, errors.New("serve: release tag routes exceed 1 MiB")
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("serve: release tag routes must be a JSON object")
	}
	if err := checkReleaseRouteJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return nil, fmt.Errorf("serve: invalid release tag routes: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var routes argojob.ReleaseTagRoutes
	if err := d.Decode(&routes); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("serve: release tag routes require one JSON object")
	}
	if err := routes.Validate(); err != nil {
		return nil, err
	}
	return routes, nil
}

// Reject duplicate keys and case-folded fields before encoding/json decodes
// the typed schema. Counts, canonical repos, patterns and paths validate later.
func checkReleaseRouteJSON(d *json.Decoder, depth int) error {
	if depth > 3 {
		return errors.New("release route document is too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if _, ok := token.(string); ok {
		return nil
	}
	if token == json.Delim('[') {
		if depth != 1 {
			return errors.New("release route arrays belong under repository keys")
		}
		for d.More() {
			if err := checkReleaseRouteJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if token != json.Delim('{') {
		return errors.New("release route values must be objects, arrays or strings")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("duplicate release route property")
		}
		seen[name] = true
		if depth == 2 && name != "pattern" && name != "file" {
			return errors.New("unknown release route property")
		}
		if err := checkReleaseRouteJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
