package installer

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const podSecurityEnforceLabel = "pod-security.kubernetes.io/enforce"

// Read both namespaces before any install write. Never relax an existing
// namespace's security policy; that decision belongs to its administrator.
func preflightPodSecurity(ctx context.Context, cfg Config, deps Deps, create bool) error {
	if deps.KubeClient == nil {
		return errors.New("pod security preflight requires Kubernetes namespace access")
	}
	var missing []string
	namespaces := deps.KubeClient.CoreV1().Namespaces()
	for _, name := range []string{cfg.Namespace, cfg.ArgoNamespace} {
		ns, err := namespaces.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return fmt.Errorf("pod security preflight: read namespace %q: %w", name, err)
		}
		if err := checkPodSecurityNamespace(ns); err != nil {
			return err
		}
	}
	if !create {
		return nil
	}
	for _, name := range missing {
		_, err := namespaces.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{podSecurityEnforceLabel: "privileged"},
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// Another installer may have created it after preflight. Recheck
			// its policy instead of treating AlreadyExists as approval.
			ns, readErr := namespaces.Get(ctx, name, metav1.GetOptions{})
			if readErr != nil {
				return fmt.Errorf("pod security preflight: reread namespace %q: %w", name, readErr)
			}
			if err := checkPodSecurityNamespace(ns); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("create namespace %q with %s=privileged: %w", name, podSecurityEnforceLabel, err)
		}
	}
	return nil
}

func checkPodSecurityNamespace(ns *corev1.Namespace) error {
	level := ns.Labels[podSecurityEnforceLabel]
	if level == "baseline" || level == "restricted" {
		return fmt.Errorf("namespace %q enforces Pod Security %q; Oberth hostPath caches and its root cache initializer require %s=privileged; ask the namespace administrator to set that label before installing", ns.Name, level, podSecurityEnforceLabel)
	}
	return nil
}
