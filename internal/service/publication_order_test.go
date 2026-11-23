package service_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestPublicationListBindingKeepsChronologicalCursor(t *testing.T) {
	f := setup(t)
	for i, id := range []string{"z-old", "a-middle", "b-new"} {
		f.offset.Store(int64(time.Duration(i) * time.Second))
		p := model.Publication{ID: id, Repo: repository, Ref: "refs/heads/topic", Actor: caller().Identity, ObjectSHA: head}
		if err := f.store.PreparePublication(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationIntent{Publication: p}); err != nil {
			t.Fatal(err)
		}
	}
	q := apicontract.PublicationListArgs{Repo: repository, Limit: 2}
	for _, want := range [][]string{{"b-new", "a-middle"}, {"z-old"}} {
		call, err := f.s.Bindings().PublicationList.Handler.Prepare(t.Context(), apicontract.Caller{Actor: caller()}, q)
		if err != nil {
			t.Fatal(err)
		}
		page, err := call(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, p := range page.Publications {
			ids = append(ids, p.ID)
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("publication page = %v, want %v", ids, want)
		}
		if len(want) == 2 && page.NextCursor != "a-middle" || len(want) == 1 && page.NextCursor != "" {
			t.Fatal("cursor does not follow chronology", page)
		}
		q.Before = page.NextCursor
	}
}
