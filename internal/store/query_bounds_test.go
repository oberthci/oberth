package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestCatalogAndRecordPageBounds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	reads := []struct {
		name string
		read func(int) error
	}{
		{"upstreams", func(n int) error { _, err := s.Upstreams(t.Context(), Page{Limit: n}); return err }},
		{"keys", func(n int) error { _, err := s.Keys(t.Context(), KeyFilter{Page: Page{Limit: n}}); return err }},
		{"repositories", func(n int) error {
			_, err := s.Repositories(t.Context(), stateport.CatalogFilter{Limit: n})
			return err
		}},
		{"secret grants", func(n int) error {
			_, err := s.SecretGrants(t.Context(), stateport.CatalogFilter{Limit: n})
			return err
		}},
		{"capabilities", func(n int) error {
			_, err := s.Capabilities(t.Context(), stateport.CatalogFilter{Limit: n})
			return err
		}},
		{"publications", func(n int) error { _, err := s.Publications(t.Context(), stateport.RecordFilter{Limit: n}); return err }},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			requireOK(t, read.read(200))
			requireCode(t, read.read(201), publicerr.InvalidInput)
			requireCode(t, read.read(0), publicerr.InvalidInput)
		})
	}
	_, err := s.Events(t.Context(), stateport.EventFilter{Through: -1, Limit: 200})
	requireCode(t, err, publicerr.InvalidInput)
}
