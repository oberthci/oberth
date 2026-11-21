package argojob

import (
	"github.com/oberthci/oberth/pkg/argoworkflow/defaults"
	"strconv"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func armHold(t *wf.Template, w *wf.Workflow, p nativebinding.Preparation, entry admissioncontract.PlanEntry) error {
	if !p.Assist || !entry.HoldEligible {
		return nil
	}
	effective, err := defaults.Template(w.Spec.TemplateDefaults, t)
	if err != nil {
		return err
	}
	*t = *effective
	if t.RetryStrategy == nil && w.Spec.RetryStrategy != nil {
		t.RetryStrategy = w.Spec.RetryStrategy.DeepCopy()
	}
	return wrapHold(t, p)
}
func wrapHold(t *wf.Template, p nativebinding.Preparation) error {
	main := holdMain(t)
	if main == nil {
		return nil
	}
	if p.HoldTTL == 0 {
		p.HoldTTL = 20 * time.Minute
	}
	if p.HoldTTL < time.Second || p.HoldTTL > 4*time.Hour || p.HelperImage == "" {
		return limited()
	}
	strategy := t.RetryStrategy
	limit := "0"
	attempt := "0"
	if strategy != nil {
		limit = strategy.Limit.String()
		attempt = "{{retries}}"
	}
	main.Command = append([]string{holdBinary, "run", "--"}, main.Command...)
	values := []core.EnvVar{
		{Name: "OBERTH_HOLD_ATTEMPT", Value: attempt},
		{Name: "OBERTH_HOLD_LIMIT", Value: limit},
		{Name: "OBERTH_HOLD_TTL", Value: strconv.FormatInt(int64(p.HoldTTL/time.Second), 10)},
		{Name: "OBERTH_HOLD_POD_UID", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "OBERTH_HOLD_PROTOCOL", Value: "1"},
	}
	for _, v := range values {
		main.Env = canonicalHoldEnv(main.Env, v)
	}
	main.VolumeMounts = append(main.VolumeMounts, core.VolumeMount{Name: "oberth-hold-bin", MountPath: "/oberth-hold/bin", ReadOnly: true}, core.VolumeMount{Name: "oberth-hold-state", MountPath: holdState})
	main.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{holdBinary, "status", "--probe"}}}, PeriodSeconds: 5, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 1}
	t.Volumes = append(t.Volumes, core.Volume{Name: "oberth-hold-bin", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}, core.Volume{Name: "oberth-hold-state", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{Medium: core.StorageMediumMemory, SizeLimit: new(resource.MustParse("1Mi"))}}})
	t.InitContainers = append(t.InitContainers, wf.UserContainer{Container: core.Container{Name: "oberth-hold-install", Image: p.HelperImage, Command: []string{"oberth", "internal-hold", "install", "--to", "/oberth-hold/bin"}, VolumeMounts: []core.VolumeMount{{Name: "oberth-hold-bin", MountPath: "/oberth-hold/bin"}}}})
	return nil
}
func canonicalHoldEnv(env []core.EnvVar, value core.EnvVar) []core.EnvVar {
	out := env[:0]
	for _, v := range env {
		if v.Name != value.Name {
			out = append(out, v)
		}
	}
	return append(out, value)
}
func holdMain(t *wf.Template) *core.Container {
	if t.Container != nil {
		return t.Container
	}
	if t.Script != nil {
		return &t.Script.Container
	}
	return nil
}
