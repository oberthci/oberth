package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestDeltaPublicationRequiresAttribution(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"prepare", "finish"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "actor-check")
			intent := publicationIntent(r)
			intent.Publication.Actor = ""
			if mode == "prepare" {
				requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
			}
			before := databaseContents(t, s.readers)
			events := []stateport.EventWrite{atomicEvent(s, "unattributed-publication")}
			var err error
			if mode == "finish" {
				err = s.Finish(t.Context(), stateport.Action{Events: events}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Publication: &intent})
			} else {
				err = s.PreparePublication(t.Context(), stateport.Action{Events: events}, intent)
			}
			requireCode(t, err, publicerr.InvalidInput)
			if databaseContents(t, s.readers) != before {
				t.Fatal("unattributed intent changed run, publication, audit or events")
			}
			intent.Publication.Actor = "operator"
			action := stateport.Action{Actor: "operator", Events: events}
			if mode == "finish" {
				requireOK(t, s.Finish(t.Context(), action, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Publication: &intent}))
			} else {
				requireOK(t, s.PreparePublication(t.Context(), action, intent))
			}
		})
	}
}
