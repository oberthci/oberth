package installer

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

type ownershipHandoff struct {
	Format               string               `json:"format"`
	Operator             string               `json:"operator"`
	VerifiedAt           time.Time            `json:"verified_at"`
	ClusterUID           string               `json:"cluster_uid"`
	SourceNamespace      string               `json:"source_namespace"`
	SourceRevision       string               `json:"source_revision"`
	SourceManifestSHA256 string               `json:"source_manifest_sha256"`
	Connector            ownershipDestination `json:"connector"`
	Site                 ownershipDestination `json:"site"`
}
type ownershipDestination struct {
	Mode                   string `json:"mode"`
	Reason                 string `json:"reason,omitempty"`
	Owner                  string `json:"owner,omitempty"`
	Namespace              string `json:"namespace,omitempty"`
	Deployment             string `json:"deployment,omitempty"`
	UID                    string `json:"uid,omitempty"`
	Repository             string `json:"repository,omitempty"`
	Revision               string `json:"revision,omitempty"`
	URL                    string `json:"url,omitempty"`
	TrafficVerified        bool   `json:"traffic_verified,omitempty"`
	CredentialsIndependent bool   `json:"credentials_independent,omitempty"`
	EvidenceSHA256         string `json:"evidence_sha256,omitempty"`
}

