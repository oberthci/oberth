package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func nonrootVerifierObjects() (*corev1.Pod, *corev1.ConfigMap) {
	created := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: argoworkflow.NonrootProfileConfigMap, Namespace: testNamespace, UID: "config-uid", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(created)}, Immutable: ptr.To(true), Data: map[string]string{"config": argoworkflow.NonrootProfileConfig}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "controller-fixture", Namespace: testNamespace, UID: "pod-uid", Labels: map[string]string{"app.kubernetes.io/name": "argo-workflows-workflow-controller"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "controller", UID: "replicaset-uid", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: argoworkflow.NonrootControllerImage, Command: []string{"workflow-controller"}, Args: []string{"--configmap", argoworkflow.NonrootProfileConfigMap, "--executor-image", argoworkflow.NonrootExecutorImage, "--loglevel", "info", "--gloglevel", "0", "--log-format", "text", "--namespaced"}, Env: []corev1.EnvVar{
		{Name: "ARGO_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		{Name: "LEADER_ELECTION_IDENTITY", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		{Name: "LEADER_ELECTION_DISABLE", Value: "true"}, {Name: "POD_NAMES", Value: "v2"},
	}}}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true, ImageID: argoworkflow.NonrootControllerAMD64, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(created.Add(10 * time.Second))}}}}}}
	nonrootControllerMountFixture(pod)
	return pod, config
}

// This models the standard service-account admission projection independently
// of the production matcher. No API token or Secret data is part of the fixture.
func nonrootControllerMountFixture(pod *corev1.Pod) {
	pod.Spec.Volumes = []corev1.Volume{
		{Name: argoworkflow.NonrootProfileVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: argoworkflow.NonrootProfileConfigMap}, Optional: ptr.To(false), DefaultMode: ptr.To(int32(0444)), Items: []corev1.KeyToPath{{Key: "config", Path: "config"}}}}},
		{Name: "kube-api-access-fixture", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0644)), Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: argoworkflow.NonrootProfileVolume, MountPath: argoworkflow.NonrootProfileMount, ReadOnly: true},
		{Name: "kube-api-access-fixture", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
	}
}

func TestNonrootControllerRequiredConfigCausality(t *testing.T) {
	for _, mutate := range []func(*corev1.Pod, *corev1.ConfigMap){
		func(p *corev1.Pod, cm *corev1.ConfigMap) {
			p.Status.ContainerStatuses[0].State.Running.StartedAt = cm.CreationTimestamp
		},
		func(p *corev1.Pod, _ *corev1.ConfigMap) { p.Spec.Volumes[0].ConfigMap.Optional = nil },
	} {
		pod, cm := nonrootVerifierObjects()
		mutate(pod, cm)
		if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(*corev1.Pod){
		"missing-volume": func(p *corev1.Pod) { p.Spec.Volumes = p.Spec.Volumes[1:] },
		"optional":       func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Optional = ptr.To(true) },
		"other-config":   func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Name = "other" },
		"missing-key":    func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Items = nil },
		"other-key":      func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Items[0].Key = "other" },
		"other-path":     func(p *corev1.Pod) { p.Spec.Volumes[0].ConfigMap.Items[0].Path = "other" },
		"writable":       func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].ReadOnly = false },
		"subpath":        func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].SubPath = "config" },
		"subpath-expr":   func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].SubPathExpr = "$(NAME)" },
		"propagation": func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts[0].MountPropagation = ptr.To(corev1.MountPropagationHostToContainer)
		},
		"duplicate-mount":  func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[1] = p.Spec.Containers[0].VolumeMounts[0] },
		"overlap":          func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[1].MountPath = "/var/run" },
		"duplicate-volume": func(p *corev1.Pod) { p.Spec.Volumes[1] = p.Spec.Volumes[0] },
		"extra-mount": func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "extra", MountPath: "/extra"})
		},
		"other-source-at-sa-path": func(p *corev1.Pod) {
			p.Spec.Volumes[1].VolumeSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "other"}}
		},
		"extra-projection": func(p *corev1.Pod) {
			p.Spec.Volumes[1].Projected.Sources = append(p.Spec.Volumes[1].Projected.Sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{}})
		},
		"alternate-audience": func(p *corev1.Pod) { p.Spec.Volumes[1].Projected.Sources[0].ServiceAccountToken.Audience = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pod, cm := nonrootVerifierObjects()
			pod.Status.ContainerStatuses[0].State.Running.StartedAt = cm.CreationTimestamp
			mutate(pod)
			if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err == nil {
				t.Fatal("uncausal or injected profile accepted")
			}
		})
	}
}

