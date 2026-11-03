package vmrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

var (
	vmiResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

const (
	vmTierLabel          = "oberth.ci/tier"
	vmSpecAnnotation     = "oberth.ci/vm-spec"
	vmIdentityAnnotation = "oberth.ci/vm-identity"
)

// KubeVirtConfig is operator-owned. This transport supports an offline amd64
// kernelBoot guest only. Networked suite profiles need separate admission.
type KubeVirtConfig struct {
	Namespace           string
	ServerNamespace     string
	GuestServiceAccount string
	RequestTimeout      time.Duration
	PollInterval        time.Duration
}

// KubeVirtBackend implements lifecycle transport, not suite result authority.
// It deliberately has no candidate command, credential or arbitrary YAML input.
type KubeVirtBackend struct {
	client dynamic.Interface
	config KubeVirtConfig
}

func NewKubeVirtBackend(client dynamic.Interface, config KubeVirtConfig) (*KubeVirtBackend, error) {
	if client == nil || config.Namespace == config.ServerNamespace ||
		len(validation.IsDNS1123Label(config.Namespace)) != 0 ||
		len(validation.IsDNS1123Label(config.ServerNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(config.GuestServiceAccount)) != 0 {
		return nil, errors.New("vmrunner: require client, distinct valid pipeline/server namespaces and a guest ServiceAccount")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 10 * time.Second
	}
	if config.PollInterval == 0 {
		config.PollInterval = time.Second
	}
	if config.RequestTimeout < time.Millisecond || config.RequestTimeout > time.Minute ||
		config.PollInterval < time.Millisecond || config.PollInterval > 10*time.Second {
		return nil, errors.New("vmrunner: invalid bounded API timeout or polling interval")
	}
	return &KubeVirtBackend{client: client, config: config}, nil
}

func (backend *KubeVirtBackend) checkNamespace(namespace string) error {
	if namespace != backend.config.Namespace {
		return errors.New("vmrunner: backend namespace mismatch")
	}
	return nil
}

func (backend *KubeVirtBackend) CreateVMI(ctx context.Context, namespace string, spec VMRunSpec) (VMInstance, error) {
	if err := backend.checkNamespace(namespace); err != nil {
		return VMInstance{}, errors.Join(ErrCreateRefused, err)
	}
	if err := ValidateVMRunSpec(spec); err != nil {
		return VMInstance{}, errors.Join(ErrCreateRefused, err)
	}
	object := backend.object(spec)
	// Retain a deterministic intent even if the API outcome is ambiguous. A
	// caller must persist this intent BEFORE calling CreateVMI. An empty UID
	// cannot authorize cleanup or release a durable capacity reservation.
	intent := VMInstance{Name: object.GetName(), SpecIdentity: SpecIdentity(spec)}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	if err := call.Err(); err != nil {
		return intent, errors.Join(ErrCreateRefused, err)
	}
	created, err := backend.client.Resource(vmiResource).Namespace(namespace).Create(call, object, metav1.CreateOptions{})
	// Only explicit API rejections prove that this request did not create.
	// Timeout, cancellation after submission, EOF and 5xx remain ambiguous.
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsNotFound(err) {
		return intent, errors.Join(ErrCreateRefused, err)
	}
	if apierrors.IsAlreadyExists(err) {
		created, err = backend.client.Resource(vmiResource).Namespace(namespace).Get(call, object.GetName(), metav1.GetOptions{})
	}
	if err != nil {
		return intent, err
	}
	observation, err := backend.observation(created)
	if err != nil {
		return intent, err
	}
	if observation.Instance.Name != intent.Name || observation.Instance.SpecIdentity != intent.SpecIdentity {
		return intent, errors.New("vmrunner: create response differs from submitted identity")
	}
	return observation.Instance, nil
}

func (backend *KubeVirtBackend) GetVMI(ctx context.Context, namespace, name string) (VMObservation, error) {
	if err := backend.checkNamespace(namespace); err != nil {
		return VMObservation{}, err
	}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	object, err := backend.client.Resource(vmiResource).Namespace(namespace).Get(call, name, metav1.GetOptions{})
	if err != nil {
		return VMObservation{}, err
	}
	observation, err := backend.observation(object)
	if err != nil {
		return VMObservation{}, err
	}
	pods, err := backend.list(ctx, podResource, "")
	if err != nil {
		return VMObservation{}, err
	}
	for i := range pods {
		if ownedLauncher(&pods[i], observation.Instance) {
			if err := backend.verifyLauncher(&pods[i]); err != nil {
				return VMObservation{}, err
			}
		}
	}
	return observation, nil
}

func (backend *KubeVirtBackend) observation(object *unstructured.Unstructured) (VMObservation, error) {
	if object == nil || object.GetUID() == "" || object.GetCreationTimestamp().Time.IsZero() ||
		object.GetNamespace() != backend.config.Namespace || object.GetAPIVersion() != "kubevirt.io/v1" ||
		object.GetKind() != "VirtualMachineInstance" || object.GetLabels()[vmTierLabel] != "vm-runner" {
		return VMObservation{}, errors.New("vmrunner: VMI metadata does not establish ownership")
	}
	body := []byte(object.GetAnnotations()[vmSpecAnnotation])
	if len(body) == 0 || len(body) > 8192 {
		return VMObservation{}, errors.New("vmrunner: missing or oversized admitted spec")
	}
	var spec VMRunSpec
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return VMObservation{}, fmt.Errorf("vmrunner: invalid admitted spec: %w", err)
	}
	canonical, err := json.Marshal(spec)
	if err != nil || !bytes.Equal(body, canonical) {
		return VMObservation{}, errors.New("vmrunner: noncanonical admitted spec")
	}
	if err := ValidateVMRunSpec(spec); err != nil {
		return VMObservation{}, err
	}
	expected := backend.object(spec)
	if object.GetName() != expected.GetName() || object.GetAnnotations()[vmIdentityAnnotation] != SpecIdentity(spec) {
		return VMObservation{}, errors.New("vmrunner: VMI spec identity mismatch")
	}
	for key := range object.GetAnnotations() {
		switch key {
		case vmSpecAnnotation, vmIdentityAnnotation, "kubevirt.io/latest-observed-api-version", "kubevirt.io/storage-observed-api-version":
		default:
			return VMObservation{}, fmt.Errorf("vmrunner: unapproved VMI annotation %s", key)
		}
	}
	if err := compareVMISpec("spec", expected.Object["spec"], object.Object["spec"]); err != nil {
		return VMObservation{}, err
	}
	phase, _, err := unstructured.NestedString(object.Object, "status", "phase")
	if err != nil {
		return VMObservation{}, err
	}
	return VMObservation{Instance: VMInstance{Name: object.GetName(), UID: string(object.GetUID()), SpecIdentity: SpecIdentity(spec), CreatedAt: object.GetCreationTimestamp().Time}, Phase: phase}, nil
}

func (backend *KubeVirtBackend) object(spec VMRunSpec) *unstructured.Unstructured {
	identity := SpecIdentity(spec)
	body, _ := json.Marshal(spec)
	memory := fmt.Sprintf("%dMi", spec.Resources.MemoryMiB)
	volumes, disks := []any{}, []any{}
	if spec.Resources.DiskGiB > 0 {
		volumes = append(volumes, map[string]any{"name": "scratch", "emptyDisk": map[string]any{"capacity": fmt.Sprintf("%dGi", spec.Resources.DiskGiB)}})
		disks = append(disks, map[string]any{"name": "scratch", "disk": map[string]any{"bus": "virtio"}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
		"metadata": map[string]any{"name": InstanceName(spec), "namespace": backend.config.Namespace,
			"labels":      map[string]any{vmTierLabel: "vm-runner"},
			"annotations": map[string]any{vmSpecAnnotation: string(body), vmIdentityAnnotation: identity}},
		"spec": map[string]any{
			"architecture": "amd64", "serviceAccountName": backend.config.GuestServiceAccount,
			"terminationGracePeriodSeconds": int64(0),
			"nodeSelector":                  map[string]any{"kubernetes.io/arch": "amd64", "kubevirt.io/schedulable": "true"},
			"networks":                      []any{}, "volumes": volumes,
			"domain": map[string]any{
				"machine":   map[string]any{"type": "q35"},
				"memory":    map[string]any{"guest": memory},
				"features":  map[string]any{"acpi": map[string]any{"enabled": true}},
				"cpu":       map[string]any{"cores": int64(spec.Resources.CPUCores)},
				"resources": map[string]any{"requests": map[string]any{"cpu": fmt.Sprint(spec.Resources.CPUCores), "memory": memory}, "limits": map[string]any{"cpu": fmt.Sprint(spec.Resources.CPUCores), "memory": memory}},
				"firmware": map[string]any{"kernelBoot": map[string]any{
					"container":  map[string]any{"image": spec.GuestImageRef, "kernelPath": "/boot/vmlinuz", "initrdPath": "/boot/initramfs"},
					"kernelArgs": "console=ttyS0 panic=-1 rdinit=/init lsm=landlock,lockdown,yama,integrity,apparmor,bpf"}},
				"devices": map[string]any{"autoattachPodInterface": false, "autoattachGraphicsDevice": false, "autoattachSerialConsole": true,
					"autoattachVSOCK": false, "disableHotplug": true, "interfaces": []any{}, "disks": disks},
			},
		},
	}}
}

// DeleteVMI requires a previously bound UID. It never infers ownership from a
// name, never deletes a replacement, and only completes after observing both
// the VMI and every launcher with this exact owner UID absent.
func (backend *KubeVirtBackend) DeleteVMI(ctx context.Context, namespace string, instance VMInstance) error {
	if err := backend.checkNamespace(namespace); err != nil {
		return err
	}
	if instance.Name == "" || instance.UID == "" || instance.SpecIdentity == "" {
		return errors.New("vmrunner: deletion requires exact ownership")
	}
	cleanup, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		if err := cleanup.Err(); err != nil {
			return err
		}
		call, cancelCall := context.WithTimeout(cleanup, backend.config.RequestTimeout)
		object, err := backend.client.Resource(vmiResource).Namespace(namespace).Get(call, instance.Name, metav1.GetOptions{})
		cancelCall()
		absent := apierrors.IsNotFound(err)
		if err != nil && !absent {
			return err
		}
		if !absent {
			if string(object.GetUID()) != instance.UID || object.GetLabels()[vmTierLabel] != "vm-runner" || object.GetAnnotations()[vmIdentityAnnotation] != instance.SpecIdentity {
				return errors.New("vmrunner: refusing deletion of a replaced or unowned VMI")
			}
			if object.GetDeletionTimestamp() == nil {
				if err := backend.deleteOwned(cleanup, vmiResource, instance.Name, instance.UID); err != nil {
					return err
				}
			}
		}
		pods, err := backend.list(cleanup, podResource, "")
		if err != nil {
			return err
		}
		launchers := 0
		for i := range pods {
			pod := &pods[i]
			if !ownedLauncher(pod, instance) {
				continue
			}
			launchers++
			if pod.GetUID() == "" {
				return errors.New("vmrunner: launcher has no UID")
			}
			if pod.GetDeletionTimestamp() == nil {
				if err := backend.deleteOwned(cleanup, podResource, pod.GetName(), string(pod.GetUID())); err != nil {
					return err
				}
			}
		}
		if absent && launchers == 0 {
			return nil
		}
		timer := time.NewTimer(backend.config.PollInterval)
		select {
		case <-cleanup.Done():
			timer.Stop()
			return cleanup.Err()
		case <-timer.C:
		}
	}
}

func ownedLauncher(pod *unstructured.Unstructured, instance VMInstance) bool {
	for _, owner := range pod.GetOwnerReferences() {
		if owner.APIVersion == "kubevirt.io/v1" && owner.Kind == "VirtualMachineInstance" && owner.Name == instance.Name && string(owner.UID) == instance.UID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func (backend *KubeVirtBackend) deleteOwned(ctx context.Context, resource schema.GroupVersionResource, name, uid string) error {
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	identity := types.UID(uid)
	policy := metav1.DeletePropagationForeground
	err := backend.client.Resource(resource).Namespace(backend.config.Namespace).Delete(call, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &identity}, PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (backend *KubeVirtBackend) list(ctx context.Context, resource schema.GroupVersionResource, selector string) ([]unstructured.Unstructured, error) {
	var objects []unstructured.Unstructured
	options := metav1.ListOptions{Limit: 200, LabelSelector: selector}
	for page := 0; page < 20; page++ {
		call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
		list, err := backend.client.Resource(resource).Namespace(backend.config.Namespace).List(call, options)
		cancel()
		if err != nil {
			return nil, err
		}
		objects = append(objects, list.Items...)
		if len(objects) > 4000 {
			return nil, errors.New("vmrunner: resource list exceeded bound")
		}
		options.Continue = list.GetContinue()
		if options.Continue == "" {
			return objects, nil
		}
	}
	return nil, errors.New("vmrunner: resource pagination exceeded bound")
}

func (backend *KubeVirtBackend) ListVMIs(ctx context.Context, namespace string, olderThan time.Duration) ([]VMInstance, error) {
	if err := backend.checkNamespace(namespace); err != nil {
		return nil, err
	}
	if olderThan <= 0 {
		return nil, errors.New("vmrunner: sweep age must be positive")
	}
	objects, err := backend.list(ctx, vmiResource, vmTierLabel+"=vm-runner")
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-olderThan)
	var instances []VMInstance
	for i := range objects {
		observation, err := backend.observation(&objects[i])
		if err != nil {
			return nil, err
		}
		if observation.Instance.CreatedAt.Before(cutoff) {
			instances = append(instances, observation.Instance)
		}
	}
	return instances, nil
}

var _ VMBackend = (*KubeVirtBackend)(nil)

// KubeVirt's fixed VMI recipe excludes service-account volumes. Independently
// reject credential projection injected into its actual launcher by defaults
// or admission; a safe-looking VMI annotation is not evidence about the Pod.
func (backend *KubeVirtBackend) verifyLauncher(pod *unstructured.Unstructured) error {
	token, found, err := unstructured.NestedBool(pod.Object, "spec", "automountServiceAccountToken")
	if err != nil || !found || token {
		return errors.New("vmrunner: launcher token automount is not explicitly disabled")
	}
	account, _, err := unstructured.NestedString(pod.Object, "spec", "serviceAccountName")
	if err != nil || account != backend.config.GuestServiceAccount {
		return errors.New("vmrunner: launcher ServiceAccount differs from operator recipe")
	}
	spec, found, err := unstructured.NestedMap(pod.Object, "spec")
	if err != nil || !found {
		return errors.New("vmrunner: launcher spec is absent")
	}
	return rejectLauncherCredentials(spec)
}

func rejectLauncherCredentials(value any) error {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			switch key {
			case "secret", "secretKeyRef", "secretRef", "serviceAccountToken":
				return fmt.Errorf("vmrunner: launcher contains a credential source: %s", key)
			}
			if err := rejectLauncherCredentials(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := rejectLauncherCredentials(child); err != nil {
				return err
			}
		}
	}
	return nil
}
