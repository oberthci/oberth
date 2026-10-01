package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/yaml"
)

const (
	openBaoTLSBootstrapPodName = "openbao-tls-bootstrap"
	openBaoTLSDataClaimName    = "data-openbao-0"
	openBaoDataMountPath       = "/openbao/data"
	openBaoTLSDirectory        = openBaoDataMountPath + "/oberth-tls"
	maxOpenBaoBootstrapLog     = 64 << 10
	openBaoStandaloneTLSConfig = `ui = true

listener "tcp" {
  address         = "[::]:8200"
  cluster_address = "[::]:8201"
  tls_disable     = 0
  tls_cert_file   = "/openbao/data/oberth-tls/tls.crt"
  tls_key_file    = "/openbao/data/oberth-tls/tls.key"
  tls_min_version = "tls13"
}

storage "file" {
  path = "/openbao/data"
}
`
)

// selectedNodeAnnotation is how the scheduler tells a WaitForFirstConsumer
// provisioner which node a claim's volume must be created on.
//
// The bootstrap Pod sets spec.nodeName directly, to put the volume on the node
// the OpenBao StatefulSet already runs on. That bypasses the scheduler -- and
// with it the volume binding that would normally annotate the claim. On a
// StorageClass with volumeBindingMode: WaitForFirstConsumer, which is the
// default on k3s (local-path) and on most managed clusters, the result is a
// deadlock the installer cannot escape: the Pod waits for a claim that is
// waiting for a scheduler that will never look at it, and the install fails at
// the timeout with "PVC is not bound" and no explanation of why.
//
// Setting the annotation the scheduler would have set keeps the node pinning
// and removes the deadlock. On an Immediate-binding class the claim is already
// bound and the annotation changes nothing.
const selectedNodeAnnotation = "volume.kubernetes.io/selected-node"

func openBaoDataPVC(namespace, nodeName string) *corev1.PersistentVolumeClaim {
	annotations := map[string]string{}
	if nodeName != "" {
		annotations[selectedNodeAnnotation] = nodeName
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        openBaoTLSDataClaimName,
			Namespace:   namespace,
			Annotations: annotations,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "openbao",
				"app.kubernetes.io/instance":   "openbao",
				"app.kubernetes.io/managed-by": "oberth-installer",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
}

func resolveOberthTLSBootstrapImage(ctx context.Context, cfg Config, deps Deps) (string, error) {
	// --image names the server image outright. Resolving a chart to discover
	// one is asking a question the operator already answered, and it was the
	// question that failed: the lookup runs after the cluster exists, so a
	// version the published repository does not carry aborts an install
	// halfway through.
	if pinned := strings.TrimSpace(cfg.ImageRef); pinned != "" {
		if err := ensureLocalImageInNode(ctx, deps, pinned); err != nil {
			return "", err
		}
		return pinned, nil
	}
	if deps.RunHelm == nil {
		return "", errors.New("OpenBao TLS bootstrap requires Helm")
	}
	if deps.KindClusterName != "" {
		if err := prepareKindImagesForCluster(ctx, cfg, deps, deps.KindClusterName); err != nil {
			return "", fmt.Errorf("prepare Oberth TLS bootstrap image for kind: %w", err)
		}
	} else if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", oberthRepoName, OberthHelmRepoURL}); err != nil {
		return "", fmt.Errorf("add Oberth helm repo before TLS bootstrap: %w", err)
	}
	valuesYAML, err := deps.RunHelm(ctx, oberthChartValuesArgs(cfg))
	if err != nil {
		return "", fmt.Errorf("read Oberth chart values for TLS bootstrap image: %w", err)
	}
	var values oberthChartValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		return "", fmt.Errorf("parse Oberth chart values for TLS bootstrap image: %w", err)
	}
	image := strings.TrimSpace(values.Image.Ref)
	_, digest, pinned := splitImageDigestRef(image)
	if image == "" || !pinned || !isSHA256Digest(digest) {
		return "", errors.New("oberth chart image.ref must be pinned by a lowercase sha256 digest for OpenBao TLS bootstrap")
	}
	return image, nil
}

