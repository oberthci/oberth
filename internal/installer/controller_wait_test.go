package installer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// rollingController stands in for the native Deployment controller: an apply
// that changes the controller's Pod template leaves one Pod of the earlier
// template until rollAfter pauses of the readiness wait have passed.
type rollingController struct {
	installer.ChartPort
	client    kubernetes.Interface
	change    bool
	rollAfter int
	pauses    int
}

func (c *rollingController) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := c.ChartPort.Apply(ctx, r); err != nil || !c.change {
		return err
	}
	d, err := c.deployments().Get(ctx, "oberth-controller-workflow-controller", metav1.GetOptions{})
	if err != nil {
		return err
	}
	d.Generation++
	d.Spec.Template.Annotations = map[string]string{"release": r.Chart.Metadata.Version + d.ResourceVersion}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation - 1, Replicas: 1, UpdatedReplicas: 0, AvailableReplicas: 1}
	_, err = c.deployments().Update(ctx, d, metav1.UpdateOptions{})
	return err
}

func (c *rollingController) pause(ctx context.Context) error {
	c.pauses++
	if c.pauses == c.rollAfter {
		d, err := c.deployments().Get(ctx, "oberth-controller-workflow-controller", metav1.GetOptions{})
		if err != nil {
			return err
		}
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
		if _, err = c.deployments().Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	if c.pauses > 20 {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *rollingController) deployments() interface {
	Get(context.Context, string, metav1.GetOptions) (*appsv1.Deployment, error)
	Update(context.Context, *appsv1.Deployment, metav1.UpdateOptions) (*appsv1.Deployment, error)
} {
	return c.client.AppsV1().Deployments("oberth")
}

func TestUpgradeReturnsOnlyOnceTheChangedControllerRolledOut(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct {
		name              string
		change            bool
		rollAfter, pauses int
		refused           bool
	}{
		{"rolls after three pauses", true, 3, 3, false},
		{"keeps an old Pod", true, 0, 21, true},
		{"template unchanged", false, 0, 0, false},
		{"template unchanged, controller unavailable", false, 0, 0, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			f, m, controller := rollingFixture(t, shape.change, shape.rollAfter)
			if strings.HasSuffix(shape.name, "unavailable") {
				unavailable(t, controller)
			}
			plan, err := m.PlanUpgrade(t.Context(), loadFixture(t, releaseFor(t, "v0.1.0", 1)), f.choices, installer.UpgradeOptions{NoBackup: true})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Plan.Close()
			_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
			var refusal *installer.Problem
			if shape.refused != (err != nil) || shape.refused && (!errors.As(err, &refusal) || !strings.Contains(refusal.Detail, "The workflow controller did not finish restarting")) {
				t.Fatal("upgrade returned while the earlier controller served, or waited for nothing", err)
			}
			if controller.pauses != shape.pauses {
				t.Fatal("readiness wait paused", controller.pauses, "times, want", shape.pauses)
			}
		})
	}
}

func TestRollbackReturnsOnlyOnceTheRestoredControllerRolledOut(t *testing.T) {
	t.Parallel()
	f, m, controller := rollingFixture(t, false, 0)
	plan, err := m.PlanUpgrade(t.Context(), loadFixture(t, releaseFor(t, "v0.1.0", 1)), f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	controller.change, controller.rollAfter = true, 2
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if controller.pauses != 2 {
		t.Fatal("rollback returned before the restored controller rolled out", controller.pauses)
	}
}

// unavailable leaves an untouched controller without an available Pod: an apply
// that does not change it must not wait for it.
func unavailable(t *testing.T, c *rollingController) {
	t.Helper()
	d, err := c.deployments().Get(t.Context(), "oberth-controller-workflow-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.AvailableReplicas = 0
	if _, err = c.deployments().Update(t.Context(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func rollingFixture(t *testing.T, change bool, rollAfter int) (fixture, installer.Maintenance, *rollingController) {
	t.Helper()
	f, m := maintenanceFixture(t)
	one := int32(1)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth-controller-workflow-controller", Namespace: "oberth", Generation: 1},
		Spec:   appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"release": "earlier"}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	if _, err := f.client.AppsV1().Deployments("oberth").Create(t.Context(), d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	controller := &rollingController{ChartPort: f.engine.Charts, client: f.client, change: change, rollAfter: rollAfter}
	f.engine.Charts = controller
	f.engine.Pause = controller.pause
	return f, m, controller
}