func TestNonrootProfileMatchesActualUpstreamHelmRender(t *testing.T) {
	// Reuse the authenticated Argo version/checksum guard, then verify the
	// independently rendered exact upstream chart template with ordinary API
	// metadata/status and its existing standard admission token projection.
	_ = nonrootControllerFixture(t, "container-before-patch.json")
	data, err := os.ReadFile(filepath.Join(nonrootControllerFixtureDirectory, "controller-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Files map[string]string `json:"files"`
	}
	encoded, err := os.ReadFile(filepath.Join(nonrootControllerFixtureDirectory, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &provenance); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if provenance.Files["controller-profile.json"] != hex.EncodeToString(digest[:]) {
		t.Fatal("rendered chart fixture integrity mismatch")
	}
	var fixture struct {
		ChartVersion string                 `json:"chartVersion"`
		ChartSHA256  string                 `json:"chartSHA256"`
		PodTemplate  corev1.PodTemplateSpec `json:"podTemplate"`
		ConfigMap    corev1.ConfigMap       `json:"configMap"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.ChartVersion != "1.0.24" || fixture.ChartSHA256 != "7b1d540095ba32bb8c914432b554c64809b47a6f737b1448273d3a4740be3d50" {
		t.Fatal("unproved controller chart semantics")
	}
	pod, meta := nonrootVerifierObjects()
	accountVolume := pod.Spec.Volumes[1]
	accountMount := pod.Spec.Containers[0].VolumeMounts[1]
	pod.Spec = fixture.PodTemplate.Spec
	pod.Spec.Volumes = append(pod.Spec.Volumes, accountVolume)
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, accountMount)
	fixture.ConfigMap.ObjectMeta = meta.ObjectMeta
	pod.Status.ContainerStatuses[0].State.Running.StartedAt = meta.CreationTimestamp
	if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, &fixture.ConfigMap), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err != nil {
		t.Fatal(err)
	}
}

func TestNonrootVerifierRejectsUnsupportedControllerState(t *testing.T) {
	cases := map[string]func(*corev1.Pod, *corev1.ConfigMap){
		"mutable-config": func(_ *corev1.Pod, c *corev1.ConfigMap) { c.Immutable = ptr.To(false) },
		"archive-injection": func(_ *corev1.Pod, c *corev1.ConfigMap) {
			c.Data["config"] = `{"artifactRepository":{"archiveLogs":true}}`
		},
		"extra-config-key": func(_ *corev1.Pod, c *corev1.ConfigMap) { c.Data["private"] = "do-not-serialize-this-value" },
		"unknown-version": func(p *corev1.Pod, _ *corev1.ConfigMap) {
			p.Spec.Containers[0].Image = "quay.io/argoproj/workflow-controller:v4.0.8"
		},
		"wrong-running-image": func(p *corev1.Pod, _ *corev1.ConfigMap) { p.Status.ContainerStatuses[0].ImageID = "sha256:unknown" },
		"alternate-watch": func(p *corev1.Pod, _ *corev1.ConfigMap) {
			p.Spec.Containers[0].Args = append(p.Spec.Containers[0].Args, "--instanceid=other")
		},
		"environment-injection": func(p *corev1.Pod, _ *corev1.ConfigMap) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "LD_PRELOAD", Value: "private"})
		},
		"env-from": func(p *corev1.Pod, _ *corev1.ConfigMap) { p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{}} },
		"executable-mount": func(p *corev1.Pod, _ *corev1.ConfigMap) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "overlay", MountPath: "/usr/local/bin", ReadOnly: true}}
		},
		"sidecar": func(p *corev1.Pod, _ *corev1.ConfigMap) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "other"})
		},
		"not-ready": func(p *corev1.Pod, _ *corev1.ConfigMap) { p.Status.Conditions = nil },
		"unowned":   func(p *corev1.Pod, _ *corev1.ConfigMap) { p.OwnerReferences = nil },
		"same-second-uncausal": func(p *corev1.Pod, c *corev1.ConfigMap) {
			p.Spec.Containers[0].VolumeMounts = p.Spec.Containers[0].VolumeMounts[1:]
			p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(c.CreationTimestamp.Add(500 * time.Millisecond))
		},
		"later-replacement": func(p *corev1.Pod, c *corev1.ConfigMap) {
			c.CreationTimestamp = metav1.NewTime(p.Status.ContainerStatuses[0].State.Running.StartedAt.Add(time.Second))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pod, cm := nonrootVerifierObjects()
			mutate(pod, cm)
			_, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			if err == nil {
				t.Fatal("unsupported controller accepted")
			}
			if strings.Contains(err.Error(), "do-not-serialize") {
				t.Fatal("configuration content escaped into error")
			}
		})
	}
	t.Run("ambiguous", func(t *testing.T) {
		pod, cm := nonrootVerifierObjects()
		other := pod.DeepCopy()
		other.Name = "second"
		other.UID = "second"
		if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, other, cm), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err == nil {
			t.Fatal("ambiguous controllers accepted")
		}
	})
	t.Run("read-failure", func(t *testing.T) {
		pod, cm := nonrootVerifierObjects()
		client := fake.NewClientset(pod, cm)
		client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("do-not-serialize-this-value")
		})
		_, err := verifyNonrootController(t.Context(), client, testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
		if err == nil || strings.Contains(err.Error(), "do-not-serialize") {
			t.Fatalf("unsafe read error: %v", err)
		}
	})
}

func TestNonrootProofBindsActualRequestAndPolicy(t *testing.T) {
	pod, cm := nonrootVerifierObjects()
	client := fake.NewClientset(pod, cm)
	config := testConfig()
	config.NonrootProfile = argoworkflow.NonrootStaticProfile
	controller, err := NewController(&staticWorkflowClient{}, client, config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := controller.PrepareNonroot(t.Context(), testRequest(periapsis.TriggerCI, nonrootTestDocument))
	if err != nil {
		t.Fatal(err)
	}
	good, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	if good.Annotations[nonrootProfileAnnotation] == "" {
		t.Fatal("proof missing from server-owned submission")
	}
	for _, name := range []string{"sha", "source", "fragment-bytes", "namespace", "expired", "no-proof"} {
		t.Run(name, func(t *testing.T) {
			changed := request
			changedConfig := config
			switch name {
			case "sha":
				changed.SHA = strings.Repeat("b", 40)
			case "source":
				changed.Source = append(append([]byte(nil), request.Source...), []byte("\n# changed\n")...)
			case "fragment-bytes":
				changed.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{{Repo: "peer", Version: "v1"}: {Source: []byte("other")}}
			case "namespace":
				changedConfig.Namespace = "other"
			case "expired":
				copy := *request.nonrootProof
				copy.verifiedAt = time.Now().Add(-time.Minute)
				changed.nonrootProof = &copy
			case "no-proof":
				changed.nonrootProof = nil
			}
			if _, err := Build(changedConfig, changed); err == nil {
				t.Fatal("unbound request accepted")
			}
		})
	}
	for _, name := range []string{"root", "writable-source", "patch", "profile"} {
		t.Run("adoption-"+name, func(t *testing.T) {
			old := good.DeepCopy()
			switch name {
			case "root":
				old.Spec.Templates[0].Container.SecurityContext.RunAsUser = ptr.To(int64(0))
			case "writable-source":
				old.Spec.Templates[0].Container.VolumeMounts = append(old.Spec.Templates[0].Container.VolumeMounts, corev1.VolumeMount{Name: "foreign", MountPath: "/work/foreign"})
			case "patch":
				old.Spec.Templates[0].PodSpecPatch = ""
			case "profile":
				delete(old.Annotations, nonrootProfileAnnotation)
			}
			if err := sameSubmission(old, good); err == nil {
				t.Fatal("matching commit/identity annotations accepted old semantics")
			}
		})
	}
	if err := sameSubmission(good.DeepCopy(), good); err != nil {
		t.Fatal(err)
	}
}

func TestNonrootRequestBindingCoversEveryInputExceptExpectedVolume(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, nonrootTestDocument)
	request.SourceDir = "/trusted/source"
	base := nonrootRequestIdentity(request)
	changes := map[string]func(*Request){
		"run":              func(r *Request) { r.RunID += "-other" },
		"name":             func(r *Request) { r.Name += "-other" },
		"repo":             func(r *Request) { r.Repo += "-other" },
		"upstream":         func(r *Request) { r.UpstreamName += "-other" },
		"organization":     func(r *Request) { r.UpstreamOrg += "-other" },
		"ref":              func(r *Request) { r.Ref += "-other" },
		"sha":              func(r *Request) { r.SHA = strings.Repeat("b", 40) },
		"trigger":          func(r *Request) { r.Trigger = periapsis.TriggerRelease },
		"source-directory": func(r *Request) { r.SourceDir += "-other" },
		"source-bytes":     func(r *Request) { r.Source = append(append([]byte(nil), r.Source...), '\n') },
		"secret-grants":    func(r *Request) { r.ApprovedSecrets = map[string]bool{"other": true} },
		"fragment": func(r *Request) {
			r.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{{Repo: "peer", Version: "v1"}: {SHA: "sha", Digest: "digest", Source: []byte("actual")}}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			copy := request
			change(&copy)
			if nonrootRequestIdentity(copy) == base {
				t.Fatal("request input is not bound")
			}
		})
	}
	request.SourceVolume = SourceVolume{ClaimName: "expected-run-claim", SubPath: "src", ArtifactsSubPath: "artifacts"}
	if nonrootRequestIdentity(request) != base {
		t.Fatal("server-assigned source volume invalidates pre-seed identity")
	}
	request.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{{Repo: "peer", Version: "v1"}: {SHA: "sha", Digest: "digest", Source: []byte("actual")}}
	base = nonrootRequestIdentity(request)
	for _, field := range []string{"sha", "digest", "bytes", "repo", "version"} {
		copy := request
		key := argoworkflow.FragmentKey{Repo: "peer", Version: "v1"}
		fragment := request.Fragments[key]
		switch field {
		case "sha":
			fragment.SHA = "other"
		case "digest":
			fragment.Digest = "other"
		case "bytes":
			fragment.Source = []byte("other")
		case "repo":
			key.Repo = "other"
		case "version":
			key.Version = "other"
		}
		copy.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment}
		if nonrootRequestIdentity(copy) == base {
			t.Fatalf("fragment %s is not bound", field)
		}
	}
}

type nonrootWorkflowClient struct {
	existing *wfv1.Workflow
	creates  int
}

func (client *nonrootWorkflowClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	if client.existing == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, name)
	}
	return client.existing.DeepCopy(), nil
}
func (client *nonrootWorkflowClient) Create(_ context.Context, workflow *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	client.creates++
	// Model API serialization and ordinary persisted metadata/status; do not
	// accidentally prove recovery only against a DeepCopy of the input object.
	encoded, err := json.Marshal(workflow)
	if err != nil {
		return nil, err
	}
	client.existing = &wfv1.Workflow{}
	if err := json.Unmarshal(encoded, client.existing); err != nil {
		return nil, err
	}
	client.existing.UID = "workflow-uid"
	client.existing.ResourceVersion = "17"
	client.existing.CreationTimestamp = metav1.Now()
	client.existing.Status.Phase = wfv1.WorkflowRunning
	client.existing.Labels["workflows.argoproj.io/phase"] = "Running"
	return client.existing.DeepCopy(), nil
}
func (client *nonrootWorkflowClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	return errors.New("unexpected Workflow deletion")
}

func TestNonrootRefreshStopsSeedOrCreateAfterConfigurationChange(t *testing.T) {
	for _, failAt := range []int{2, 3, 4} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			pod, cm := nonrootVerifierObjects()
			client := fake.NewClientset(pod, cm)
			runningSeedPods(client)
			reads := 0
			client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				reads++
				if reads == failAt {
					changed := cm.DeepCopy()
					changed.ResourceVersion = "2"
					return true, changed, nil
				}
				return false, nil, nil
			})
			config := testConfig()
			config.NonrootProfile = argoworkflow.NonrootStaticProfile
			workflows := &nonrootWorkflowClient{}
			controller, err := NewController(workflows, client, config)
			if err != nil {
				t.Fatal(err)
			}
			seedCalls := 0
			controller.WithSourceSeeder(NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string, stdin io.Reader, _, _ io.Writer) error {
				seedCalls++
				if stdin != nil {
					_, err := io.Copy(io.Discard, stdin)
					return err
				}
				return nil
			}, config))
			request := testRequest(periapsis.TriggerCI, nonrootTestDocument)
			request.SourceDir = writeCheckout(t)
			request, err = controller.PrepareNonroot(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Create(t.Context(), request); err == nil {
				t.Fatal("changed configuration accepted")
			}
			if workflows.creates != 0 {
				t.Fatal("Workflow submitted after proof changed")
			}
			if failAt < 4 && seedCalls != 0 {
				t.Fatal("source seeded before verification")
			}
			if failAt == 4 && seedCalls == 0 {
				t.Fatal("did not exercise post-seed rejection")
			}
		})
	}
}

func TestNonrootCreateAndRecoverPersistedWorkflow(t *testing.T) {
	for _, script := range []bool{false, true} {
		pod, cm := nonrootVerifierObjects()
		client := fake.NewClientset(pod, cm)
		runningSeedPods(client)
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		workflows := &nonrootWorkflowClient{}
		controller, err := NewController(workflows, client, config)
		if err != nil {
			t.Fatal(err)
		}
		seedCalls := 0
		controller.WithSourceSeeder(NewSourceSeeder(client, func(_ context.Context, _, _, _ string, _ []string, stdin io.Reader, _, _ io.Writer) error {
			seedCalls++
			if stdin != nil {
				_, err := io.Copy(io.Discard, stdin)
				return err
			}
			return nil
		}, config))
		source := nonrootTestDocument
		if script {
			workflow, decodeErr := argoworkflow.Decode([]byte(source))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			template := &workflow.Spec.Templates[0]
			template.Script = &wfv1.ScriptTemplate{Container: *template.Container, Source: "id"}
			template.Script.Command = []string{"/bin/sh"}
			template.Container = nil
			encoded, encodeErr := json.Marshal(workflow)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			source = string(encoded)
		}
		request := testRequest(periapsis.TriggerCI, source)
		request.SourceDir = writeCheckout(t)
		request, err = controller.PrepareNonroot(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Create(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if workflows.creates != 1 || seedCalls == 0 {
			t.Fatal("create did not use the verified seeding path")
		}
		before := seedCalls
		// A recovered request is prepared anew after a server restart; expected
		// SourceVolume assignment must not alter its pre-seed request binding.
		request.nonrootProof = nil
		request, err = controller.PrepareNonroot(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Create(t.Context(), request); err != nil {
			t.Fatalf("persisted %v workflow could not recover: %v", script, err)
		}
		if workflows.creates != 1 || seedCalls != before {
			t.Fatal("recovery rewrote or recreated the running source")
		}
		// Even with matching annotations, altered persisted policies cannot be
		// adopted, and a newly observed ConfigMap identity invalidates the proof.
		workflows.existing.Spec.Templates[0].PodSpecPatch = ""
		if _, err := controller.Create(t.Context(), request); err == nil {
			t.Fatal("old/root stored workflow adopted")
		}
		if seedCalls != before {
			t.Fatal("failed adoption modified source")
		}
	}
}
