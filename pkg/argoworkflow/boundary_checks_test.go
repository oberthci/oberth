package argoworkflow

import (
	"context"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

type checkDocument struct {
	wf       *wfv1.Workflow
	request  admissioncontract.PrepareRequest
	policy   Policy
	hooks    admissioncontract.PrepareHooks
	bindings []sourcecontract.Binding
}
type boundaryCheck struct {
	name, code, path string
	base, change     func(*checkDocument)
}

func clusterGrant(step, kind, value string) admissioncontract.GrantEvidence {
	return admissioncontract.GrantEvidence{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: step, Kind: kind, Path: value, Revision: "1", State: admissioncontract.StateVerified}
}
func (d *checkDocument) sync(t *testing.T) {
	t.Helper()
	d.request.Document = mustDocument(t, d.wf)
	d.request.SourceReceipt.DocumentDigest = sourcecontract.Digest(d.request.Document)
	d.hooks.BindSource = bindingHook(d.bindings...)
}
func (d *checkDocument) template() *wfv1.Template { return &d.wf.Spec.Templates[0] }
func (d *checkDocument) volume(source corev1.VolumeSource) {
	d.template().Volumes = []corev1.Volume{{Name: "v", VolumeSource: source}}
}
func resourceDocument(d *checkDocument) {
	wf, p := resourceWorkflow(`apiVersion: example.test/v1
kind: Widget
metadata: {name: test, namespace: resources}`)
	d.wf, d.policy = wf, p.Policy
	d.request.IdentityRequest.ResourceAccount = "resources"
}
func tokenDocument(d *checkDocument) {
	wf, f := tokenWorkflow()
	d.wf, d.policy = wf, f.Policy
	d.hooks.LoginAudience = f.LoginAudience
	d.request.Grants = f.Grants
	d.bindings = []sourcecontract.Binding{{Template: "main", Artifact: "source", Path: "/work/src", Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, RepositoryID: 1, Revision: d.request.SourceReceipt.Revision}}
}
func sharedDocument(d *checkDocument) {
	d.wf = sharedConsumer("shared", "work")
	d.policy.TemplateGetter = getterFor(map[string]*wfv1.WorkflowTemplate{"shared": sharedObject("shared")})
}
func TestBoundaryChecksIndividually(t *testing.T) {
	t.Parallel()
	for _, tc := range append(append(boundaryChecks(), deltaBoundaryChecks()...), followupBoundaryChecks()...) {
		t.Run(tc.name, func(t *testing.T) {
			d := checkDocument{wf: simpleWorkflow(), request: requestFor(t, simpleWorkflow()), hooks: identityHooks()}
			if tc.base != nil {
				tc.base(&d)
			}
			d.sync(t)
			if _, err := Prepare(t.Context(), d.request, d.policy, d.hooks); err != nil {
				t.Fatal("base is not admitted", err)
			}
			tc.change(&d)
			d.sync(t)
			prepared, err := Prepare(t.Context(), d.request, d.policy, d.hooks)
			if prepared != nil {
				t.Fatal("forbidden change sealed")
			}
			for _, f := range admissionFindings(t, err) {
				if f.Code == tc.code && f.Path == tc.path {
					return
				}
			}
			t.Fatalf("missing %s at %s: %v", tc.code, tc.path, err)
		})
	}
}
func boundaryChecks() []boundaryCheck {
	const template = `spec.templates["main"]`
	return append(append(declarationChecks(), sharedAndHookChecks()...), append(identityChecks(), []boundaryCheck{
		{"r-hostprocess-ctr", "host", template + ".container.securityContext.windowsOptions.hostProcess", nil, func(d *checkDocument) {
			d.template().Container.SecurityContext = &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(true)}}
		}},
		{"r-hostprocess-pod", "host", "spec.securityContext.windowsOptions.hostProcess", nil, func(d *checkDocument) {
			d.wf.Spec.SecurityContext = &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: ptr.To(true)}}
		}},
		{"r-secret-volume", "read", template + ".volumes[v].secret", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "private"}})
		}},
		{"r-csi-volume", "volume", template + ".volumes[v].csi", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "storage.example.test"}})
		}},
		{"r-driver-secretref", "volume", template + ".volumes[v].rbd", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{RBD: &corev1.RBDVolumeSource{SecretRef: &corev1.LocalObjectReference{Name: "private"}}})
		}},
		{"r-envfrom-secret", "read", template + ".container.envFrom[0].secretRef", nil, func(d *checkDocument) {
			d.template().Container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}}}}
		}},
		{"r-pvc-grant", "pvc", template + ".volumes[v]", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "private"}})
		}},
		{"r-vct-volumename", "read", "spec.volumeClaimTemplates[own].spec", nil, func(d *checkDocument) {
			d.wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "own"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "server-data"}}}
		}},
		{"r-vct-datasourceref", "read", "spec.volumeClaimTemplates[own].spec", nil, func(d *checkDocument) {
			d.wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "own"}, Spec: corev1.PersistentVolumeClaimSpec{DataSourceRef: &corev1.TypedObjectReference{Kind: "PersistentVolumeClaim", Name: "private"}}}}
		}},
		{"r-vct-selector", "read", "spec.volumeClaimTemplates[own].spec", nil, func(d *checkDocument) {
			d.wf.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "own"}, Spec: corev1.PersistentVolumeClaimSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "server"}}}}}
		}},
		{"r-ephemeral", "volume", template + ".volumes[v].ephemeral", nil, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{}}})
		}},
		{"r-path-relative", "path", template + `.container.volumeMounts[v].mountPath["relative"]`, nil, func(d *checkDocument) {
			d.template().Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "relative"}}
		}},
		{"r-device-path", "path", template + ".container.volumeDevices[v]", nil, func(d *checkDocument) {
			d.template().Container.VolumeDevices = []corev1.VolumeDevice{{Name: "v", DevicePath: "/var/run/secrets/secretstore"}}
		}},
		{"r-token-only-secret", "token", template + ".volumes[login]", tokenDocument, func(d *checkDocument) {
			d.template().Volumes[0].Projected.Sources[0].Secret = &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}}
		}},
		{"r-token-single-main", "token", template + ".volumes[login]", tokenDocument, func(d *checkDocument) {
			d.template().ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "extra", Image: "image"}}}}
		}},
		{"r-archive", "read", template + ".archiveLocation.s3.accessKeySecret", nil, func(d *checkDocument) {
			d.template().ArchiveLocation = &wfv1.ArtifactLocation{S3: &wfv1.S3Artifact{S3Bucket: wfv1.S3Bucket{AccessKeySecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}}}}}
		}},
	}...)...)
}
func declarationChecks() []boundaryCheck {
	const template = `spec.templates["main"]`
	return []boundaryCheck{
		{"r-reserved-var-run-argo", "reserved", template + ".volumes[var-run-argo].name", nil, func(d *checkDocument) {
			d.template().Volumes = []corev1.Volume{{Name: "var-run-argo", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{"r-reserved-oberth-prefix", "reserved", template + ".volumes[oberth-private].name", nil, func(d *checkDocument) {
			d.template().Volumes = []corev1.Volume{{Name: "oberth-private", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}},
		{"r-meta-name", "reserved", "metadata.name", nil, func(d *checkDocument) { d.wf.Name = "server" }},
		{"r-meta-ownerrefs", "reserved", "metadata.ownerReferences", nil, func(d *checkDocument) {
			d.wf.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "server", UID: "server-uid"}}
		}},
		{"r-key-argo-prefix", "reserved", `metadata.labels["workflows.argoproj.io/workflow"]`, nil, func(d *checkDocument) { d.wf.Labels = map[string]string{"workflows.argoproj.io/workflow": "server"} }},
		{"r-labelsfrom", "reserved", "spec.workflowMetadata.labelsFrom[workflows.argoproj.io/workflow]", nil, func(d *checkDocument) {
			d.wf.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{LabelsFrom: map[string]wfv1.LabelValueFrom{"workflows.argoproj.io/workflow": {Expression: "'server'"}}}
		}},
		{"r-sem-name", "read", "spec.synchronization.semaphores[0]", semaphoreDocument, func(d *checkDocument) { d.wf.Spec.Synchronization.Semaphores[0].ConfigMapKeyRef.Name = "private" }},
		{"r-sem-namespace", "read", "spec.synchronization.semaphores[0]", semaphoreDocument, func(d *checkDocument) { d.wf.Spec.Synchronization.Semaphores[0].Namespace = "other" }},
		{"r-sem-database", "read", "spec.synchronization.semaphores[0]", semaphoreDocument, func(d *checkDocument) {
			d.wf.Spec.Synchronization.Semaphores[0].Database = &wfv1.SyncDatabaseRef{Key: "private"}
		}},
		{"r-data", "read", template + ".data", nil, func(d *checkDocument) { d.template().Data = &wfv1.Data{} }},
		{"r-artifact-repo-ref", "read", "spec.artifactRepositoryRef", nil, func(d *checkDocument) {
			d.wf.Spec.ArtifactRepositoryRef = &wfv1.ArtifactRepositoryRef{ConfigMap: "private", Key: "location"}
		}},
		{"r-wf-arg-artifacts", "artifact", "spec.arguments.artifacts", nil, func(d *checkDocument) {
			d.wf.Spec.Arguments.Artifacts = []wfv1.Artifact{{Name: "private", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "input"}}}}
		}},
		{"r-cluster-scope", "reference", template + ".dag.tasks[work]", sharedDocument, func(d *checkDocument) { d.template().DAG.Tasks[0].TemplateRef.ClusterScope = true }},
		{"r-source-exactkeys", "source", template + ".inputs.artifacts[source]", nil, func(d *checkDocument) {
			a := mainSource()
			a.Plugin.Configuration = `{"version":1,"Kind":"main"}`
			d.template().Inputs.Artifacts = []wfv1.Artifact{a}
		}},
		{"r-artifact-from", "artifact", template + ".inputs.artifacts[a]", nil, func(d *checkDocument) {
			d.template().Inputs.Artifacts = []wfv1.Artifact{{Name: "a", Path: "/input", From: "{{workflow.outputs.artifacts.private}}"}}
		}},
		{"r-image-dynamic", "image", template + ".container.image", func(d *checkDocument) { d.policy.ImagePrefixes = []string{"example.test/", "{{"} }, func(d *checkDocument) { d.template().Container.Image = "{{inputs.parameters.image}}" }},
		{"r-image-prefix", "image", template + ".container.image", func(d *checkDocument) { d.policy.ImagePrefixes = []string{"example.test/"} }, func(d *checkDocument) { d.template().Container.Image = "other.test/image" }},
		{"r-secret-dup", "secret_path", template + ".metadata.annotations[" + SecretPathsAnnotation + "][public/key]", func(d *checkDocument) {
			declareSecretPaths(d.template(), "public/key")
			d.request.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "secret", "public/key")}
			d.hooks.LoginAudience = "login"
		}, func(d *checkDocument) { declareSecretPaths(d.template(), "public/key,public/key") }},
	}
}
func semaphoreDocument(d *checkDocument) {
	d.wf.Spec.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "oberth-sync-limits"}, Key: "build"}}}}
}
func identityChecks() []boundaryCheck {
	const resource = `spec.templates["main"].resource`
	return []boundaryCheck{
		{"r-res-dynamic", "resource", resource + ".manifest", func(d *checkDocument) {
			resourceDocument(d)
			d.template().Inputs.Parameters = []wfv1.Parameter{{Name: "value", Value: wfv1.AnyStringPtr("test")}}
		}, func(d *checkDocument) {
			d.template().Resource.Manifest += `
spec: {message: "{{inputs.parameters.value}}"}`
		}},
		{"r-res-release", "release_resource", resource, resourceDocument, func(d *checkDocument) { d.policy.Release = true; d.request.Trigger = model.TriggerTag }},
		{"r-res-helpers", "capability", resource, resourceDocument, func(d *checkDocument) {
			d.template().InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "helper", Image: "image"}}}
		}},
		{"r-res-flags", "resource", resource + ".manifest", resourceDocument, func(d *checkDocument) { d.template().Resource.Flags = []string{"--server-side"} }},
		{"r-res-identity", "identity", resource, resourceDocument, func(d *checkDocument) { d.policy.Capability.ServiceAccount = "other-executor" }},
		{"r-id-resource-alias", "identity", "identity", resourceDocument, func(d *checkDocument) {
			d.request.IdentityRequest.ResourceAccount = d.request.IdentityRequest.PipelineAccount
			d.policy.Capability.ServiceAccount = d.request.IdentityRequest.PipelineAccount
		}},
		{"r-id-uid", "identity", "identity", nil, func(d *checkDocument) {
			verify := d.hooks.VerifyIdentities
			d.hooks.VerifyIdentities = func(ctx context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
				e, err := verify(ctx, r)
				e.Pipeline.UID = ""
				return e, err
			}
		}},
		{"r-id-pipeline-default", "identity", "identity", nil, func(d *checkDocument) { d.request.IdentityRequest.PipelineAccount = "default" }},
		{"r-binding-count", "binding", "source_bindings[main/source]", tokenDocument, func(d *checkDocument) { d.bindings = nil }},
		{"r-cap-default-sa", "capability", "policy.capability", resourceDocument, func(d *checkDocument) { d.policy.Capability.ServiceAccount = "default" }},
		{"r-cap-list-kind", "capability", "policy.capability", resourceDocument, func(d *checkDocument) {
			d.policy.Capability.ResourceKinds[0].Kind = "WidgetList"
			d.template().Resource.Manifest = strings.Replace(d.template().Resource.Manifest, "Widget", "WidgetList", 1)
		}},
	}
}

