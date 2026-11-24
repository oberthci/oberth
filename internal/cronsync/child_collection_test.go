package cronsync_test

import (
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestNativeScheduledChildCarriesRecordFinalizer(t *testing.T) {
	f := newFixture(t)
	child := f.child()
	if !slices.Contains(child.Finalizers, nativebinding.RecordFinalizer) {
		t.Fatal("native schedule child can be deleted before its run record is durable", child.Finalizers)
	}
}
