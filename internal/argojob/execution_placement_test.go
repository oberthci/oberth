package argojob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func placementFixture(t *testing.T) (Config, Request, []runtime.Object) {
	t.Helper()
	config, request := trustedKVMRequest(t)
	config.applyDefaults()
	config.SourceStorageClass = "isolated-fixture"
	config.GoProxyURL = "https://proxy.example.test:8444"
	config.GoProxyModulePrefix = "go.example.test"
	config.GoProxyCACertPEM = testVaultAnchorPEM
	request.SourceVolume = (&Controller{config: config}).expectedSourceVolume(request)
	request.PipelineFile = ".oberth/build.yaml"
	d := placementDocument{}
	// Explicitly synthetic identities; authenticated runtime evidence is external.
	fields := reflect.ValueOf(&d).Elem()
	for i := 0; i < fields.NumField(); i++ {
		if fields.Type().Field(i).Name == "ParentRunID" || fields.Type().Field(i).Name == "NormalWorkflow" || fields.Type().Field(i).Name == "PreflightWorkflow" || fields.Type().Field(i).Name == "BranchPolicyBaseline" || fields.Type().Field(i).Name == "GraphDigest" || fields.Type().Field(i).Name == "FragmentLockDigest" {
			continue
		}
		if fields.Field(i).Kind() == reflect.String {
			fields.Field(i).SetString("fixture")
		}
	}
	d.Schema = "oberth.execution-placement/v1"
	d.Repository = "codeberg/acme/oberth"
	d.RepositoryID = request.RepositoryID
	d.Ref = request.Ref
	d.SHA = request.SHA
	d.SourceDigest = placementHashRaw(request.Source)
	d.PipelineFile = request.PipelineFile
	d.Namespace = config.Namespace
	d.ServiceAccount = config.PipelineServiceAccount
	d.IntentDigest = strings.Repeat("a", 64)
	d.PolicyDigest = strings.Repeat("b", 64)
	d.NativeIdentity = executionBinaryIdentity().digest
	d.DependencyIdentity = placementDependencyIdentity(config)
	d.AdmissionConfiguration = strings.Repeat("c", 64)
	d.ClusterCreated = "2026-01-01T00:00:00Z"
	d.Expires = time.Now().Add(time.Hour)
	d.NodeName = "fixture-worker"
	d.NodeUID = "node-uid"
	d.ProviderID = "gce://example-project/example-zone/fixture-worker"
	d.Zone = "example-zone"
	d.Pool = "nested"
	d.TaintKey = "oberth.ci/lane"
	d.TaintValue = "nested"
	d.NodeVersion = "v1.35.7-fixture"
	d.ControlPlaneVersion = "v1.35.7-fixture"
	d.EndpointIdentity = placementHash([]any{"https://api.example.org", "", []byte("fixture-ca")})
	pod, controllerCM := nonrootVerifierObjects()
	d.ControllerConfigUID = string(controllerCM.UID)
	d.ControllerPod = pod.Name
	d.ControllerUID = string(pod.UID)
	d.ReplicaSet = "controller"
	d.ReplicaSetUID = "replicaset-uid"
	d.Deployment = "controller"
	d.DeploymentUID = "deployment-uid"
	d.AdmissionConfigMap = "admission-fixture"
	d.AdmissionUID = "admission-uid"
	d.AdmissionProfile = "kube1357-dts300"
	d.Normal = placementRun{RunID: "normal-run", Workflow: "normal-workflow"}
	d.Preflight = placementRun{RunID: request.RunID, Workflow: request.Name, TrustedIntent: vmrunner.KVMIntentDigest(*request.TrustedKVM)}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.AdmissionConfigMap, Namespace: d.Namespace, UID: types.UID(d.AdmissionUID), ResourceVersion: "1"}, Immutable: ptr.To(true), Data: map[string]string{"cluster_id": d.ClusterID, "cluster_created": d.ClusterCreated, "control_plane_version": d.ControlPlaneVersion, "node_version": d.NodeVersion, "admission_configuration": d.AdmissionConfiguration, "profile": d.AdmissionProfile, "intent_digest": d.IntentDigest, "policy_digest": d.PolicyDigest}}
	d.AdmissionDigest = placementHash(cm.Data)
	body, _ := json.Marshal(d)
	p, err := ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutionPlacement = p
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: d.NodeName, UID: types.UID(d.NodeUID), Labels: p.selectors()}, Spec: corev1.NodeSpec{ProviderID: d.ProviderID}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: d.NodeVersion}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Allocatable: corev1.ResourceList{corev1.ResourceName(kvmDeviceResource): resource.MustParse("4")}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: d.ReplicaSet, Namespace: d.Namespace, UID: types.UID(d.ReplicaSetUID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Deployment, UID: types.UID(d.DeploymentUID), Controller: ptr.To(true)}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Deployment, Namespace: d.Namespace, UID: types.UID(d.DeploymentUID)}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	request = prepareTrustedTestRequest(t, config, request)
	return config, request, []runtime.Object{pod, controllerCM, node, rs, deployment, cm, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(d.ClusterAnchorUID)}}}
}

