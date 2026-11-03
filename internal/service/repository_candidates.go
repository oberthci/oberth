package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// Callers supply only repositories visible to the actor, including when no
// repository argument was supplied and a SHA or branch matched several.
func (service *API) ambiguousRepositories(ctx context.Context, selector string, repos []model.Repository) error {
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		name, err := service.repositoryIdentity(ctx, repo.ID)
		if err != nil {
			return err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("%w: %q matches %s; set repo to upstream/org/repo", ErrAmbiguousRepository, selector, strings.Join(names, ", "))
}