func openBaoDeploymentTLSState(ctx context.Context, deps Deps, namespace string) (deployed, verified bool, err error) {
	statefulSet, err := deps.KubeClient.AppsV1().StatefulSets(namespace).Get(ctx, "openbao", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect existing OpenBao StatefulSet TLS state: %w", err)
	}
	hasHTTPSAddress := false
	hasCAPath := false
	for _, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name != "openbao" {
			continue
		}
		for _, environment := range container.Env {
			switch environment.Name {
			case "BAO_ADDR":
				hasHTTPSAddress = strings.HasPrefix(environment.Value, "https://")
			case "BAO_CACERT":
				hasCAPath = environment.Value == openBaoTLSDirectory+"/ca.crt"
			}
		}
	}
	configMap, err := deps.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, "openbao-config", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, false, nil
	}
	if err != nil {
		return true, false, fmt.Errorf("inspect existing OpenBao listener TLS config: %w", err)
	}
	hcl := configMap.Data["extraconfig-from-values.hcl"]
	compact := strings.Join(strings.Fields(hcl), "")
	hasVerifiedListener := strings.Contains(compact, "tls_disable=0") &&
		strings.Contains(compact, `tls_cert_file="`+openBaoTLSDirectory+`/tls.crt"`) &&
		strings.Contains(compact, `tls_key_file="`+openBaoTLSDirectory+`/tls.key"`) &&
		strings.Contains(compact, `tls_min_version="tls13"`)
	return true, hasHTTPSAddress && hasCAPath && hasVerifiedListener, nil
}

func restartOpenBaoAfterTLSMigration(ctx context.Context, cfg Config, deps Deps, namespace string) error {
	pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: openBaoPodSelector})
	if err != nil {
		return fmt.Errorf("list OpenBao pods for TLS migration: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil
	}
	oldUIDs := make(map[string]struct{}, len(pods.Items))
	for index := range pods.Items {
		oldUIDs[string(pods.Items[index].UID)] = struct{}{}
		if err := deps.KubeClient.CoreV1().Pods(namespace).Delete(ctx, pods.Items[index].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("restart OpenBao pod for TLS migration: %w", err)
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, listErr := deps.KubeClient.CoreV1().Pods(namespace).List(deadline, metav1.ListOptions{LabelSelector: openBaoPodSelector})
		if listErr != nil {
			return fmt.Errorf("wait for TLS-enabled OpenBao pod: %w", listErr)
		}
		for index := range current.Items {
			_, old := oldUIDs[string(current.Items[index].UID)]
			if !old && current.Items[index].DeletionTimestamp == nil && isPodRunning(&current.Items[index]) {
				return nil
			}
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("timeout (%s) waiting for TLS-enabled OpenBao pod after OnDelete rollout", timeout)
		case <-ticker.C:
		}
	}
}

func openBaoTLSBootstrapPod(namespace, image, nodeName string) *corev1.Pod {
	const (
		bootstrapUID = int64(65534)
		openBaoGID   = int64(1000)
	)
	falseValue := false
	trueValue := true
	fsGroupChangePolicy := corev1.FSGroupChangeOnRootMismatch

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      openBaoTLSBootstrapPodName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "openbao",
				"app.kubernetes.io/component":  "tls-bootstrap",
				"app.kubernetes.io/managed-by": "oberth-installer",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseValue,
			NodeName:                     nodeName,
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        &trueValue,
				RunAsUser:           int64Ptr(bootstrapUID),
				RunAsGroup:          int64Ptr(openBaoGID),
				FSGroup:             int64Ptr(openBaoGID),
				FSGroupChangePolicy: &fsGroupChangePolicy,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:            "bootstrap-tls",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{
					"secretstore",
					"bootstrap-tls",
					"--output-dir=" + openBaoTLSDirectory,
					"--namespace=" + namespace,
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseValue,
					ReadOnlyRootFilesystem:   &trueValue,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: openBaoDataMountPath,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: openBaoTLSDataClaimName,
					},
				},
			}},
		},
	}
}

