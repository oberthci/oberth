package argojob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// ExecutionPlacement is an immutable administrator snapshot. Its unexported
// document prevents request callers from constructing or editing authority.
// ParseExecutionPlacement is for trusted deployment configuration, never YAML,
// request bodies, Pod annotations or repository files.
type ExecutionPlacement struct {
	observationOnly bool
	document        placementDocument
	digest          string
}

type placementRun struct {
	RunID         string `json:"run_id"`
	Workflow      string `json:"workflow"`
	TrustedIntent string `json:"trusted_intent"`
}
type placementDocument struct {
	// Compatibility slots preserve historical branch-v2 identity hashes.
	ParentRunID            string              `json:"parent_run_id,omitempty"`
	NormalWorkflow         string              `json:"normal_workflow,omitempty"`
	PreflightWorkflow      string              `json:"preflight_workflow,omitempty"`
	BranchPolicyBaseline   string              `json:"branch_policy_baseline,omitempty"`
	GraphDigest            string              `json:"graph_digest,omitempty"`
	FragmentLockDigest     string              `json:"fragment_lock_digest,omitempty"`
	Fragments              []placementFragment `json:"fragments,omitempty"`
	Schema                 string              `json:"schema"`
	Repository             string              `json:"repository"`
	RepositoryID           int64               `json:"repository_id"`
	Ref                    string              `json:"ref"`
	SHA                    string              `json:"sha"`
	SourceDigest           string              `json:"source_digest"`
	PipelineFile           string              `json:"pipeline_file"`
	Namespace              string              `json:"namespace"`
	ServiceAccount         string              `json:"service_account"`
	IntentDigest           string              `json:"intent_digest"`
	PolicyDigest           string              `json:"policy_digest"`
	ClusterID              string              `json:"cluster_id"`
	ClusterCreated         string              `json:"cluster_created"`
	ClusterAnchorUID       string              `json:"cluster_anchor_uid"`
	EndpointIdentity       string              `json:"endpoint_identity"`
	NativeIdentity         string              `json:"native_identity"`
	DependencyIdentity     string              `json:"dependency_identity"`
	ControllerPod          string              `json:"controller_pod"`
	ControllerUID          string              `json:"controller_uid"`
	ControllerConfigUID    string              `json:"controller_config_uid"`
	ReplicaSet             string              `json:"replica_set"`
	ReplicaSetUID          string              `json:"replica_set_uid"`
	Deployment             string              `json:"deployment"`
	DeploymentUID          string              `json:"deployment_uid"`
	AdmissionConfigMap     string              `json:"admission_config_map"`
	AdmissionUID           string              `json:"admission_uid"`
	AdmissionDigest        string              `json:"admission_digest"`
	AdmissionProfile       string              `json:"admission_profile"`
	ControlPlaneVersion    string              `json:"control_plane_version"`
	NodeVersion            string              `json:"node_version"`
	AdmissionConfiguration string              `json:"admission_configuration"`
	Expires                time.Time           `json:"expires"`
	Normal                 placementRun        `json:"normal"`
	Preflight              placementRun        `json:"preflight"`
}

