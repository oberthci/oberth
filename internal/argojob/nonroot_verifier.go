package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const nonrootProfileAnnotation = "oberth.ci/verified-controller-profile"

type nonrootProof struct {
	requestID, podUID, configUID, configVersion string
	namespace                                   string
	verifiedAt                                  time.Time
}

func nonrootRequestIdentity(request Request) string {
	fragments := make(map[string][]string, len(request.Fragments))
	for key, fragment := range request.Fragments {
		fragments[key.String()] = []string{key.Repo, key.Version, fragment.SHA, fragment.Digest, string(fragment.Source)}
	}
	// Explicit primitive fields avoid JSON's unsupported struct-keyed Fragment
	// map and include actual fragment bytes (Fragment.Source normally has json:-).
	identity := struct {
		Principal       []string
		Source          []byte
		ApprovedSecrets map[string]bool
		Fragments       map[string][]string
	}{[]string{request.RunID, request.Name, request.Repo, request.UpstreamName, request.UpstreamOrg, request.Ref, request.SHA, string(request.Trigger), request.SourceDir}, request.Source, request.ApprovedSecrets, fragments}
	encoded, _ := json.Marshal(identity) // Only primitive serializable types.
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (proof *nonrootProof) binding() string {
	encoded, _ := json.Marshal([]string{argoworkflow.NonrootStaticProfile, argoworkflow.NonrootProfileConfigSHA256, proof.namespace, proof.requestID, proof.podUID, proof.configUID, proof.configVersion})
	digest := sha256.Sum256(encoded)
	return argoworkflow.NonrootStaticProfile + ":" + hex.EncodeToString(digest[:])
}

func requestSelectsNonroot(request Request) (bool, error) {
	workflow, err := argoworkflow.Decode(request.Source)
	if err != nil {
		return false, err
	}
	names, err := argoworkflow.DeclaredNonrootTemplates(workflow)
	return len(names) != 0, err
}

// PrepareNonroot establishes the request's private capability before the app's
// existing audit Build. Create rechecks it before seed/create/adoption; nothing
// in repository YAML or a public boolean can manufacture this proof.
func (controller *Controller) PrepareNonroot(ctx context.Context, request Request) (Request, error) {
	selected, err := requestSelectsNonroot(request)
	if err != nil || !selected {
		return request, err
	}
	if controller.config.NonrootProfile != argoworkflow.NonrootStaticProfile || controller.kube == nil {
		return Request{}, errors.New("argojob: nonroot controller support has not been established")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	proof, err := verifyNonrootController(ctx, controller.kube, controller.config.Namespace, request)
	if err != nil {
		return Request{}, err
	}
	if request.nonrootProof != nil && request.nonrootProof.binding() != proof.binding() {
		return Request{}, errors.New("argojob: nonroot controller identity changed after the submission binding was prepared")
	}
	request.nonrootProof = proof
	return request, nil
}

func (controller *Controller) refreshNonroot(ctx context.Context, request Request) (Request, error) {
	selected, err := requestSelectsNonroot(request)
	if err != nil || !selected {
		return request, err
	}
	if request.nonrootProof == nil {
		return Request{}, errors.New("argojob: selected nonroot submission was not prepared before audit")
	}
	return controller.PrepareNonroot(ctx, request)
}

func (controller *Controller) nonrootExpectedSource(request Request) SourceVolume {
	volume := SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
	if request.Trigger == periapsis.TriggerRelease {
		volume.BinarySubPath = binarySubPath
		if controller.config.VaultCACertPEM != "" {
			volume.VaultCASubPath = vaultCASubPath
		}
	}
	return volume
}

func verifyNonrootController(ctx context.Context, client kubernetes.Interface, namespace string, request Request) (*nonrootProof, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=argo-workflows-workflow-controller"})
	if err != nil {
		return nil, errors.New("argojob: cannot verify nonroot controller Pod identity")
	}
	if len(pods.Items) != 1 {
		return nil, errors.New("argojob: nonroot profile requires one unambiguous controller Pod")
	}
	pod := &pods.Items[0]
	if pod.Namespace != namespace {
		return nil, errors.New("argojob: controller Pod namespace differs from the verified profile")
	}
	if err := verifyNonrootControllerPod(pod); err != nil {
		return nil, err
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, argoworkflow.NonrootProfileConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, errors.New("argojob: cannot read the exact nonroot controller profile ConfigMap")
	}
	if cm.Name != argoworkflow.NonrootProfileConfigMap || cm.Namespace != namespace || cm.UID == "" || cm.ResourceVersion == "" || cm.Immutable == nil || !*cm.Immutable || cm.DeletionTimestamp != nil ||
		len(cm.Data) != 1 || cm.Data["config"] != argoworkflow.NonrootProfileConfig || len(cm.BinaryData) != 0 {
		return nil, errors.New("argojob: nonroot controller configuration is not the exact immutable built-in profile")
	}
	// The exact required public-config mount is a causal startup dependency.
	// Pinned Argo synchronously GETs/parses this same config before Run and
	// exits on failure. Equality at API/kubelet second precision is therefore
	// supported only after that mount/profile proof. Later creation is still
	// refused. This relies on trusted installation and no immutable-name reuse;
	// it does not freeze Kubernetes against a malicious administrator.
	started := pod.Status.ContainerStatuses[0].State.Running.StartedAt.Truncate(time.Second)
	created := cm.CreationTimestamp.Truncate(time.Second)
	if cm.CreationTimestamp.IsZero() || created.After(started) {
		return nil, errors.New("argojob: controller configuration was created after the running process")
	}
	return &nonrootProof{requestID: nonrootRequestIdentity(request), namespace: namespace, podUID: string(pod.UID), configUID: string(cm.UID), configVersion: cm.ResourceVersion, verifiedAt: time.Now()}, nil
}

func verifyNonrootControllerPod(pod *corev1.Pod) error {
	invalid := func(reason string) error {
		return fmt.Errorf("argojob: unsupported nonroot controller profile: %s", reason)
	}
	if pod.UID == "" || pod.DeletionTimestamp != nil || len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return invalid("Pod shape or identity")
	}
	owned := false
	for _, owner := range pod.OwnerReferences {
		owned = owned || (owner.Controller != nil && *owner.Controller && owner.Kind == "ReplicaSet" && owner.UID != "")
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		ready = ready || (condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue)
	}
	if !owned || !ready || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Running == nil {
		return invalid("controller is not an owned running Ready process")
	}
	container := &pod.Spec.Containers[0]
	status := &pod.Status.ContainerStatuses[0]
	indexID := strings.Replace(argoworkflow.NonrootControllerImage, ":v4.0.8@", "@", 1)
	if container.Name != "controller" || status.Name != container.Name || !status.Ready || status.State.Running.StartedAt.IsZero() ||
		container.Image != argoworkflow.NonrootControllerImage || (status.ImageID != indexID && status.ImageID != argoworkflow.NonrootControllerAMD64) {
		return invalid("controller image or running image identity")
	}
	wantArgs := []string{"--configmap", argoworkflow.NonrootProfileConfigMap, "--executor-image", argoworkflow.NonrootExecutorImage, "--loglevel", "info", "--gloglevel", "0", "--log-format", "text", "--namespaced"}
	if !reflect.DeepEqual(container.Command, []string{"workflow-controller"}) || !reflect.DeepEqual(container.Args, wantArgs) || len(container.EnvFrom) != 0 {
		return invalid("command, watch scope or executor image")
	}
	wanted := map[string]corev1.EnvVar{
		"ARGO_NAMESPACE":           {Name: "ARGO_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
		"LEADER_ELECTION_IDENTITY": {Name: "LEADER_ELECTION_IDENTITY", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
		"LEADER_ELECTION_DISABLE":  {Name: "LEADER_ELECTION_DISABLE", Value: "true"},
		"POD_NAMES":                {Name: "POD_NAMES", Value: "v2"},
	}
	for _, env := range container.Env {
		if expected, ok := wanted[env.Name]; !ok || !reflect.DeepEqual(env, expected) {
			return invalid("controller environment")
		}
		delete(wanted, env.Name)
	}
	if len(wanted) != 0 {
		return invalid("missing controller environment")
	}
	if err := verifyNonrootControllerMounts(pod); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func verifyNonrootControllerMounts(pod *corev1.Pod) error {
	// A controller gets the existing standard projected service-account mount
	// plus exactly one public configuration mount. No Secret is introduced.
	if len(pod.Spec.Volumes) != 2 || len(pod.Spec.Containers[0].VolumeMounts) != 2 {
		return errors.New("controller requires only its public profile and standard service-account projection")
	}
	volumes := make(map[string]corev1.Volume, 2)
	for _, volume := range pod.Spec.Volumes {
		if _, duplicate := volumes[volume.Name]; duplicate {
			return errors.New("duplicate controller volume")
		}
		volumes[volume.Name] = volume
	}
	profile := volumes[argoworkflow.NonrootProfileVolume]
	if profile.ConfigMap == nil {
		return errors.New("missing required public controller configuration volume")
	}
	wantProfile := corev1.Volume{Name: argoworkflow.NonrootProfileVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: argoworkflow.NonrootProfileConfigMap},
		Items:                []corev1.KeyToPath{{Key: "config", Path: "config"}}, DefaultMode: ptr.To(int32(0444)), Optional: ptr.To(false),
	}}}
	// Kubernetes permits omitted optional:false; it has the same required-key
	// behavior. No alternative mode/key/source/subpath is admitted.
	profile.ConfigMap = profile.ConfigMap.DeepCopy()
	if profile.ConfigMap.Optional == nil {
		profile.ConfigMap.Optional = ptr.To(false)
	}
	if !reflect.DeepEqual(profile, wantProfile) {
		return errors.New("controller public configuration source differs from the required profile")
	}
	delete(volumes, argoworkflow.NonrootProfileVolume)
	var accountName string
	for name, volume := range volumes {
		accountName = name
		want := corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0644)), Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: ptr.To(int64(3607)), Path: "token"}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}}
		if !strings.HasPrefix(name, "kube-api-access-") || !reflect.DeepEqual(volume, want) {
			return errors.New("controller service-account volume is not the standard projection")
		}
	}
	wantMounts := map[string]corev1.VolumeMount{
		argoworkflow.NonrootProfileVolume: {Name: argoworkflow.NonrootProfileVolume, MountPath: argoworkflow.NonrootProfileMount, ReadOnly: true},
		accountName:                       {Name: accountName, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		want, ok := wantMounts[mount.Name]
		if !ok || !reflect.DeepEqual(mount, want) {
			return errors.New("controller mount differs from the fixed profile")
		}
		delete(wantMounts, mount.Name)
	}
	return nil
}
