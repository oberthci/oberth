package contracttest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type publicationListPort interface {
	PreparePublication(context.Context, stateport.Action, stateport.PublicationIntent) error
	Publications(context.Context, stateport.RecordFilter) ([]model.Publication, error)
}

// PublicationOrder checks chronological order, exact ties and exclusive pages.
func PublicationOrder(t *testing.T, p publicationListPort, repo string, clock func(time.Time)) {
	t.Helper()
	base := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"z-old", "a-middle", "b-new", "c-new"} {
		clock(base.Add(time.Duration(min(i, 2))))
		pub := model.Publication{ID: id, Repo: repo, Ref: "refs/heads/topic", Actor: "operator", ObjectSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		mustRun(t, p.PreparePublication(t.Context(), stateport.Action{Actor: pub.Actor}, stateport.PublicationIntent{Publication: pub}))
	}
	for _, tc := range []struct {
		before, after, status string
		limit                 int
		want                  []string
	}{
		{limit: 10, want: []string{"c-new", "b-new", "a-middle", "z-old"}},
		{limit: 2, want: []string{"c-new", "b-new"}},
		{before: "b-new", limit: 2, want: []string{"a-middle", "z-old"}},
		{before: "c-new", status: "pending", limit: 2, want: []string{"b-new", "a-middle"}},
		{after: "a-middle", limit: 2, want: []string{"b-new", "c-new"}},
		{before: "missing", limit: 2, want: []string{}},
	} {
		q := stateport.RecordFilter{Repositories: []string{repo}, Before: tc.before, After: tc.after, Status: tc.status, Limit: tc.limit}
		rows, err := p.Publications(t.Context(), q)
		mustRun(t, err)
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if !reflect.DeepEqual(ids, tc.want) {
			t.Fatalf("page %+v = %v, want %v", q, ids, tc.want)
		}
	}
}