func TestExecutionPlacementBuildAndDefault(t *testing.T) {
	config, request, _ := placementFixture(t)
	for _, preflight := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "preflight"}[preflight], func(t *testing.T) {
			r := request
			if !preflight {
				r.TrustedKVM = nil
				r.RunID = config.ExecutionPlacement.document.Normal.RunID
				r.Name = config.ExecutionPlacement.document.Normal.Workflow
				r.SourceVolume = (&Controller{config: config}).expectedSourceVolume(r)
			}
			wf, err := Build(config, r)
			if err != nil {
				t.Fatal(err)
			}
			if !preflight {
				audit := r
				audit.SourceVolume = SourceVolume{}
				predicted, err := Build(config, audit)
				if err != nil || !reflect.DeepEqual(predicted, wf) {
					t.Fatal("pre-seed audit differs from actual normal delivery", err)
				}
			}
			count := 0
			walkTemplates(wf, func(leaf *wfv1.Template) {
				if leaf.Container == nil && leaf.Script == nil {
					return
				}
				count++
				if !reflect.DeepEqual(leaf.NodeSelector, config.ExecutionPlacement.selectors()) || !equalPlacementTolerations(leaf.Tolerations, config.ExecutionPlacement.nativeTolerations()) {
					t.Fatal("leaf not placed")
				}
				var patch struct {
					Volumes        []corev1.Volume    `json:"volumes"`
					Containers     []corev1.Container `json:"containers"`
					InitContainers []corev1.Container `json:"initContainers"`
				}
				if err := json.Unmarshal([]byte(leaf.PodSpecPatch), &patch); err != nil {
					t.Fatal(err)
				}
				caps := map[string]string{}
				for _, v := range patch.Volumes {
					if v.EmptyDir != nil && v.EmptyDir.SizeLimit != nil {
						caps[v.Name] = v.EmptyDir.SizeLimit.String()
					}
				}
				if caps["var-run-argo"] != "1Gi" || caps["tmp-dir-argo"] != "64Mi" {
					t.Fatalf("caps %v", caps)
				}
				for _, c := range append(patch.Containers, patch.InitContainers...) {
					if c.Name == "wait" || c.Name == "init" || c.Name == executorTokenWaiter {
						if !apiequality.Semantic.DeepEqual(c.Resources, executorControlResources()) {
							t.Fatalf("helper resources %s", c.Name)
						}
					}
				}
			})
			if count < 1 {
				t.Fatal("no leaves")
			}
			if destination := os.Getenv("OBERTH_PLACEMENT_EXPORT"); destination != "" {
				body, _ := json.MarshalIndent(wf, "", "  ")
				if err := os.WriteFile(filepath.Join(destination, map[bool]string{false: "normal", true: "preflight"}[preflight]+"-workflow.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	config.ExecutionPlacement = nil
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range wf.Spec.Templates {
		if len(leaf.Tolerations) != 0 {
			t.Fatal("default behavior changed")
		}
	}
}

func TestExecutionPlacementSelectionRefusesBeforeSeed(t *testing.T) {
	config, request, objects := placementFixture(t)
	mutations := map[string]func(*Request){"source-volume": func(r *Request) { r.SourceVolume.ClaimName = "foreign" },
		"run": func(r *Request) { r.RunID = "other" }, "workflow": func(r *Request) { r.Name = "other" }, "repository": func(r *Request) { r.Repo = "other" }, "repo-id": func(r *Request) { r.RepositoryID++ }, "ref": func(r *Request) { r.Ref = "refs/heads/other" }, "source": func(r *Request) { r.Source = append(append([]byte{}, r.Source...), 10) }, "sha": func(r *Request) { r.SHA = strings.Repeat("b", 40) }, "pipeline": func(r *Request) { r.PipelineFile = ".oberth/other.yaml" }, "intent": func(r *Request) { v := *r.TrustedKVM; v.InventoryDigest = strings.Repeat("1", 64); r.TrustedKVM = &v }, "trigger": func(r *Request) { r.Trigger = pipeline.TriggerRelease }}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := request
			mutate(&r)
			if _, err := Build(config, r); err == nil {
				t.Fatal("Build accepted changed binding")
			}
			client := placementClient(objects...)
			seeder := NewSourceSeeder(client, recordingExec(nil), config)
			if _, err := seeder.SeedRequest(t.Context(), r, false, false); err == nil {
				t.Fatal("seed accepted changed binding")
			}
			if len(client.Actions()) != 0 {
				t.Fatal("seed performed I/O before selection")
			}
		})
	}
	client := placementClient(objects...)
	seeder := NewSourceSeeder(client, recordingExec(nil), config)
	if _, err := seeder.Seed(t.Context(), request.Name, t.TempDir(), false); err == nil {
		t.Fatal("legacy seed bypass")
	}
	if len(client.Actions()) != 0 {
		t.Fatal("legacy seed touched cluster")
	}
}

