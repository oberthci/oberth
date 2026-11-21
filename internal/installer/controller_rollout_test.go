package installer_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const controllerInstanceAnnotation = "workflows.argoproj.io/controller-instanceid"

// Reconcile the actual chart Pod template through the fake Kubernetes API.
// A template change is the native Deployment controller's rollout boundary.
type controllerRolloutChart struct {
	*fake.Chart
	client   kubernetes.Interface
	rollouts int
}

func (c *controllerRolloutChart) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := c.Chart.Apply(ctx, r); err != nil {
		return err
	}
	next, err := requestController(r)
	if err != nil {
		return err
	}
	deployments := c.client.AppsV1().Deployments(r.Namespace)
	old, err := deployments.Get(ctx, next.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(old.Spec.Template, next.Spec.Template) {
		c.rollouts++
	}
	next.ResourceVersion = old.ResourceVersion
	// The native Deployment controller finishes the rollout at once here.
	next.Status = appsv1.DeploymentStatus{ObservedGeneration: next.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	_, err = deployments.Update(ctx, next, metav1.UpdateOptions{})
	return err
}
func requestController(r installer.ChartRequest) (*appsv1.Deployment, error) {
	files, err := installer.Render(r.Chart, r.Values, r.Namespace)
	if err != nil {
		return nil, err
	}
	for name, raw := range files {
		if !strings.Contains(name, "argoNative") || !strings.Contains(raw, "kind: Deployment") {
			continue
		}
		var d appsv1.Deployment
		if err = yaml.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		return &d, nil
	}
	return nil, fmt.Errorf("controller Deployment missing")
}
func TestUpgradeIntroducesControllerInstanceWithOneRollout(t *testing.T) {
	f, m := maintenanceFixture(t)
	request := f.charts.Applies[0]
	legacy, err := requestController(request)
	if err != nil {
		t.Fatal(err)
	}
	delete(legacy.Spec.Template.Annotations, controllerInstanceAnnotation)
	if _, err = f.client.AppsV1().Deployments(request.Namespace).Create(t.Context(), legacy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-controller-config", Namespace: request.Namespace}, Data: map[string]string{"parallelism": "4"}}
	if _, err = f.client.CoreV1().ConfigMaps(request.Namespace).Create(t.Context(), config, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	chart := &controllerRolloutChart{Chart: f.charts, client: f.client}
	f.engine.Charts = chart
	for range 2 {
		p, e := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(p.Plan.Close)
		if !strings.Contains(strings.Join(p.Plan.Review(), "\n"), "unlabelled runs not adopted; finish before upgrade.") {
			t.Fatal("upgrade promises to adopt unlabelled runs")
		}
		if _, e = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); e != nil {
			t.Fatal(e)
		}
	}
	after, err := f.client.AppsV1().Deployments(request.Namespace).Get(t.Context(), legacy.Name, metav1.GetOptions{})
	if err != nil || after.Spec.Template.Annotations[controllerInstanceAnnotation] != request.Namespace || chart.rollouts != 1 {
		t.Fatal("instance ID transition did not roll out exactly once", chart.rollouts, after, err)
	}
}