func int64Ptr(value int64) *int64 { return &value }

func ensureOpenBaoTLS(ctx context.Context, cfg Config, deps Deps, image string) (string, error) {
	if _, digest, ok := splitImageDigestRef(image); !ok || !isSHA256Digest(digest) {
		return "", errors.New("OpenBao TLS bootstrap requires a digest-pinned Oberth image")
	}
	namespace := cfg.OpenBaoNamespace
	if namespace == "" {
		namespace = DefaultOpenBaoNamespace
	}
	if deps.KubeClient == nil {
		return "", errors.New("OpenBao TLS bootstrap requires a Kubernetes client")
	}

	namespaceObject := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if _, err := deps.KubeClient.CoreV1().Namespaces().Create(ctx, namespaceObject, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create OpenBao namespace %s: %w", namespace, err)
	}

	// Wait for the default service account — the namespace controller creates
	// it asynchronously, and pod creation is forbidden until it exists.
	saDeadline, saCancel := context.WithTimeout(ctx, 30*time.Second)
	defer saCancel()
	for {
		_, saErr := deps.KubeClient.CoreV1().ServiceAccounts(namespace).Get(saDeadline, "default", metav1.GetOptions{})
		if saErr == nil {
			break
		}
		if saDeadline.Err() != nil {
			return "", fmt.Errorf("timed out waiting for default service account in namespace %s", namespace)
		}
		select {
		case <-saDeadline.Done():
			return "", fmt.Errorf("timed out waiting for default service account in namespace %s", namespace)
		case <-time.After(500 * time.Millisecond):
		}
	}

	// The node is resolved before the claim, not after: a claim created without
	// the node the Pod will be pinned to is a claim a WaitForFirstConsumer
	// provisioner will never act on.
	nodeName, err := currentOpenBaoNode(ctx, deps, namespace)
	if err != nil {
		return "", err
	}
	claim := openBaoDataPVC(namespace, nodeName)
	if _, err := deps.KubeClient.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("precreate OpenBao data claim: %w", err)
		}
		existing, getErr := deps.KubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claim.Name, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("inspect existing OpenBao data claim: %w", getErr)
		}
		if err := validateOpenBaoDataPVC(existing); err != nil {
			return "", err
		}
		// A claim left Pending by an earlier run of this same installer -- one
		// that created it before this fix existed -- stays Pending forever
		// otherwise. Adopting it means finishing the binding decision for it.
		if nodeName != "" && existing.Annotations[selectedNodeAnnotation] == "" &&
			existing.Status.Phase != corev1.ClaimBound {
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[selectedNodeAnnotation] = nodeName
			if _, err := deps.KubeClient.CoreV1().PersistentVolumeClaims(namespace).
				Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
				return "", fmt.Errorf("select node %s for the pending OpenBao data claim: %w", nodeName, err)
			}
		}
	}

	pod := openBaoTLSBootstrapPod(namespace, image, nodeName)
	created, err := deps.KubeClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create OpenBao TLS bootstrap pod: %w", err)
		}
		created, err = deps.KubeClient.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("inspect existing OpenBao TLS bootstrap pod: %w", err)
		}
		if err := validateOpenBaoTLSBootstrapPod(created, pod); err != nil {
			return "", err
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cleaned := false
	defer func() {
		if !cleaned {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			_ = deleteOpenBaoTLSBootstrapPod(cleanupContext, deps, namespace, pod.Name)
		}
	}()
	completed, err := waitForNamedPodTerminal(ctx, deps, namespace, pod.Name, created, timeout)
	if err != nil {
		return "", err
	}
	logs, err := openBaoBootstrapPodLogs(ctx, deps, namespace, pod.Name)
	if err != nil {
		return "", err
	}
	if completed.Status.Phase != corev1.PodSucceeded {
		return "", fmt.Errorf("OpenBao TLS bootstrap pod failed%s", commandOutputSuffix(logs))
	}
	caPEM, err := validatePublicCAPEM(logs)
	if err != nil {
		return "", fmt.Errorf("validate OpenBao TLS bootstrap output: %w", err)
	}
	deleteContext, cancelDelete := context.WithTimeout(ctx, timeout)
	defer cancelDelete()
	if err := deleteOpenBaoTLSBootstrapPod(deleteContext, deps, namespace, pod.Name); err != nil {
		return "", err
	}
	cleaned = true
	return string(caPEM), nil
}