func TestExecutionPlacementConfigurationAndEnvironment(t *testing.T) {
	config, request, objects := placementFixture(t)
	p := config.ExecutionPlacement
	if err := p.verifyEnvironment(t.Context(), placementClient(objects...), config); err != nil {
		t.Fatal(err)
	}
	changed := placementClient(objects...)
	changed.serverVersion = "v1.35.8-fixture"
	if err := p.verifyEnvironment(t.Context(), changed, config); err == nil {
		t.Fatal("changed control plane accepted")
	}
	if err := p.verifyEnvironment(t.Context(), fake.NewClientset(objects...), config); err == nil {
		t.Fatal("missing authenticated discovery accepted")
	}
	if err := p.VerifyTarget(&rest.Config{Host: "https://api.example.org", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("fixture-ca")}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*rest.Config{nil, {Host: "https://other.example.org"}, {Host: "https://api.example.org", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("fixture-ca"), Insecure: true}}} {
		if err := p.VerifyTarget(target); err == nil {
			t.Fatal("transport mismatch")
		}
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("fixture-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyTarget(&rest.Config{Host: "https://api.example.org", TLSClientConfig: rest.TLSClientConfig{CAFile: caFile}}); err != nil {
		t.Fatal(err)
	}
	// Every required scalar identity is independently mandatory.
	fields := reflect.ValueOf(&p.document).Elem()
	for i := 0; i < fields.NumField(); i++ {
		if fields.Field(i).Kind() != reflect.String {
			continue
		}
		t.Run(fields.Type().Field(i).Name, func(t *testing.T) {
			d := p.document
			value := reflect.ValueOf(&d).Elem().Field(i)
			if value.String() == "" {
				value.SetString("unexpected-branch-identity")
			} else {
				value.SetString("")
			}
			body, _ := json.Marshal(d)
			if _, err := ParseExecutionPlacement(body); err == nil {
				t.Fatal("missing required identity or foreign branch identity accepted")
			}
		})
	}
	body, _ := json.Marshal(p.document)
	for _, bad := range [][]byte{append([]byte(`{"schema":"duplicate",`), body[1:]...), []byte(strings.Replace(string(body), `"schema"`, `"Schema"`, 1)), append(body, []byte(` {}`)...), []byte(strings.Replace(string(body), `"schema":`, `"unknown":`, 1))} {
		if _, err := ParseExecutionPlacement(bad); err == nil {
			t.Fatal("ambiguous config accepted")
		}
	}
	for index := range objects {
		t.Run(reflect.TypeOf(objects[index]).String()+string(rune('a'+index)), func(t *testing.T) {
			copies := make([]runtime.Object, len(objects))
			for i, v := range objects {
				copies[i] = v.DeepCopyObject()
			}
			meta := copies[index].(metav1.Object)
			meta.SetUID("replaced")
			if err := p.verifyEnvironment(t.Context(), placementClient(copies...), config); err == nil {
				t.Fatal("replaced provenance accepted")
			}
		})
	}
	client := placementClient(objects...)
	seeder := NewSourceSeeder(client, recordingExec(nil), config)
	if err := seeder.createSeedPod(t.Context(), sourceClaimName(request.Name), sourceClaimName(request.Name), &request); err == nil {
		t.Fatal("fake API without UID/RV claimed authenticated seed")
	}
}

func TestExecutionPlacementRolesAndCanonicalRefusals(t *testing.T) {
	config, _, _ := placementFixture(t)
	p := config.ExecutionPlacement
	for _, profile := range []string{"kube1357-dts300", "kube1357-dts300-ptr-empty"} {
		p.document.AdmissionProfile = profile
		p.digest = placementHash(p.document)
		for _, role := range []string{"candidate", "harness", "seed", "control-deployment", "daemonset", "hostnetwork-daemonset"} {
			t.Run(profile+"/"+role, func(t *testing.T) {
				expected, err := p.admittedTolerations(role)
				if err != nil {
					t.Fatal(err)
				}
				if !equalPlacementTolerations(expected, expected) {
					t.Fatal("positive")
				}
				reordered := append([]corev1.Toleration{}, expected...)
				for i, j := 0, len(reordered)-1; i < j; i, j = i+1, j-1 {
					reordered[i], reordered[j] = reordered[j], reordered[i]
				}
				if !equalPlacementTolerations(reordered, expected) {
					t.Fatal("canonical order")
				}
				for i := range expected {
					for _, change := range []func(*corev1.Toleration){func(t *corev1.Toleration) { t.Key = "" }, func(t *corev1.Toleration) {
						t.Operator = corev1.TolerationOpEqual
						if t.Value == "nested" {
							t.Operator = corev1.TolerationOpExists
						}
					}, func(t *corev1.Toleration) { t.Effect = "" }, func(t *corev1.Toleration) { t.Value = "other" }, func(t *corev1.Toleration) { t.TolerationSeconds = ptr.To(int64(0)) }, func(t *corev1.Toleration) {
						if t.TolerationSeconds == nil {
							t.TolerationSeconds = ptr.To(int64(300))
						} else {
							t.TolerationSeconds = nil
						}
					}} {
						mutated := append([]corev1.Toleration{}, expected...)
						change(&mutated[i])
						if equalPlacementTolerations(mutated, expected) {
							t.Fatalf("accepted mutation %d", i)
						}
					}
					if equalPlacementTolerations(append(append([]corev1.Toleration{}, expected...), expected[i]), expected) {
						t.Fatal("duplicate")
					}
					if equalPlacementTolerations(append(append([]corev1.Toleration{}, expected[:i]...), expected[i+1:]...), expected) {
						t.Fatal("removal")
					}
				}
				for _, otherRole := range []string{"candidate", "daemonset", "hostnetwork-daemonset"} {
					other, _ := p.admittedTolerations(otherRole)
					if !reflect.DeepEqual(other, expected) && equalPlacementTolerations(other, expected) {
						t.Fatal("role transfer")
					}
				}
			})
		}
	}
	if _, err := p.admittedTolerations("unknown"); err == nil {
		t.Fatal("unknown role")
	}
}

func TestExecutionPlacementWholeTrustedPod(t *testing.T) {
	config, request, _ := placementFixture(t)
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "workflow-uid"
	wf.Status.StartedAt = metav1.NewTime(request.TrustedKVM.AdmittedAt)
	var leaf *wfv1.Template
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == trustedKVMTemplate {
			leaf = &wf.Spec.Templates[i]
		}
	}
	pod := trustedKVMMaterializedPod(t, wf, leaf)
	pod.ResourceVersion = "1"
	pod.Spec.NodeName = config.ExecutionPlacement.document.NodeName
	pod.Spec.Tolerations, _ = config.ExecutionPlacement.admittedTolerations("harness")
	var node wfv1.NodeStatus
	for _, v := range wf.Status.Nodes {
		if v.TemplateName == trustedKVMTemplate {
			node = v
		}
	}
	if err := verifyTrustedKVMPod(wf, leaf, pod, &node, config); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*corev1.Pod){"selector": func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}
	}, "node": func(p *corev1.Pod) { p.Spec.NodeName = "replaced" }, "resource-version": func(p *corev1.Pod) { p.ResourceVersion = "" }, "owner": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" }, "privileged": func(p *corev1.Pod) { p.Spec.Containers[1].SecurityContext.Privileged = ptr.To(true) }, "executable": func(p *corev1.Pod) { p.Spec.Containers[1].Command = []string{"/bin/true"} }, "helper-resources": func(p *corev1.Pod) { p.Spec.Containers[0].Resources = corev1.ResourceRequirements{} }, "tolerations": func(p *corev1.Pod) { p.Spec.Tolerations = config.ExecutionPlacement.nativeTolerations() }, "scratch": func(p *corev1.Pod) {
		for i := range p.Spec.Volumes {
			if p.Spec.Volumes[i].Name == "tmp-dir-argo" {
				p.Spec.Volumes[i].EmptyDir.SizeLimit = nil
			}
		}
	}}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := pod.DeepCopy()
			mutate(p)
			if err := verifyTrustedKVMPod(wf, leaf, p, &node, config); err == nil {
				t.Fatal("whole-Pod mutation accepted")
			}
		})
	}
	old := leaf.DeepCopy()
	old.NodeSelector = map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}
	if err := verifyTrustedKVMPod(wf, old, pod, &node, config); err == nil {
		t.Fatal("old leaf/new selector accepted")
	}
}

