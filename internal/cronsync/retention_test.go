package cronsync_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestScheduleAdmissionCommitsBeforeNativePublication(t *testing.T) {
	f := newFixture(t)
	outage := errors.New("store unavailable")
	config := f.config
	config.RetainAdmission = func(context.Context, *admissioncontract.PreparedAdmission) error { return outage }
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SyncAll(t.Context()); !errors.Is(err, outage) {
		t.Fatal("retention outage ignored", err)
	}
	for _, a := range f.native.Actions() {
		if a.Verb == "create" || a.Verb == "update" {
			t.Fatal("parent published before admission commit", a)
		}
	}
	config.RetainAdmission = nil
	if _, err := cronsync.New(config); err == nil {
		t.Fatal("missing retention dependency accepted")
	}
}

func TestSchedulePublicationRequiresSuccessAndSurfacesStoreOutage(t *testing.T) {
	f := newFixture(t)
	config := f.config
	outage := errors.New("publication unavailable")
	publications := 0
	config.PublishAdmission = func(ctx context.Context, id int64, entry string, keep []string) error {
		publications++
		name, err := nativebinding.CronName(id, entry)
		if err != nil {
			return err
		}
		parent, err := f.native.Get(ctx, name, meta.GetOptions{})
		if err != nil {
			return err
		}
		if parent.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation] != keep[0] {
			t.Error("publication preceded native apply")
		}
		return outage
	}
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SyncAll(t.Context()); !errors.Is(err, outage) || publications != 1 {
		t.Fatal("publication outage not surfaced", err, publications)
	}
	config.PublishAdmission = nil
	if _, err := cronsync.New(config); err == nil {
		t.Fatal("missing publication callback accepted")
	}
}

func TestAdmissionPruningWaitsForParentDeletion(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: old, cron: '0 4 * * *'}]")
	f.sync()
	old := f.object(f.repo, "old")
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	outage := errors.New("native deletion unavailable")
	f.hook = func(_ context.Context, verb, _ string) error {
		if verb == "delete" {
			return outage
		}
		return nil
	}
	pruned := 0
	config := f.config
	config.PruneAdmissions = func(ctx context.Context, id int64, keep []string) error {
		pruned++
		if id != f.repo.ID || len(keep) != 1 || keep[0] != "nightly" {
			t.Error("wrong retained schedule admissions", id, keep)
		}
		if _, err := f.native.Get(ctx, old.Name, meta.GetOptions{}); err == nil {
			t.Error("admission pruning preceded parent deletion")
		}
		return nil
	}
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SyncAll(t.Context()); !errors.Is(err, outage) || pruned != 0 {
		t.Fatal("failed native removal pruned admissions", err, pruned)
	}
	if f.object(f.repo, "old").UID != old.UID {
		t.Fatal("failed deletion changed the parent")
	}
	f.hook = nil
	if err = c.SyncAll(t.Context()); err != nil || pruned != 1 {
		t.Fatal("successful repair did not prune admissions", err, pruned)
	}
}