func ownershipHash(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func ownershipHex(value string, length int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == length && value == strings.ToLower(value)
}
func ownershipError(message string) error {
	return fmt.Errorf("upgrade refused: connector/site ownership handoff: %s; see docs/ownership-handoff.md", message)
}

// The receipt records operator-verified traffic and independent credentials.
// The installer independently checks the selected cluster/release, source
// manifest, external rollout, UID and pod dependencies before any writes.
func checkUpgradeOwnership(ctx context.Context, cfg UpgradeConfig, deps Deps, currentVersion string) error {
	if semver.Compare(canonicalChartVersion(cfg.BinaryVersion), "v0.17.83-0") < 0 || semver.Compare(canonicalChartVersion(currentVersion), "v0.17.83-0") >= 0 {
		return nil
	}
	if cfg.OwnershipHandoff == "" {
		return ownershipError("--ownership-handoff receipt required, including explicit unused declarations")
	}
	file, err := os.OpenFile(cfg.OwnershipHandoff, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ownershipError("cannot open receipt")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return ownershipError("receipt must be a regular file at most 64 KiB")
	}
	body, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(body) > 65536 {
		return ownershipError("receipt unreadable or oversized")
	}
	var receipt ownershipHandoff
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		return ownershipError("receipt format invalid")
	}
	if receipt.Format != "oberth-ownership-handoff-v1" || strings.TrimSpace(receipt.Operator) == "" || receipt.SourceNamespace != cfg.Namespace || !ownershipHex(receipt.SourceManifestSHA256, 32) {
		return ownershipError("receipt identity does not match release")
	}
	rev, err := strconv.ParseUint(receipt.SourceRevision, 10, 64)
	if err != nil || rev == 0 || strconv.FormatUint(rev, 10) != receipt.SourceRevision {
		return ownershipError("source revision must be a positive Helm revision")
	}
	now := time.Now().UTC()
	if receipt.VerifiedAt.Before(now.Add(-24*time.Hour)) || receipt.VerifiedAt.After(now.Add(5*time.Minute)) {
		return ownershipError("repeat handoff verification within 24 hours of upgrade")
	}
	if deps.KubeClient == nil {
		return ownershipError("Kubernetes connection required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cluster, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || receipt.ClusterUID == "" || string(cluster.UID) != receipt.ClusterUID {
		return ownershipError("cluster identity changed or cannot be read")
	}
	release, exists, err := findHelmRelease(ctx, deps, "oberth", cfg.Namespace)
	if err != nil || !exists || helmRevisionLabel(release.Revision) != " revision "+receipt.SourceRevision || chartVersionFromRelease(release.Chart, "oberth") != currentVersion || (release.Status != "deployed" && release.Status != "failed") {
		return ownershipError("source release/revision changed or cannot be read")
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "manifest", "oberth", "-n", cfg.Namespace, "--revision", receipt.SourceRevision})
	if err != nil || len(raw) > 16<<20 || ownershipHash(raw) != receipt.SourceManifestSHA256 {
		return ownershipError("source manifest changed or cannot be verified")
	}
	source, connector, err := ownershipSource(raw, cfg.Namespace)
	if err != nil {
		return err
	}
	if err := checkOwnershipDestination(receipt.Site, "site"); err != nil {
		return err
	}
	if receipt.Site.Mode == "external" {
		if !ownershipHTTPS(receipt.Site.Repository) || !ownershipHTTPS(receipt.Site.URL) || (!ownershipHex(receipt.Site.Revision, 20) && !ownershipHex(receipt.Site.Revision, 32)) {
			return ownershipError("site needs external HTTPS repository, immutable commit and verified URL")
		}
	}
	if err := checkOwnershipDestination(receipt.Connector, "connector"); err != nil {
		return err
	}
	if receipt.Connector.Mode == "unused" {
		if connector {
			return ownershipError("installed manifest still owns a connector Deployment; move ingress before removal")
		}
		return nil
	}
	return checkExternalConnector(ctx, cfg, deps, receipt.Connector, source)
}
func ownershipHTTPS(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func checkOwnershipDestination(d ownershipDestination, kind string) error {
	if d.Mode == "unused" {
		if strings.TrimSpace(d.Reason) == "" {
			return ownershipError(kind + " unused declaration requires operator reason")
		}
		return nil
	}
	if d.Mode != "external" || d.Owner == "" || len(validation.IsValidLabelValue(d.Owner)) != 0 || !d.TrafficVerified || !d.CredentialsIndependent || !ownershipHex(d.EvidenceSHA256, 32) {
		return ownershipError(kind + " external owner, traffic evidence and independent credentials are required")
	}
	return nil
}
func ownershipSource(raw []byte, namespace string) (map[string]bool, bool, error) {
	source := map[string]bool{}
	connector := false
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for count := 0; ; count++ {
		if count >= 5000 {
			return nil, false, ownershipError("source inventory exceeds 5000 objects")
		}
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, ownershipError("source manifest is malformed")
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetAPIVersion() == "" || object.GetKind() == "" || object.GetName() == "" {
			return nil, false, ownershipError("source manifest has an unnamed resource")
		}
		ns := object.GetNamespace()
		if ns == "" && object.GetKind() != "PriorityClass" && object.GetKind() != "Namespace" {
			ns = namespace
		}
		source[object.GetKind()+"/"+ns+"/"+object.GetName()] = true
		if object.GetKind() == "Deployment" && object.GetLabels()["app.kubernetes.io/component"] == "tunnel-connector" {
			connector = true
		}
	}
	return source, connector, nil
}
func checkExternalOwnership(object metav1.Object, kind, owner string, source map[string]bool, cfg UpgradeConfig) error {
	if object.GetDeletionTimestamp() != nil || object.GetLabels()["oberth.ci/external-owner"] != owner || source[kind+"/"+object.GetNamespace()+"/"+object.GetName()] || (object.GetAnnotations()["meta.helm.sh/release-name"] == "oberth" && object.GetAnnotations()["meta.helm.sh/release-namespace"] == cfg.Namespace) {
		return ownershipError(fmt.Sprintf("%s %s/%s is still core-owned, deleting, or lacks external owner %s", kind, object.GetNamespace(), object.GetName(), owner))
	}
	return nil
}
func checkExternalConnector(ctx context.Context, cfg UpgradeConfig, deps Deps, d ownershipDestination, source map[string]bool) error {
	if d.Namespace == cfg.Namespace || len(validation.IsDNS1123Label(d.Namespace)) != 0 || d.Namespace == "" || len(validation.IsDNS1123Subdomain(d.Deployment)) != 0 || d.Deployment == "" || d.UID == "" {
		return ownershipError("connector must use a distinct external namespace and identified Deployment UID")
	}
	namespace, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, d.Namespace, metav1.GetOptions{})
	if err != nil {
		return ownershipError("cannot read external connector namespace")
	}
	if err := checkExternalOwnership(namespace, "Namespace", d.Owner, source, cfg); err != nil {
		return err
	}
	deployment, err := deps.KubeClient.AppsV1().Deployments(d.Namespace).Get(ctx, d.Deployment, metav1.GetOptions{})
	if err != nil {
		return ownershipError("cannot read external connector Deployment")
	}
	if err := checkExternalOwnership(deployment, "Deployment", d.Owner, source, cfg); err != nil {
		return err
	}
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	if string(deployment.UID) != d.UID || deployment.Spec.Paused || desired < 1 || deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.UpdatedReplicas < desired || deployment.Status.ReadyReplicas < desired || deployment.Status.AvailableReplicas < desired {
		return ownershipError("external connector UID or completed rollout is unverified")
	}
	pod := deployment.Spec.Template.Spec
	refs := map[string]bool{}
	add := func(kind, name string) {
		if name != "" {
			refs[kind+"/"+name] = true
		}
	}
	sa := pod.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	account, err := deps.KubeClient.CoreV1().ServiceAccounts(d.Namespace).Get(ctx, sa, metav1.GetOptions{})
	if err != nil {
		return ownershipError("cannot read external ServiceAccount " + sa)
	}
	if err := checkExternalOwnership(account, "ServiceAccount", d.Owner, source, cfg); err != nil {
		return err
	}
	for _, secret := range account.ImagePullSecrets {
		add("Secret", secret.Name)
	}
	for _, ref := range pod.ImagePullSecrets {
		add("Secret", ref.Name)
	}
	for _, volume := range pod.Volumes {
		if volume.ConfigMap != nil {
			add("ConfigMap", volume.ConfigMap.Name)
		}
		if volume.Secret != nil {
			add("Secret", volume.Secret.SecretName)
		}
		if volume.PersistentVolumeClaim != nil {
			add("PersistentVolumeClaim", volume.PersistentVolumeClaim.ClaimName)
		}
		if volume.Projected != nil {
			for _, projection := range volume.Projected.Sources {
				if projection.ConfigMap != nil {
					add("ConfigMap", projection.ConfigMap.Name)
				}
				if projection.Secret != nil {
					add("Secret", projection.Secret.Name)
				}
			}
		}
	}
	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		for _, env := range container.Env {
			if env.ValueFrom != nil {
				if env.ValueFrom.ConfigMapKeyRef != nil {
					add("ConfigMap", env.ValueFrom.ConfigMapKeyRef.Name)
				}
				if env.ValueFrom.SecretKeyRef != nil {
					add("Secret", env.ValueFrom.SecretKeyRef.Name)
				}
			}
		}
		for _, env := range container.EnvFrom {
			if env.ConfigMapRef != nil {
				add("ConfigMap", env.ConfigMapRef.Name)
			}
			if env.SecretRef != nil {
				add("Secret", env.SecretRef.Name)
			}
		}
	}
	add("PriorityClass", pod.PriorityClassName)
	if len(refs) > 256 {
		return ownershipError("connector has too many dependencies")
	}
	for ref := range refs {
		parts := strings.SplitN(ref, "/", 2)
		kind, name := parts[0], parts[1]
		var object metav1.Object
		switch kind {
		case "ConfigMap":
			object, err = deps.KubeClient.CoreV1().ConfigMaps(d.Namespace).Get(ctx, name, metav1.GetOptions{})
		case "Secret":
			object, err = deps.KubeClient.CoreV1().Secrets(d.Namespace).Get(ctx, name, metav1.GetOptions{})
		case "PersistentVolumeClaim":
			object, err = deps.KubeClient.CoreV1().PersistentVolumeClaims(d.Namespace).Get(ctx, name, metav1.GetOptions{})
		case "PriorityClass":
			object, err = deps.KubeClient.SchedulingV1().PriorityClasses().Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return ownershipError("cannot read external dependency " + ref)
		}
		if err := checkExternalOwnership(object, kind, d.Owner, source, cfg); err != nil {
			return err
		}
	}
	return nil
}