func placementHashRaw(body []byte) string { h := sha256.Sum256(body); return hex.EncodeToString(h[:]) }

func TestExecutionPlacementSeedCreateAndReadback(t *testing.T) {
	config, request, objects := placementFixture(t)
	client := placementClient(objects...)
	var submitted *corev1.Pod
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		if pod.Labels["oberth.ci/component"] == "source-seed" {
			pod.UID = "seed-uid"
			pod.ResourceVersion = "1"
			pod.Spec.Tolerations, _ = config.ExecutionPlacement.admittedTolerations("seed")
			submitted = pod.DeepCopy()
		}
		return false, nil, nil
	})
	seeder := NewSourceSeeder(client, recordingExec(nil), config)
	name := sourceClaimName(request.Name)
	created, err := seeder.createSeedPodBound(t.Context(), name, name, &request)
	if err != nil {
		t.Fatal(err)
	}
	if submitted == nil || !reflect.DeepEqual(submitted.Spec.NodeSelector, config.ExecutionPlacement.selectors()) {
		t.Fatal("direct seeder lost placement")
	}
	readback := created.DeepCopy()
	readback.Spec.NodeName = config.ExecutionPlacement.document.NodeName
	if err := verifyPlacedSeed(created, readback, config.ExecutionPlacement, true); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*corev1.Pod){func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/true"} }, func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "INJECTED", Value: "1"}} }, func(p *corev1.Pod) { p.Spec.Containers[0].Resources = corev1.ResourceRequirements{} }, func(p *corev1.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "foreign" }, func(p *corev1.Pod) { p.Spec.HostNetwork = true }, func(p *corev1.Pod) { p.Spec.Tolerations = p.Spec.Tolerations[:1] }} {
		pod := readback.DeepCopy()
		mutate(pod)
		if err := verifyPlacedSeed(created, pod, config.ExecutionPlacement, true); err == nil {
			t.Fatal("mutated seed accepted")
		}
	}
	if destination := os.Getenv("OBERTH_PLACEMENT_EXPORT"); destination != "" {
		body, _ := json.MarshalIndent(submitted, "", "  ")
		if err := os.WriteFile(filepath.Join(destination, "seed-submitted.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// The fake has an explicit authenticated-transport response. A nil discovery
// transport refuses in production; no optional version-verification bypass.
type placementFakeClient struct {
	*fake.Clientset
	serverVersion string
}
type placementDiscovery struct {
	discovery.DiscoveryInterface
	client rest.Interface
}

func (d placementDiscovery) RESTClient() rest.Interface { return d.client }
func placementClient(objects ...runtime.Object) *placementFakeClient {
	return &placementFakeClient{Clientset: fake.NewClientset(objects...), serverVersion: "v1.35.7-fixture"}
}
func (c *placementFakeClient) Discovery() discovery.DiscoveryInterface {
	client := &fakerest.RESTClient{NegotiatedSerializer: scheme.Codecs.WithoutConversion(), Client: fakerest.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/version" {
			return nil, fmt.Errorf("unexpected fixture discovery path %s", request.URL.Path)
		}
		body, _ := json.Marshal(map[string]string{"gitVersion": c.serverVersion})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	return placementDiscovery{DiscoveryInterface: c.Clientset.Discovery(), client: client}
}