func deleteOpenBaoTLSBootstrapPod(ctx context.Context, deps Deps, namespace, name string) error {
	if err := deps.KubeClient.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete OpenBao TLS bootstrap pod: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := deps.KubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for OpenBao TLS bootstrap pod deletion: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for OpenBao TLS bootstrap pod deletion: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateOpenBaoDataPVC(claim *corev1.PersistentVolumeClaim) error {
	if claim == nil || claim.Name != openBaoTLSDataClaimName {
		return errors.New("existing OpenBao data claim has an unexpected identity")
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		return fmt.Errorf("existing OpenBao data claim %s must use ReadWriteOnce", openBaoTLSDataClaimName)
	}
	minimum := resource.MustParse("10Gi")
	storage := claim.Spec.Resources.Requests.Storage()
	if storage == nil || storage.Cmp(minimum) < 0 {
		return fmt.Errorf("existing OpenBao data claim %s must request at least 10Gi", openBaoTLSDataClaimName)
	}
	return nil
}

func currentOpenBaoNode(ctx context.Context, deps Deps, namespace string) (string, error) {
	pods, err := deps.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: openBaoPodSelector})
	if err != nil {
		return "", fmt.Errorf("list existing OpenBao pods before TLS bootstrap: %w", err)
	}
	for index := range pods.Items {
		if pods.Items[index].Spec.NodeName != "" && pods.Items[index].DeletionTimestamp == nil {
			return pods.Items[index].Spec.NodeName, nil
		}
	}
	return "", nil
}

func validateOpenBaoTLSBootstrapPod(existing, expected *corev1.Pod) error {
	if existing == nil || expected == nil || existing.Name != expected.Name || existing.Namespace != expected.Namespace ||
		existing.Spec.AutomountServiceAccountToken == nil || *existing.Spec.AutomountServiceAccountToken ||
		existing.Spec.RestartPolicy != corev1.RestartPolicyNever || existing.Spec.HostNetwork || existing.Spec.HostPID || existing.Spec.HostIPC ||
		len(existing.Spec.InitContainers) != 0 || len(existing.Spec.EphemeralContainers) != 0 || len(existing.Spec.Containers) != 1 ||
		len(expected.Spec.Containers) != 1 || len(existing.Spec.Volumes) != 1 {
		return errors.New("an unexpected pod occupies the OpenBao TLS bootstrap name; refusing to consume its output")
	}
	if expected.Spec.NodeName != "" && existing.Spec.NodeName != expected.Spec.NodeName {
		return errors.New("an unexpected pod occupies the OpenBao TLS bootstrap name; refusing to consume its output")
	}
	podSecurity := existing.Spec.SecurityContext
	if podSecurity == nil || podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot ||
		podSecurity.RunAsUser == nil || *podSecurity.RunAsUser != 65534 || podSecurity.RunAsGroup == nil || *podSecurity.RunAsGroup != 1000 ||
		podSecurity.FSGroup == nil || *podSecurity.FSGroup != 1000 || podSecurity.FSGroupChangePolicy == nil ||
		*podSecurity.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch || podSecurity.SeccompProfile == nil ||
		podSecurity.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		return errors.New("an unexpected pod occupies the OpenBao TLS bootstrap name; refusing to consume its output")
	}
	container := existing.Spec.Containers[0]
	expectedContainer := expected.Spec.Containers[0]
	if container.Image != expectedContainer.Image || container.ImagePullPolicy != expectedContainer.ImagePullPolicy ||
		!slices.Equal(container.Command, expectedContainer.Command) || !slices.Equal(container.Args, expectedContainer.Args) ||
		len(container.Env) != 0 || len(container.EnvFrom) != 0 || len(container.VolumeMounts) != 1 || len(container.VolumeDevices) != 0 ||
		container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation ||
		container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem ||
		(container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged) || container.SecurityContext.Capabilities == nil ||
		len(container.SecurityContext.Capabilities.Add) != 0 || !slices.Equal(container.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		return errors.New("an unexpected pod occupies the OpenBao TLS bootstrap name; refusing to consume its output")
	}
	mount := container.VolumeMounts[0]
	volume := existing.Spec.Volumes[0]
	if mount.Name != "data" || mount.MountPath != openBaoDataMountPath || mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" ||
		volume.Name != "data" || volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != openBaoTLSDataClaimName ||
		volume.PersistentVolumeClaim.ReadOnly {
		return errors.New("an unexpected pod occupies the OpenBao TLS bootstrap name; refusing to consume its output")
	}
	return nil
}

