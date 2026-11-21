package argoworkflow

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestTemplateDefaultSecretsRefuseWildcardGrant(t *testing.T) {
	t.Parallel()
	selector := &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "private"}, Key: "k"}
	for _, archive := range []bool{false, true} {
		w := simpleWorkflow()
		w.Spec.TemplateDefaults = &wf.Template{}
		if archive {
			w.Spec.TemplateDefaults.ArchiveLocation = &wf.ArtifactLocation{S3: &wf.S3Artifact{S3Bucket: wf.S3Bucket{Endpoint: "objects.example.test", Bucket: "data", AccessKeySecret: selector}, Key: "output"}}
		} else {
			w.Spec.TemplateDefaults.Container = &core.Container{Env: []core.EnvVar{{Name: "PRIVATE", ValueFrom: &core.EnvVarSource{SecretKeyRef: selector}}}}
		}
		r := requestFor(t, w)
		r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", "cluster-secret", "private")}
		if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err == nil {
			t.Fatal("templateDefaults secret accepted with wildcard grant", archive)
		}
	}
}
