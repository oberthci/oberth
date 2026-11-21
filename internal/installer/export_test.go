package installer

import "context"

import "helm.sh/helm/v3/pkg/action"
import "helm.sh/helm/v3/pkg/chart"
import "helm.sh/helm/v3/pkg/storage/driver"
import typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

// ChartForTest injects native action storage without exposing a production override.
func ChartForTest(config *action.Configuration) ChartLibrary { return ChartLibrary{config: config} }

func ReleaseSecretsForTest(secrets typedcorev1.SecretInterface) driver.Driver {
	return newReleaseSecrets(secrets)
}

func (e *Engine) StorePodForTest(ctx context.Context, namespace string) (string, error) {
	return e.storePod(ctx, namespace)
}

func ReleaseChartForTest(r Release) *chart.Chart { return r.chart }

func RevisionLabelsForTest(r ChartRequest) map[string]string { return revisionLabels(r) }

func RevisionBindingForTest(number int, kind, version string) string {
	return revisionBinding(number, kind, version)
}
