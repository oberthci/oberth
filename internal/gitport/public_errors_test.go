package gitport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

type publicRefusalCase struct {
	name           string
	err            error
	code           publicerr.Code
	reason, remedy string
}

func TestGitRefusalsExposePublicClassification(t *testing.T) {
	cases := []publicRefusalCase{
		{"absent", &AbsentError{Repository: "origin/team/widget", Ref: "refs/heads/main", Path: "private-path"}, publicerr.NotFound, "Git data was not found.", "Refresh the repository and select an existing exact revision and file."},
		{"author", &AuthorRefusalError{Code: "invalid_ref", Message: "private-detail", Path: "private-path", Hint: "Choose an existing public branch."}, publicerr.InvalidInput, "The Git ref is invalid.", "Choose an existing public branch."},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			assertPublicGitRefusal(t, row)
			for _, mixed := range []error{errors.Join(row.err, context.Canceled), errors.Join(errors.New("store unavailable"), row.err), errors.Join(row.err, publicerr.New(publicerr.Unavailable, "store is unavailable", "Restore store access.", nil))} {
				if publicerr.Permanent(mixed) {
					t.Fatal("Git refusal hid an outage")
				}
			}
		})
	}
}

func assertPublicGitRefusal(t *testing.T, row publicRefusalCase) {
	t.Helper()
	for _, err := range []error{row.err, fmt.Errorf("wrapped: %w", row.err), errors.Join(row.err, row.err)} {
		public := publicerr.Classify(err)
		if public.Code() != row.code || public.Reason() != row.reason || public.Remedy() != row.remedy || !publicerr.Permanent(err) {
			t.Fatal(public)
		}
		if !errors.Is(public, row.err) {
			t.Fatal("original typed error was lost")
		}
		raw, failure := json.Marshal(public)
		if failure != nil || strings.Contains(string(raw), "private-") {
			t.Fatal("private Git evidence entered the public error", failure)
		}
	}
}
