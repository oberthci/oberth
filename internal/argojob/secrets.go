package argojob

import (
	"encoding/json"
	"slices"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	core "k8s.io/api/core/v1"
)

func (e *Executor) appendSecrets(t *wf.Template, grants []admissioncontract.GrantEvidence) error {
	if len(grants) == 0 {
		return nil
	}
	cfg := e.config.SecretStore
	if !e.storeConfigured {
		reason, remedy := runtimeconfig.SecretStoreConfiguration()
		return publicerr.New(publicerr.Unavailable, reason, remedy, nil)
	}
	paths := make([]string, 0, len(grants))
	for _, g := range grants {
		paths = append(paths, g.Path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	config, _ := json.Marshal(cfg)
	selected, _ := json.Marshal(paths)
	env := []core.EnvVar{
		{Name: runtimeconfig.SecretConfigEnv, Value: string(config)},
		{Name: runtimeconfig.SecretPathsEnv, Value: string(selected)},
		{Name: runtimeconfig.SecretTokenEnv, Value: runtimeconfig.SecretTokenDirectory + "/token"},
		{Name: runtimeconfig.SecretScratchEnv, Value: runtimeconfig.SecretScratchDirectory},
	}
	mounts := []core.VolumeMount{
		{Name: "oberth-store-token", MountPath: runtimeconfig.SecretTokenDirectory, ReadOnly: true},
		{Name: "oberth-secret-scratch", MountPath: runtimeconfig.SecretScratchDirectory},
	}
	t.Volumes = append(t.Volumes,
		core.Volume{Name: "oberth-store-token", VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{Sources: []core.VolumeProjection{{ServiceAccountToken: &core.ServiceAccountTokenProjection{Audience: cfg.Access.Audience, Path: "token", ExpirationSeconds: new(int64(600))}}}}}},
		core.Volume{Name: "oberth-secret-scratch", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{Medium: core.StorageMediumMemory}}},
	)
	for _, c := range secretContainers(t) {
		c.Env = replaceSecretEnvironment(c.Env, env)
		c.VolumeMounts = append(c.VolumeMounts, mounts...)
	}
	return nil
}

func secretContainers(t *wf.Template) []*core.Container {
	var out []*core.Container
	if t.Container != nil {
		out = append(out, t.Container)
	}
	if t.Script != nil {
		out = append(out, &t.Script.Container)
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			out = append(out, &t.ContainerSet.Containers[i].Container)
		}
	}
	return out
}
func replaceSecretEnvironment(env, values []core.EnvVar) []core.EnvVar {
	env = slices.DeleteFunc(env, func(v core.EnvVar) bool {
		return slices.ContainsFunc(values, func(w core.EnvVar) bool { return v.Name == w.Name })
	})
	return append(env, values...)
}
