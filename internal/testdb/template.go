// Package testdb supports isolated database fixtures.
package testdb

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type Template struct {
	once sync.Once
	body []byte
	err  error
}

// Copy initializes one template and copies it to each fixture's private path.
// initialize must checkpoint and close the database before returning.
func (template *Template) Copy(t *testing.T, path string, initialize func(string) error) {
	t.Helper()
	template.once.Do(func() {
		template.body, template.err = snapshot(initialize)
	})
	if template.err != nil {
		t.Fatalf("initialize database template: %v", template.err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- caller-owned test fixture path.
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(template.body)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatalf("copy database template: %v", err)
	}
}

func snapshot(initialize func(string) error) (body []byte, resultErr error) {
	dir, err := os.MkdirTemp("", "oberth-test-template-")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "oberth.sqlite")
	if err := initialize(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path) // #nosec G304 -- private test template path.
}
