package secretstore_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestGrantAndStoreShareSecretPathSyntax(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path  string
		valid bool
	}{
		{"a/key", true}, {"A_1/key-name.txt", true}, {".hidden/key", true}, {strings.Repeat("a", 1024), true},
		{"", false}, {".", false}, {"..", false}, {"a..b/key", false}, {"a/../b", false}, {"a//b", false}, {"a/./b", false}, {"a/", false}, {"/a", false}, {"a b", false}, {"a\\b", false}, {"a?b", false}, {"a#b", false}, {"é/key", false}, {strings.Repeat("a", 1025), false},
	} {
		t.Run(tc.path[:min(len(tc.path), 40)], func(t *testing.T) { secretPathCase(t, tc.path, tc.valid) })
	}
}

func secretPathCase(t *testing.T, path string, valid bool) {
	t.Helper()

	key := workflowpolicy.GrantKey{Path: path}
	key.Repository, key.File, key.Step = "origin/team/repo", ".oberth/build.yaml", "build"
	if (key.Validate() == nil) != valid {
		t.Fatal("grant differs", path)
	}
	checkStoredPath(t, path, valid)
	checkFetchedPath(t, path, valid)
}

func checkStoredPath(t *testing.T, path string, valid bool) {
	t.Helper()
	writer, backend := boundedWriter(t)
	if err := writer.Put(t.Context(), "step-one", path, []byte("value")); (err == nil) != valid {
		t.Fatal("write differs", err)
	}
	if !valid && len(backend.Calls) != 0 {
		t.Fatal("refused write contacted store")
	}
	backend.Calls = nil
	if err := writer.Delete(t.Context(), "step-one", path); (err == nil) != valid {
		t.Fatal("delete differs", err)
	}
	if !valid && len(backend.Calls) != 0 {
		t.Fatal("refused delete contacted store")
	}
}

func checkFetchedPath(t *testing.T, path string, valid bool) {
	t.Helper()
	writer, backend := boundedWriter(t)
	backend.Resources = map[string]map[string]any{"/oberth/steps/step-one/" + path: {"value": "dmFsdWU="}}
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	values, err := writer.Client.Fetch(t.Context(), config(), []byte("header."+claim+".signature"), []string{path})
	if (err == nil) != valid {
		t.Fatal("fetch differs", err)
	}
	if valid && string(values[path]) != "value" {
		t.Fatal("stored value differs")
	}
	if !valid && (len(backend.Calls) != 0 || values != nil) {
		t.Fatal("refused fetch contacted store")
	}
}

func TestFetchValidatesAllPathsBeforeLogin(t *testing.T) {
	for _, paths := range [][]string{{"valid", "a..b/key"}, {"valid", "valid"}} {
		writer, backend := boundedWriter(t)
		claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
		values, err := writer.Client.Fetch(t.Context(), config(), []byte("header."+claim+".signature"), paths)
		if err == nil || values != nil || len(backend.Calls) != 0 {
			t.Fatal("refused set contacted store", err)
		}
	}
}
