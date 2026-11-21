package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// versionedController stands in for an apply whose rendered controller Pod
// template follows the chart version, as a release that changes it does.
type versionedController struct {
	installer.ChartPort
	client kubernetes.Interface
}

func (c versionedController) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := c.ChartPort.Apply(ctx, r); err != nil {
		return err
	}
	deployments := c.client.AppsV1().Deployments("oberth")
	d, err := deployments.Get(ctx, "oberth-controller-workflow-controller", metav1.GetOptions{})
	if err != nil {
		return err
	}
	d.Spec.Template.Annotations = map[string]string{"release": r.Chart.Metadata.Version}
	// The native Deployment controller finishes the rollout at once here.
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	_, err = deployments.Update(ctx, d, metav1.UpdateOptions{})
	return err
}

func TestChangeThatRollsTheControllerSaysSoOnce(t *testing.T) {
	f := signedMaintenanceFixture(t)
	client := f.session.engine.Cluster.Client
	controller := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth-controller-workflow-controller", Namespace: "oberth"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"release": "0.1.0"}}}}}
	if _, err := client.AppsV1().Deployments("oberth").Create(t.Context(), controller, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.session.engine.Charts = versionedController{ChartPort: f.charts, client: client}
	liveMetadataServer(t, &f)
	upgrade := func(version string) []string {
		return []string{"upgrade", "--version", version, "--release-dir", bothReleases(t, f), "--no-backup", "--yes"}
	}
	for _, step := range []struct {
		args    []string
		restart bool
	}{
		{upgrade("v0.2.0"), true},
		{[]string{"rollback", "--release-dir", f.previous, "--yes"}, true},
		{upgrade("v0.1.0"), false},
	} {
		app, out, diagnostic := f.rollbackApplication(t)
		if code := app.run(t.Context(), step.args); code != 0 {
			t.Fatal(step.args[0], "failed", code, diagnostic)
		}
		if got := strings.Count(out.String(), "The workflow controller was restarted.\n"); got != map[bool]int{true: 1, false: 0}[step.restart] {
			t.Fatal(step.args[0], "controller restart reported", got, "times", out)
		}
	}
}

// liveMetadataServer answers the upgrade's live metadata and audit calls.
func liveMetadataServer(t *testing.T, f *signedMaintenance) {
	t.Helper()
	bindings := fake.Bindings()
	bindings.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{SchemaVersion: store.Schema, Protocol: stateport.DefaultProtocol()}}
	bindings.AdminAuditRecord.Handler = &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: "recorded"}}
	server, _, _ := fixture(t, bindings)
	f.session.app.config = server.config
	f.session.app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Authorization", "Bearer valid")
		return server.config.Transport.RoundTrip(r)
	})
}