func sharedAndHookChecks() []boundaryCheck {
	return []boundaryCheck{
		{"credentialed-source-input", "source", `spec.templates["main"].inputs.artifacts["tool"]`, tokenDocument, func(d *checkDocument) {
			d.template().Inputs.Artifacts = append(d.template().Inputs.Artifacts, wfv1.Artifact{Name: "tool", Path: "/tool", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "authored"}}})
		}},
		{"ordinary-projection-protected-path", "path", `spec.templates["main"].container.volumeMounts[v].mountPath["/var/run/secrets/secretstore"]`, func(d *checkDocument) {
			d.volume(corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{DownwardAPI: &corev1.DownwardAPIProjection{}}}}})
			d.template().Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/info"}}
		}, func(d *checkDocument) {
			d.template().Container.VolumeMounts[0].MountPath = "/var/run/secrets/secretstore"
		}},
		{"r-hook-arguments", "artifact", `spec.templates["main"].dag.tasks[run].hooks[exit].arguments.artifacts[x]`, func(d *checkDocument) {
			d.wf = planDAG()
			d.template().DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf"}}
		}, func(d *checkDocument) {
			h := d.template().DAG.Tasks[0].Hooks["exit"]
			h.Arguments.Artifacts = []wfv1.Artifact{{Name: "x", ArtifactLocation: wfv1.ArtifactLocation{Raw: &wfv1.RawArtifact{Data: "input"}}}}
			d.template().DAG.Tasks[0].Hooks["exit"] = h
		}},
	}
}
