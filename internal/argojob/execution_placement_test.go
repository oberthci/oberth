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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func placementFixture(t *testing.T) (Config, Request, []runtime.Object) {
	t.Helper()
	config := testConfig()
	config.NonrootProfile = argoworkflow.NonrootStaticProfile
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	request.UpstreamName = "codeberg"
	request.RepositoryID = 1
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
		switch fields.Type().Field(i).Name {
		case "ParentRunID", "NormalWorkflow", "PreflightWorkflow", "BranchPolicyBaseline", "GraphDigest", "FragmentLockDigest":
			continue
		}
		if fields.Field(i).Kind() == reflect.String {
			fields.Field(i).SetString("fixture")
		}
	}
	d.Schema = branchPlacementSchema
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
	d.Normal = placementRun{RunID: request.RunID, Workflow: request.Name}
	proof, err := workflowpolicy.ResolveBranchAuthority(t.Context(), absentBranchPolicy{}, d.Repository, request.SHA)
	if err != nil {
		t.Fatal(err)
	}
	request.BranchAuthority = proof
	d.BranchPolicyBaseline, d.PolicyDigest = proof.Identity()
	resolved, err := argoworkflow.ResolveRunSource(request.Source, request.Fragments, argoworkflow.RunSourceBinding{RunID: request.RunID, RepositoryID: request.RepositoryID, SHA: request.SHA, File: request.PipelineFile, Trigger: request.effectiveRunTrigger()})
	if err != nil {
		t.Fatal(err)
	}
	d.GraphDigest, d.FragmentLockDigest = resolved.GraphIdentity(), placementHash(resolved.Lock())
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.AdmissionConfigMap, Namespace: d.Namespace, UID: types.UID(d.AdmissionUID), ResourceVersion: "1"}, Immutable: ptr.To(true), Data: map[string]string{"cluster_id": d.ClusterID, "cluster_created": d.ClusterCreated, "control_plane_version": d.ControlPlaneVersion, "node_version": d.NodeVersion, "admission_configuration": d.AdmissionConfiguration, "profile": d.AdmissionProfile, "intent_digest": d.IntentDigest, "policy_digest": d.PolicyDigest}}
	d.AdmissionDigest = placementHash(cm.Data)
	body, _ := json.Marshal(&ExecutionPlacement{document: d})
	p, err := ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutionPlacement = p
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: d.ReplicaSet, Namespace: d.Namespace, UID: types.UID(d.ReplicaSetUID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Deployment, UID: types.UID(d.DeploymentUID), Controller: ptr.To(true)}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Deployment, Namespace: d.Namespace, UID: types.UID(d.DeploymentUID)}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	return config, request, []runtime.Object{pod, controllerCM, rs, deployment, cm, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(d.ClusterAnchorUID)}}}
}

func TestExecutionPlacementSelectionRefusesBeforeSeed(t *testing.T) {
	config, request, objects := placementFixture(t)
	mutations := map[string]func(*Request){"source-volume": func(r *Request) { r.SourceVolume.ClaimName = "foreign" },
		"run": func(r *Request) { r.RunID = "other" }, "workflow": func(r *Request) { r.Name = "other" }, "repository": func(r *Request) { r.Repo = "other" }, "repo-id": func(r *Request) { r.RepositoryID++ }, "ref": func(r *Request) { r.Ref = "refs/heads/other" }, "source": func(r *Request) { r.Source = append(append([]byte{}, r.Source...), 10) }, "sha": func(r *Request) { r.SHA = strings.Repeat("b", 40) }, "pipeline": func(r *Request) { r.PipelineFile = ".oberth/other.yaml" }, "trigger": func(r *Request) { r.Trigger = pipeline.TriggerRelease }}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := request
			mutate(&r)
			if _, err := Build(config, r); err == nil {
				t.Fatal("Build accepted changed binding")
			}
			client := placementClient(objects...)
			seeder := NewSourceSeeder(client, recordingExec(nil), config)
			if _, err := seeder.SeedRequest(t.Context(), r, false); err == nil {
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
			body, _ := json.Marshal(&ExecutionPlacement{document: d})
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
	if _, err := seeder.createSeedPodBound(t.Context(), sourceClaimName(request.Name), sourceClaimName(request.Name), &request); err == nil {
		t.Fatal("fake API without UID/RV claimed authenticated seed")
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
			pod.Spec.NodeSelector = map[string]string{"operator.example/node-pool": "dedicated"}
			pod.Spec.Tolerations = []corev1.Toleration{{Key: "operator.example/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
			pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "operator.example/node-pool", Operator: corev1.NodeSelectorOpIn, Values: []string{"dedicated"}}}}}}}}
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
	if submitted == nil || submitted.Spec.NodeSelector["operator.example/node-pool"] != "dedicated" {
		t.Fatal("admission-added seed placement was lost")
	}
	readback := created.DeepCopy()
	readback.Spec.NodeName = "any-scheduled-node"
	if err := verifyPlacedSeed(created, readback, config.ExecutionPlacement); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*corev1.Pod){func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}} }, func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"/bin/true"} }, func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "INJECTED", Value: "1"}} }, func(p *corev1.Pod) { p.Spec.Containers[0].Resources = corev1.ResourceRequirements{} }, func(p *corev1.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "foreign" }, func(p *corev1.Pod) { p.Spec.HostNetwork = true }, func(p *corev1.Pod) { p.Spec.ServiceAccountName = "foreign" }} {
		pod := readback.DeepCopy()
		mutate(pod)
		if err := verifyPlacedSeed(created, pod, config.ExecutionPlacement); err == nil {
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
