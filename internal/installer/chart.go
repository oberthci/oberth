package installer

import (
	"context"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// ChartLibrary adapts an explicitly configured library action client.
// Its REST client getter must be composed from Engine.Cluster.Config.
type ChartLibrary struct {
	target  *kubeconfig.Target
	config  *action.Configuration
	Timeout time.Duration
}

func (l ChartLibrary) Read(ctx context.Context, ns string) (History, error) {
	if err := ctx.Err(); err != nil {
		return History{}, err
	}
	config, err := l.configuration(ctx, ns)
	if err != nil {
		return History{}, err
	}
	return readHistory(ctx, config, ns)
}

func (l ChartLibrary) Preview(ctx context.Context, r ChartRequest) error { return l.run(ctx, r, true) }
func (l ChartLibrary) Apply(ctx context.Context, r ChartRequest) error   { return l.run(ctx, r, false) }
func (l ChartLibrary) run(ctx context.Context, r ChartRequest, dry bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dry && r.offlinePreview() {
		_, err := Render(r.Chart, r.Values, r.Namespace)
		return err
	}

	config, err := l.configuration(ctx, r.Namespace)
	if err != nil {
		return err
	}
	r.Chart, r.Values, err = preparedChart(r.Chart, r.Values, r.Namespace)
	if err != nil {
		return err
	}
	l.Timeout = l.timeout()
	if err = l.preflight(ctx, config, r); err != nil {
		return err
	}
	if r.RollbackID != "" {
		if err = l.rollbackRevision(config, r, dry); err != nil {
			return err
		}
	}
	if dry {
		if r.Revision == 0 {
			return l.install(ctx, config, r, true)
		}
		return nil
	}
	return l.applyPrepared(ctx, config, r)
}

func (l ChartLibrary) applyPrepared(ctx context.Context, config *action.Configuration, r ChartRequest) error {
	r, err := l.repairRequest(ctx, config, r)
	if err != nil {
		return err
	}
	if r.Revision == 0 {
		return l.install(ctx, config, r, false)
	}
	return l.upgrade(ctx, config, r)
}
func (l ChartLibrary) upgrade(ctx context.Context, config *action.Configuration, r ChartRequest) error {
	checked := *config
	releases := *config.Releases
	releases.Driver = revisionWriteGuard{Driver: config.Releases.Driver, ctx: ctx, config: config, request: r}
	checked.Releases = &releases
	checked.KubeClient = deletionGuard{Interface: config.KubeClient, ctx: ctx, request: r}
	a := l.upgradeAction(&checked, r, false)
	_, err := a.RunWithContext(ctx, "oberth", r.Chart, r.Values)
	return err
}
func (l ChartLibrary) upgradeAction(config *action.Configuration, r ChartRequest, dry bool) *action.Upgrade {
	a := action.NewUpgrade(config)
	a.Labels = revisionLabels(r)
	a.Labels["oberth.ci/rollback"] = "null"
	if r.RollbackID != "" {
		a.Labels["oberth.ci/rollback"] = r.RollbackID
	}
	a.Namespace, a.Timeout, a.Wait = r.Namespace, l.Timeout, false
	a.DryRun, a.DryRunOption, a.ResetValues = dry, "none", true
	if dry {
		a.DryRunOption = "server"
	}
	return a
}

func (l ChartLibrary) install(ctx context.Context, config *action.Configuration, r ChartRequest, dry bool) error {
	a := action.NewInstall(config)
	a.Labels = revisionLabels(r)
	a.ReleaseName = "oberth"
	a.Namespace = r.Namespace
	a.Timeout = l.Timeout
	a.Wait = false
	a.DryRun = dry
	a.DryRunOption = "none"
	if dry {
		a.DryRunOption = "server"
	}
	a.CreateNamespace = false
	_, err := a.RunWithContext(ctx, r.Chart, r.Values)
	return err
}

// Render validates the schema and renders all dependencies offline.
func Render(ch *chart.Chart, values map[string]any, namespace string) (map[string]string, error) {
	ch, values, err := preparedChart(ch, values, namespace)
	if err != nil {
		return nil, err
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	rendered, err := chartutil.ToRenderValuesWithSchemaValidation(ch, values, chartutil.ReleaseOptions{Name: "oberth", Namespace: namespace, Revision: 1, IsInstall: true}, caps, false)
	if err != nil {
		return nil, err
	}
	return engine.Render(ch, rendered)
}

func (l ChartLibrary) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return 5 * time.Minute
}

func (r ChartRequest) offlinePreview() bool { return r.Local && r.Revision == 0 && r.Recovery == nil }
