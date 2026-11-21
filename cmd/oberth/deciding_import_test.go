package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestDecidingIssueImportReadLimit(t *testing.T) {
	for _, size := range []int{apicontract.RequestBytes, apicontract.RequestBytes + 1} {
		name := "at-limit"
		if size > apicontract.RequestBytes {
			name = "over-limit"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(decidingDirectory(t), "issues.jsonl")
			if err := os.WriteFile(path, []byte(strings.Repeat(" ", size)), 0600); err != nil {
				t.Fatal(err)
			}
			app, _, diagnostic := fixture(t, fake.Bindings())
			calls := 0
			app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected import request")
			})
			if code := app.run(t.Context(), []string{"issue", "import", path}); code != 1 || calls != 0 {
				t.Fatal("bounded import reached transport", code, calls, diagnostic.String())
			}
			want := "The encoded request exceeds 1048576 bytes.\nUse a smaller request and retry.\n"
			if size > apicontract.RequestBytes {
				want = "The issue import file exceeds 1048576 bytes.\nUse several files with disjoint numbers, each within the file and encoded\n  request limits.\n"
			}
			want += "No issues imported.\nline 1\nFile:\n" + path + "\n"
			if diagnostic.String() != want {
				t.Fatalf("limit refusal=%q want=%q", diagnostic.String(), want)
			}
		})
	}
}
