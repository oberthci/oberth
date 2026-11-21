package client

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/apicontract"
)

// ReserveCredentials reserves destinations before requesting any result that can contain a credential.
func ReserveCredentials(base string, d apicontract.Definition, resolved Options) (map[string]string, error) {
	paths := map[string]string{}
	complete := false
	defer func() {
		if !complete {
			CleanupCredentials(paths)
		}
	}()
	for _, f := range apicontract.Arguments(d.ResultType) {
		if !f.Sensitive {
			continue
		}
		if base == "" {
			base = filepath.Dir(resolved.TokenFile)
		}
		dir := filepath.Join(base, "credentials")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fileFailure(dir, "write", err)
		}
		path := filepath.Join(dir, rand.Text())
		if err := ReservePrivate(path); err != nil {
			return nil, err
		}
		paths[f.Name] = path
	}
	complete = true
	return paths, nil
}

// SaveCredentials saves credentials before either human or JSON output can observe the result.
func SaveCredentials(data json.RawMessage, paths map[string]string) (json.RawMessage, error) {
	defer CleanupCredentials(paths)
	if len(paths) == 0 {
		return data, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	changed := false
	for field, path := range paths {
		var secret string
		if err := json.Unmarshal(object[field], &secret); len(object[field]) > 0 && err != nil {
			return nil, err
		}
		if secret == "" {
			continue
		}
		if err := WritePrivate(path, []byte(secret)); err != nil {
			return nil, err
		}
		delete(object, field)
		object[field+"_file"], _ = json.Marshal(path)
		changed = true
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(object)
}

// CleanupCredentials removes only unused reservations, preserving saved secrets.
func CleanupCredentials(paths map[string]string) {
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() && info.Size() == 0 {
			_ = os.Remove(path)
		}
	}
}
