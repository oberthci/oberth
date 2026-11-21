package fake

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func (f *Runs) PublishedRelease(ctx context.Context, repo, ref string) (model.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "PublishedRelease", false); err != nil {
		return model.Publication{}, err
	}
	if p, ok := f.data.publishedRelease(repo, ref); ok {
		return p.Clone(), nil
	}
	return model.Publication{}, runProblem(publicerr.NotFound)
}

func (m *runMemory) publishedRelease(repo, ref string) (model.Publication, bool) {
	for _, p := range m.publications {
		if p.Status == model.PublicationDelivered && p.Release != nil && p.Repo == repo && p.Ref == ref {
			return p, true
		}
	}
	return model.Publication{}, false
}
