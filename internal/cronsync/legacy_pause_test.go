package cronsync_test

import (
	"reflect"
	"testing"

	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPauseLabelsExistingUnlabelledParent(t *testing.T) {
	f := newFixture(t)
	f.sync()
	before := f.object(f.repo, "nightly")
	delete(before.Labels, "workflows.argoproj.io/controller-instanceid")
	if _, err := f.native.Update(t.Context(), before, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.paused = true
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil {
		t.Fatal(err)
	}
	after := f.object(f.repo, "nightly")
	if err := instanceid.NewService(f.config.InstanceID).Validate(after); err != nil {
		t.Fatal("paused legacy parent escaped controller", err)
	}
	if instanceid.NewService("").Validate(after) == nil {
		t.Fatal("unlabelled controller still selects parent")
	}
	before.Spec.Suspend = true
	before.Labels["workflows.argoproj.io/controller-instanceid"] = f.config.InstanceID
	after.ResourceVersion = before.ResourceVersion
	if !reflect.DeepEqual(before, after) {
		t.Fatal("pause changed more than intent and controller identity", before, after)
	}
}
