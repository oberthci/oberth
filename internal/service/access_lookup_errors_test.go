package service

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type brokenAccessLookup struct {
	RepositoryReader
	SecretAccessStore
	lookupErr, qualifiedErr error
}

func (s brokenAccessLookup) RepositoryByName(context.Context, string) (model.Repository, error) {
	return model.Repository{ID: 1}, s.lookupErr
}
func (s brokenAccessLookup) QualifiedRepoName(context.Context, int64) (string, error) {
	return "", s.qualifiedErr
}
func TestCanonicalAccessRepoPropagatesDatabaseErrors(t *testing.T) {
	for _, stage := range []string{"lookup", "qualification"} {
		for _, failure := range []error{context.DeadlineExceeded, deadlineBusyError(5)} {
			source := brokenAccessLookup{}
			if stage == "lookup" {
				source.lookupErr = failure
			} else {
				source.qualifiedErr = failure
			}
			s := &API{repositories: source, secretAccess: source}
			if key, err := s.canonicalAccessRepo(t.Context(), "oberth"); !errors.Is(err, failure) || key != "" {
				t.Errorf("%s: key=%q err=%v", stage, key, err)
			}
		}
	}
}