func waitForNamedPodTerminal(ctx context.Context, deps Deps, namespace, name string, initial *corev1.Pod, timeout time.Duration) (*corev1.Pod, error) {
	if initial != nil && (initial.Status.Phase == corev1.PodSucceeded || initial.Status.Phase == corev1.PodFailed) {
		return initial, nil
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	current, err := deps.KubeClient.CoreV1().Pods(namespace).Get(deadline, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect OpenBao TLS bootstrap pod before watch: %w", err)
	}
	if current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed {
		return current, nil
	}
	watcher, err := deps.KubeClient.CoreV1().Pods(namespace).Watch(deadline, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", name).String(),
		ResourceVersion: current.ResourceVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("watch OpenBao TLS bootstrap pod: %w", err)
	}
	defer watcher.Stop()
	for {
		select {
		case <-deadline.Done():
			return nil, fmt.Errorf("timeout (%s) waiting for OpenBao TLS bootstrap pod", timeout)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, errors.New("OpenBao TLS bootstrap pod watch closed before completion")
			}
			pod, ok := event.Object.(*corev1.Pod)
			if ok && (pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed) {
				return pod, nil
			}
		}
	}
}

func openBaoBootstrapPodLogs(ctx context.Context, deps Deps, namespace, pod string) ([]byte, error) {
	var (
		logs []byte
		err  error
	)
	if deps.PodLogs != nil {
		logs, err = deps.PodLogs(ctx, namespace, pod)
	} else {
		logs, err = deps.KubeClient.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{LimitBytes: int64Ptr(maxOpenBaoBootstrapLog)}).DoRaw(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("read OpenBao TLS bootstrap pod logs: %w", err)
	}
	if len(logs) == 0 || len(logs) > maxOpenBaoBootstrapLog {
		return nil, errors.New("OpenBao TLS bootstrap pod output must be non-empty and bounded")
	}
	return logs, nil
}

func validatePublicCAPEM(output []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(output)
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("bootstrap output must contain exactly one public CA certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public CA certificate: %w", err)
	}
	if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 || certificate.CheckSignatureFrom(certificate) != nil ||
		certificate.SignatureAlgorithm != x509.PureEd25519 {
		return nil, errors.New("bootstrap output is not a valid self-signed CA certificate")
	}
	if _, ok := certificate.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("bootstrap CA certificate must use Ed25519")
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) {
		return nil, errors.New("bootstrap CA certificate is not valid yet")
	}
	if certificate.NotAfter.Sub(now) < 30*24*time.Hour {
		return nil, errors.New("bootstrap CA certificate expires within 30 days")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), nil
}
