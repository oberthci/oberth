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
	"github.com/oberthci/oberth/pkg/pipeline"
)

const nonrootProfileAnnotation = "oberth.ci/verified-controller-profile"

type nonrootProof struct {
	requestID, podUID, configUID, configVersion string
	namespace                                   string
	// configSHA256 is the verified canonical profile's content digest.
	configSHA256 string
	verifiedAt   time.Time
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
	if request.Capability != nil {
		var receipt *argoworkflow.RunSourceSummary
		if request.DemandReceipt != nil {
			summary := request.DemandReceipt.Summary()
			receipt = &summary
		}
		encoded, _ := json.Marshal(struct {
			Identity      any
			Capability    string
			SourceReceipt *argoworkflow.RunSourceSummary
		}{identity, capabilityBinding(request.RepositoryID, request.Capability), receipt})
		digest := sha256.Sum256(encoded)
		return hex.EncodeToString(digest[:])
	}
	encoded, _ := json.Marshal(identity) // Only primitive serializable types.
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (proof *nonrootProof) binding() string {
	sum := proof.configSHA256
	if sum == "" {
		sum = argoworkflow.NonrootProfileConfigSHA256 // the built-in profile
	}
	encoded, _ := json.Marshal([]string{argoworkflow.NonrootStaticProfile, sum, proof.namespace, proof.requestID, proof.podUID, proof.configUID, proof.configVersion})
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
	proof, err := controller.verifyNonrootWithRetry(ctx, request, false)
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

func (controller *Controller) expectedSourceVolume(request Request) SourceVolume {
	volume := SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath}
	if request.Assist {
		volume.BinarySubPath = binarySubPath
	}
	if controller.config.GoProxyCACertPEM != "" {
		volume.GoProxyCASubPath = goProxyCASubPath + "/" + goProxyCACertFile
	}
	if request.Trigger == pipeline.TriggerRelease || pipelineDeclaresSecretPaths(request.Source) {
		volume.BinarySubPath = binarySubPath
		if controller.config.VaultCACertPEM != "" {
			volume.VaultCASubPath = vaultCASubPath
		}
	}
	return volume
}

// verifyNonrootController proves the running controller uses a canonical
// profile (#1517). An explicit configMap pins one profile name; empty
// follows the profile the controller itself names in --configmap. Either
// way the name is a digest of the content, and the content must be exactly
// the canonical profile that name derives from, so following the controller
// admits no other configuration: it only removes the ordering between the
// argo-workflows and the Oberth release when the controller limits change.
func verifyNonrootController(ctx context.Context, client kubernetes.Interface, namespace, configMap string, request Request) (*nonrootProof, error) {
	if client == nil {
		return nil, errors.New("argojob: controller profile verification requires Kubernetes")
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=argo-workflows-workflow-controller"})
	if err != nil {
		return nil, &profileReadError{message: "argojob: cannot verify nonroot controller Pod identity", object: namespace + "/Pod/workflow-controller", cause: err}
	}
	if pods.Continue != "" || (pods.RemainingItemCount != nil && *pods.RemainingItemCount > 0) {
		return nil, errors.New("argojob: controller Pod list is incomplete")
	}
	evicted := evictedControllerCandidates(pods.Items)
	pod, err := selectNonrootControllerWithHistory(pods.Items, namespace, evicted)
	if err != nil {
		return nil, err
	}
	if configMap == "" {
		configMap = runningProfileConfigMap(pod)
	}
	if err := verifyNonrootControllerPod(pod, configMap); err != nil {
		return nil, err
	}
	if len(evicted) > 0 {
		if err := verifyEvictedControllerHistory(ctx, client, namespace, pods.Items, pod, evicted); err != nil {
			return nil, err
		}
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, configMap, metav1.GetOptions{})
	if err != nil {
		return nil, &profileReadError{message: "argojob: cannot read the exact nonroot controller profile ConfigMap", object: namespace + "/ConfigMap/" + configMap, cause: err}
	}
	if cm.Name != configMap || cm.Namespace != namespace || cm.UID == "" || cm.ResourceVersion == "" || cm.Immutable == nil || !*cm.Immutable || cm.DeletionTimestamp != nil ||
		len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
		return nil, errors.New("argojob: nonroot controller configuration is not the exact immutable built-in profile")
	}
	profile, err := argoworkflow.ParseNonrootProfile(cm.Name, cm.Data["config"])
	if err != nil {
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
	return &nonrootProof{requestID: nonrootRequestIdentity(request), namespace: namespace, podUID: string(pod.UID), configUID: string(cm.UID), configVersion: cm.ResourceVersion, configSHA256: profile.SHA256, verifiedAt: time.Now()}, nil
}

// runningProfileConfigMap is the profile ConfigMap the controller names in
// its --configmap argument when that has the profile name form, else the
// built-in profile (which verifyNonrootControllerPod then holds it to).
func runningProfileConfigMap(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 1 {
		args := pod.Spec.Containers[0].Args
		if len(args) >= 2 && args[0] == "--configmap" && argoworkflow.ValidNonrootProfileConfigMap(args[1]) {
			return args[1]
		}
	}
	return argoworkflow.NonrootProfileConfigMap
}

func verifyNonrootControllerPod(pod *corev1.Pod, configMap string) error {
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
	wantArgs := []string{"--configmap", configMap, "--executor-image", argoworkflow.NonrootExecutorImage, "--loglevel", "info", "--gloglevel", "0", "--log-format", "text", "--namespaced"}
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
	if err := verifyNonrootControllerMounts(pod, configMap); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func verifyNonrootControllerMounts(pod *corev1.Pod, configMap string) error {
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
		LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
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

// VerifyNonrootControllerProfile observes the exact supported installed boundary.
// It does not issue a submission proof or authorize any repository request.
func VerifyNonrootControllerProfile(ctx context.Context, client kubernetes.Interface, namespace, configMap string) error {
	if client == nil {
		return errors.New("argojob: controller profile verification requires Kubernetes")
	}
	_, err := verifyNonrootController(ctx, client, namespace, configMap, Request{})
	return err
}