type placementFragment struct {
	Key    string `json:"key"`
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

const branchPlacementSchema = "oberth.execution-placement/branch-v2"

func (p *ExecutionPlacement) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("null"), nil
	}
	body, err := json.Marshal(p.document)
	if err != nil || (p.document.Schema != branchPlacementSchema) {
		return body, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	delete(fields, "preflight")
	var normal map[string]json.RawMessage
	if err := json.Unmarshal(fields["normal"], &normal); err != nil {
		return nil, err
	}
	delete(normal, "trusted_intent")
	fields["normal"], err = json.Marshal(normal)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (p *ExecutionPlacement) UnmarshalJSON(body []byte) error {
	// Decoding reconstructs immutable custody, including expired bindings. It
	// does not admit work; selection and every new target POST check expiry.
	parsed, err := decodeExecutionPlacement(body)
	if err == nil {
		*p = *parsed
	}
	return err
}

// Identity is public operator configuration, never a transport credential.
func (p *ExecutionPlacement) Identity() (repository string, repositoryID int64, ref, sha, normalRun, workflow, digest string) {
	if p == nil {
		return
	}
	d := p.document
	return d.Repository, d.RepositoryID, d.Ref, d.SHA, d.Normal.RunID, d.Normal.Workflow, p.digest
}

func (p *ExecutionPlacement) BranchOnly() bool {
	return p != nil && p.document.Schema == branchPlacementSchema
}

func (p *ExecutionPlacement) ValidateRequest(config Config, request Request) error {
	return p.selectRequest(config, request)
}

// ParseExecutionPlacement rejects duplicate/unknown fields before typed decoding.
// There is deliberately no profile inferred from observed Pods and no default
// cluster/version/owner identity. The entire record is public configuration.
func ParseExecutionPlacement(body []byte) (*ExecutionPlacement, error) {
	placement, err := decodeExecutionPlacement(body)
	if err != nil {
		return nil, err
	}
	if err := placement.ValidateAdmission(); err != nil {
		return nil, err
	}
	return placement, nil
}

func decodeExecutionPlacement(body []byte) (*ExecutionPlacement, error) {
	if len(body) == 0 || len(body) > 65536 {
		return nil, errors.New("argojob: placement configuration must be 1..65536 bytes")
	}
	if err := checkPlacementJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return nil, err
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawFields); err != nil {
		return nil, err
	}
	for _, field := range []string{"node_name", "node_uid", "provider_id", "zone", "pool", "taint_key", "taint_value"} {
		if _, exists := rawFields[field]; exists {
			return nil, fmt.Errorf("argojob: execution-target placement field %q was removed; put nodeSelector and tolerations in repository Workflow YAML and enforce node isolation with operator policy", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var document placementDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("argojob: placement requires one JSON object")
	}
	if document.Schema == branchPlacementSchema {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		for key, raw := range fields {
			if strings.EqualFold(key, "preflight") {
				return nil, errors.New("argojob: branch-only placement forbids a preflight field")
			}
			if strings.EqualFold(key, "normal") {
				var normal map[string]json.RawMessage
				if json.Unmarshal(raw, &normal) != nil {
					return nil, errors.New("argojob: branch-only normal binding must be an object")
				}
				for field := range normal {
					if strings.EqualFold(field, "trusted_intent") {
						return nil, errors.New("argojob: branch-only placement forbids a trusted intent field")
					}
				}
			}
		}
	}
	placement := &ExecutionPlacement{document: document, digest: placementHash(document)}
	if err := placement.validate(); err != nil {
		return nil, err
	}
	return placement, nil
}

func checkPlacementJSON(d *json.Decoder, depth int) error {
	if depth > 5 {
		return errors.New("argojob: placement JSON too deep")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, object := token.(json.Delim)
	if !object {
		return nil
	}
	if delimiter == '[' {
		count := 0
		for d.More() {
			count++
			if count > argoworkflow.MaxFragments {
				return errors.New("argojob: placement inventory too large")
			}
			if err := checkPlacementJSON(d, depth+1); err != nil {
				return err
			}
		}
		_, err := d.Token()
		return err
	}
	if delimiter != '{' {
		return errors.New("argojob: placement requires objects and scalar fields")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || key != strings.ToLower(key) || seen[key] {
			return errors.New("argojob: duplicate or noncanonical placement field")
		}
		seen[key] = true
		if err := checkPlacementJSON(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
func placementHash(value any) string {
	body, _ := json.Marshal(value)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
func placementSHA(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

// validate authenticates the immutable record, independently of admission time.
func (p *ExecutionPlacement) validate() error {
	if p == nil {
		return nil
	}
	d := p.document
	branch := d.Schema == branchPlacementSchema
	if !branch || d.Preflight != (placementRun{}) || d.Normal.TrustedIntent != "" || d.ParentRunID != "" || d.NormalWorkflow != "" || d.PreflightWorkflow != "" || d.PipelineFile != ".oberth/build.yaml" || len(strings.Split(d.Repository, "/")) != 3 || d.RepositoryID <= 0 || len(d.SHA) != 40 || strings.ContainsAny(d.Repository+d.Ref, "*?[]\\\n\r\t ") || p.digest != placementHash(d) {
		return errors.New("argojob: incomplete or unsupported closed placement selection")
	}
	if branch {
		if !argoworkflow.IsFullFragmentSHA(d.BranchPolicyBaseline) || !placementSHA(d.GraphDigest) || !placementSHA(d.FragmentLockDigest) || len(d.Fragments) > argoworkflow.MaxFragments {
			return errors.New("argojob: incomplete fragment-bound branch authority")
		}
	}
	if branch {
		if !placementSHA(d.GraphDigest) || !placementSHA(d.FragmentLockDigest) || len(d.Fragments) > argoworkflow.MaxFragments {
			return errors.New("argojob: incomplete fragment graph")
		}
		seen := map[string]bool{}
		for _, fragment := range d.Fragments {
			key, err := argoworkflow.ParseFragmentRef(fragment.Key)
			if err != nil || key.String() != fragment.Key || seen[fragment.Key] || !argoworkflow.IsFullFragmentSHA(fragment.Commit) || !placementSHA(fragment.Digest) {
				return errors.New("argojob: invalid or repeated placement fragment")
			}
			seen[fragment.Key] = true
		}
	}

	// Every scalar identity is mandatory.
	fields := reflect.ValueOf(d)
	kind := fields.Type()
	for i := 0; i < fields.NumField(); i++ {
		if kind.Field(i).Name == "ParentRunID" || kind.Field(i).Name == "NormalWorkflow" || kind.Field(i).Name == "PreflightWorkflow" {
			continue
		}
		if fields.Field(i).Kind() == reflect.String && strings.TrimSpace(fields.Field(i).String()) == "" {
			return fmt.Errorf("argojob: missing placement %s", kind.Field(i).Tag.Get("json"))
		}
	}
	runs := []placementRun{d.Normal}
	for _, run := range runs {
		if run.RunID == "" || run.Workflow == "" {
			return errors.New("argojob: placement requires a complete run binding")
		}
	}
	for _, digest := range []string{d.SourceDigest, d.IntentDigest, d.PolicyDigest, d.NativeIdentity, d.DependencyIdentity, d.AdmissionDigest, d.AdmissionConfiguration, d.EndpointIdentity} {
		if !placementSHA(digest) {
			return errors.New("argojob: invalid placement identity digest")
		}
	}
	if _, err := time.Parse(time.RFC3339, d.ClusterCreated); err != nil || d.Expires.IsZero() {
		return errors.New("argojob: invalid cluster binding lifetime")
	}
	if d.AdmissionProfile != "kube1357-dts300" && d.AdmissionProfile != "kube1357-dts300-ptr-empty" {
		return errors.New("argojob: unknown admission profile")
	}
	if len(k8svalidation.IsDNS1123Label(d.Namespace)) != 0 || len(k8svalidation.IsDNS1123Subdomain(d.ServiceAccount)) != 0 {
		return errors.New("argojob: invalid target namespace or service account")
	}
	if _, err := hex.DecodeString(d.SHA); err != nil {
		return errors.New("argojob: invalid source SHA")
	}
	return nil
}

// ValidateAdmission checks the immutable record and its current lifetime.
// Retained observation and disposal use structural validation independently.
func (p *ExecutionPlacement) ValidateAdmission() error {
	if err := p.validate(); err != nil {
		return err
	}
	if p != nil && !time.Now().Before(p.document.Expires) {
		return errors.New("argojob: expired cluster binding")
	}
	return nil
}

func (p *ExecutionPlacement) selectRequest(config Config, r Request) error {
	if p == nil {
		return nil
	}
	if p.observationOnly {
		if err := p.validate(); err != nil {
			return err
		}
	} else if err := p.ValidateAdmission(); err != nil {
		return err
	}
	d := p.document
	if d.DependencyIdentity != placementDependencyIdentity(config) || config.NonrootProfile != argoworkflow.NonrootStaticProfile || config.GoProxyURL == "" || config.GoProxyModulePrefix == "" || config.GoProxyCACertPEM == "" || config.SourceStorageClass == "" || config.executorScratchLimit().Cmp(resource.MustParse("1Gi")) != 0 {
		return errors.New("argojob: isolated executor configuration differs or lacks dependency closure")
	}
	selected := d.Normal
	if !r.BranchAuthority.Matches(d.Repository, r.SHA, d.BranchPolicyBaseline, d.PolicyDigest) {
		return errors.New("argojob: branch-only execution lacks canonical native policy authority")
	}
	if err := p.selectPlacementFragments(r); err != nil {
		return err
	}
	digest := sha256.Sum256(r.Source)
	if config.Namespace != d.Namespace || config.PipelineServiceAccount != d.ServiceAccount || canonicalRepoKey(r.UpstreamName, r.UpstreamOrg, r.Repo) != d.Repository || r.RepositoryID != d.RepositoryID || r.SHA != d.SHA || r.Ref != d.Ref || r.PipelineFile != d.PipelineFile || r.RunID != selected.RunID || r.Name != selected.Workflow || r.Trigger != pipeline.TriggerCI || r.effectiveRunTrigger() != argoworkflow.RunTriggerBranch || hex.EncodeToString(digest[:]) != d.SourceDigest || r.releasePreflight {
		return errors.New("argojob: request differs from isolated placement authority")
	}
	return nil
}
func applyExecutionPlacement(wf *wfv1.Workflow, config Config, request Request) error {
	p := config.ExecutionPlacement
	if p == nil {
		return nil
	}
	if err := p.selectRequest(config, request); err != nil {
		return err
	}
	var result error
	walkTemplates(wf, func(t *wfv1.Template) {
		if result != nil {
			return
		}
		if t.Container == nil && t.Script == nil {
			if t.Resource != nil || t.Data != nil || t.ContainerSet != nil {
				result = errors.New("argojob: unsupported isolated leaf")
			}
			return
		}
		patch := map[string]any{}
		if t.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(t.PodSpecPatch), &patch); err != nil {
				result = err
				return
			}
		}
		// Only server-created patches reach here; source patches were refused at admission.
		volumes, _ := patch["volumes"].([]any)
		for _, name := range []string{"var-run-argo", "tmp-dir-argo"} {
			cap := "1Gi"
			if name == "tmp-dir-argo" {
				cap = "64Mi"
			}
			found := false
			for _, v := range volumes {
				if m, ok := v.(map[string]any); ok && m["name"] == name {
					m["emptyDir"] = map[string]any{"sizeLimit": cap}
					found = true
				}
			}
			if !found {
				volumes = append(volumes, map[string]any{"name": name, "emptyDir": map[string]any{"sizeLimit": cap}})
			}
		}
		patch["volumes"] = volumes
		for _, key := range []string{"containers", "initContainers"} {
			items, _ := patch[key].([]any)
			for _, item := range items {
				if m, ok := item.(map[string]any); ok && (m["name"] == "wait" || m["name"] == "init" || m["name"] == executorTokenWaiter) {
					m["resources"] = helperResources(wf, t, !request.legacyHelperStorage)
				}
			}
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			result = err
			return
		}
		t.PodSpecPatch = string(encoded)
	})
	if result != nil {
		return result
	}
	wf.Annotations["oberth.ci/execution-placement"] = p.digest
	return nil
}

// verifyEnvironment uses the separately authenticated Kubernetes client. The
// immutable admission record must be provisioned by the operator from actual
// control-plane configuration evidence, independently of workload objects.
func (p *ExecutionPlacement) verifyEnvironment(ctx context.Context, client kubernetes.Interface, config Config) error {
	if p == nil {
		return nil
	}
	if p.observationOnly {
		if err := p.validate(); err != nil {
			return err
		}
	} else if err := p.ValidateAdmission(); err != nil {
		return err
	}
	if p.document.NativeIdentity != executionBinaryIdentity().digest || executionBinaryIdentity().err != nil || p.document.DependencyIdentity != placementDependencyIdentity(config) {
		return errors.New("argojob: native binary or dependency/config identity differs")
	}
	if client == nil {
		return errors.New("argojob: placement requires authenticated Kubernetes")
	}
	d := p.document
	discoveryClient := client.Discovery().RESTClient()
	if discoveryClient == nil {
		return errors.New("argojob: authenticated control-plane version is unavailable")
	}
	body, err := discoveryClient.Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return err
	}
	var server version.Info
	if json.Unmarshal(body, &server) != nil || server.GitVersion != d.ControlPlaneVersion {
		return errors.New("argojob: control-plane build differs from admission authority")
	}
	anchor, err := client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(anchor.UID) != d.ClusterAnchorUID || anchor.DeletionTimestamp != nil {
		return errors.New("argojob: cluster anchor replaced")
	}
	proof, err := verifyNonrootController(ctx, client, config.Namespace, config.NonrootProfileConfigMap, Request{})
	if err != nil {
		return err
	}
	if proof.podUID != d.ControllerUID || proof.configUID != d.ControllerConfigUID {
		return errors.New("argojob: selected controller differs from authority")
	}
	controller, err := client.CoreV1().Pods(config.Namespace).Get(ctx, d.ControllerPod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(controller.UID) != d.ControllerUID || !placementOwner(controller.OwnerReferences, "ReplicaSet", d.ReplicaSet, d.ReplicaSetUID) {
		return errors.New("argojob: controller provenance differs")
	}
	rs, err := client.AppsV1().ReplicaSets(config.Namespace).Get(ctx, d.ReplicaSet, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(rs.UID) != d.ReplicaSetUID || rs.DeletionTimestamp != nil || !placementOwner(rs.OwnerReferences, "Deployment", d.Deployment, d.DeploymentUID) {
		return errors.New("argojob: controller ReplicaSet provenance differs")
	}
	deployment, err := client.AppsV1().Deployments(config.Namespace).Get(ctx, d.Deployment, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(deployment.UID) != d.DeploymentUID || deployment.DeletionTimestamp != nil || deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.ReadyReplicas != 1 || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		return errors.New("argojob: controller Deployment provenance differs")
	}
	cm, err := client.CoreV1().ConfigMaps(config.Namespace).Get(ctx, d.AdmissionConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}
	expected := map[string]string{"cluster_id": d.ClusterID, "cluster_created": d.ClusterCreated, "control_plane_version": d.ControlPlaneVersion, "node_version": d.NodeVersion, "admission_configuration": d.AdmissionConfiguration, "profile": d.AdmissionProfile, "intent_digest": d.IntentDigest, "policy_digest": d.PolicyDigest}
	if string(cm.UID) != d.AdmissionUID || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Immutable == nil || !*cm.Immutable || len(cm.BinaryData) != 0 || !reflect.DeepEqual(cm.Data, expected) || placementHash(cm.Data) != d.AdmissionDigest {
		return errors.New("argojob: authenticated admission profile differs")
	}
	return nil
}
func placementOwner(owners []metav1.OwnerReference, kind, name, uid string) bool {
	return len(owners) == 1 && owners[0].Kind == kind && owners[0].Name == name && string(owners[0].UID) == uid && owners[0].Controller != nil && *owners[0].Controller
}

// Verify identity without constraining Kubernetes placement. Retained Workflow
// intents carry their original authority digest even after configuration removal.
func (p *ExecutionPlacement) verifyWorkflowIdentity(ctx context.Context, config Config, wf *wfv1.Workflow) error {
	expected := p.digest
	if config.Custody != nil {
		resources, err := config.Custody.Resources(ctx)
		if err != nil {
			return err
		}
		expected = ""
		for _, r := range resources {
			if r.Kind != "Workflow" || r.Namespace != wf.Namespace || r.Name != wf.Name || r.UID != string(wf.UID) {
				continue
			}
			var original wfv1.Workflow
			if json.Unmarshal(r.Object, &original) != nil {
				return errors.New("argojob: invalid original Workflow identity")
			}
			expected = original.Annotations["oberth.ci/execution-placement"]
		}
	}
	if !placementSHA(expected) || wf.Annotations["oberth.ci/execution-placement"] != expected {
		return errors.New("argojob: Workflow execution authority differs")
	}
	return nil
}
func (p *ExecutionPlacement) verifyPodIdentity(pod *corev1.Pod) error {
	if err := p.validate(); err != nil {
		return err
	}
	if pod.Namespace != p.document.Namespace || pod.UID == "" || pod.ResourceVersion == "" || pod.Spec.ServiceAccountName != p.document.ServiceAccount {
		return errors.New("argojob: Pod namespace or service identity differs from approved target")
	}
	return nil
}

type binaryIdentity struct {
	digest string
	err    error
}

var executionBinaryIdentity = sync.OnceValue(func() binaryIdentity {
	name, err := os.Executable()
	if err != nil {
		return binaryIdentity{err: err}
	}
	// #nosec G304 -- os.Executable returns this process's own executable, never a request path.
	file, err := os.Open(name)
	if err != nil {
		return binaryIdentity{err: err}
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return binaryIdentity{err: err}
	}
	return binaryIdentity{digest: hex.EncodeToString(hash.Sum(nil))}
})

func placementDependencyIdentity(config Config) string {
	config.ExecutionPlacement = nil
	return placementHash(config)
}

// VerifyTarget binds the authenticated transport before constructing the engine.
// No insecure TLS, impersonation or alternative endpoint can reuse a binding.
func (p *ExecutionPlacement) VerifyTarget(config *rest.Config) error {
	if p == nil {
		return nil
	}
	if config == nil {
		return errors.New("argojob: missing authenticated transport")
	}
	caData := config.CAData
	if len(caData) == 0 && config.CAFile != "" {
		// The authenticated client configuration selects this public CA file.
		// #nosec G304 G703 -- administrator transport CA path, never repository input.
		file, err := os.Open(config.CAFile)
		if err != nil {
			return err
		}
		caData, err = io.ReadAll(io.LimitReader(file, 65537))
		_ = file.Close()
		if err != nil {
			return err
		}
		if len(caData) > 65536 {
			return errors.New("argojob: transport CA exceeds 64 KiB")
		}
	}
	target, err := url.Parse(config.Host)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil || config.Insecure || len(caData) == 0 || config.Impersonate.UserName != "" || config.Impersonate.UID != "" || len(config.Impersonate.Groups) != 0 || len(config.Impersonate.Extra) != 0 || placementHash([]any{config.Host, config.ServerName, caData}) != p.document.EndpointIdentity {
		return errors.New("argojob: authenticated endpoint/CA differs from placement authority")
	}
	return nil
}

func verifyPlacedSeed(expected, actual *corev1.Pod, p *ExecutionPlacement) error {
	if actual == nil || actual.UID == "" || actual.ResourceVersion == "" || actual.Name != expected.Name || actual.Namespace != expected.Namespace || !reflect.DeepEqual(actual.OwnerReferences, expected.OwnerReferences) || actual.DeletionTimestamp != nil || !reflect.DeepEqual(actual.Labels, expected.Labels) || len(actual.Annotations) != 0 {
		return errors.New("argojob: seed API identity differs")
	}
	want := expected.DeepCopy()
	got := actual.DeepCopy()
	if err := p.validate(); err != nil {
		return err
	}
	normalizeSeedDefaults(&want.Spec)
	normalizeSeedDefaults(&got.Spec)
	clearSeedPlacement(&want.Spec)
	clearSeedPlacement(&got.Spec)
	if !apiequality.Semantic.DeepEqual(want.Spec, got.Spec) {
		return errors.New("argojob: seed executable, resources, storage or isolation differs")
	}
	return nil
}

// Cluster admission owns seed placement. These fields confer no API, file,
// executable or ServiceAccount authority; all other fields remain compared.
func clearSeedPlacement(spec *corev1.PodSpec) {
	spec.NodeName = ""
	spec.NodeSelector = nil
	spec.Affinity = nil
	spec.Tolerations = nil
	spec.TopologySpreadConstraints = nil
	spec.SchedulingGates = nil
	spec.SchedulerName = ""
	spec.PriorityClassName = ""
	spec.Priority = nil
	spec.PreemptionPolicy = nil
}

func normalizeSeedDefaults(spec *corev1.PodSpec) {
	if spec.DNSPolicy == "" {
		spec.DNSPolicy = corev1.DNSClusterFirst
	}
	if spec.SchedulerName == "" {
		spec.SchedulerName = corev1.DefaultSchedulerName
	}
	if spec.TerminationGracePeriodSeconds == nil {
		spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if spec.EnableServiceLinks == nil {
		spec.EnableServiceLinks = ptr.To(true)
	}
	if spec.PreemptionPolicy == nil {
		spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
	}
	if spec.Priority != nil && *spec.Priority == 0 {
		spec.Priority = nil
	}
	if spec.DeprecatedServiceAccount == spec.ServiceAccountName {
		spec.DeprecatedServiceAccount = ""
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.ImagePullPolicy == "" {
			c.ImagePullPolicy = corev1.PullIfNotPresent
		}
		if c.TerminationMessagePath == "" {
			c.TerminationMessagePath = corev1.TerminationMessagePathDefault
		}
		if c.TerminationMessagePolicy == "" {
			c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
	}
}
