package argojob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMalformedHelperConfigurationNeverStartsOriginal(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"ATTEMPT", "wrong"}, {"ATTEMPT", "-1"},
		{"LIMIT", "wrong"}, {"LIMIT", "-1"},
		{"TTL", "wrong"}, {"TTL", "-1"}, {"TTL", "0"}, {"TTL", "14401"},
		{"PROTOCOL", "2"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			r, _ := testHoldRuntime(t)
			original := r.env
			r.env = func(key string) string {
				if key == "OBERTH_HOLD_"+test.key {
					return test.value
				}
				return original(key)
			}
			marker := filepath.Join(r.root, "effect")
			code := r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "printf changed > \"$1\"", "sh", marker}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
			if code != 126 {
				t.Fatal("invalid configuration admitted", test, code)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("invalid configuration started original", test, err)
			}
			if _, err := os.Stat(filepath.Join(r.root, "entry")); !os.IsNotExist(err) {
				t.Fatal("invalid configuration claimed entry", test, err)
			}
		})
	}
}
func TestSocketShapesCannotCreateHold(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := testHoldRuntime(t)
			path := filepath.Join(r.root, "hold.sock")
			var err error
			if kind == "directory" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.Symlink("missing", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if code := r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}); code != 7 {
				t.Fatal("bind failure changed original code", code)
			}
		})
	}
}
