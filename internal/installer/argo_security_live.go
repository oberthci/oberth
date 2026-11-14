package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	argoconfig "github.com/argoproj/argo-workflows/v4/config"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func argoExternalControllerConfigMap(values map[string]any) bool {
	create, ok := valueAt(values, "controller", "configMap", "create").(bool)
	return ok && !create
}

func argoSecurityConfigMapRemedy(namespace string, cause error, stored map[string]any) error {
	var invalid *argoWorkflowDefaultsInvalid
	if errors.As(cause, &invalid) {
		return fmt.Errorf("cannot reconcile Argo security defaults: %w. Correct the named native key on the live controller ConfigMap in namespace %s, under data.config (or equivalent split data entries); preserve unrelated configuration", cause, namespace)
	}
	args, _, _ := argoSecurityDefaults(nil)
	keys := []string{}
	for i := 0; i+1 < len(args); i += 2 {
		if strings.HasPrefix(args[i+1], "mainContainer.securityContext.") || strings.HasPrefix(args[i+1], "executor.securityContext.") {
			keys = append(keys, args[i+1])
		}
	}
	hint := ""
	if argoExternalControllerConfigMap(stored) {
		hint = fmt.Sprintf("\nWith controller.configMap.create=false, native Helm values do not reach this ConfigMap. If this release used the old controller profile, complete the v0.22.5 migration in docs/releases/v0.22.5.md: export the complete user values with helm get values argo-workflows -n %s -o yaml; record the old ConfigMap name; remove controller.configMap.create and controller.configMap.name, only the oberth-controller-profile volume and mount, and only the old named profile ConfigMap from extraObjects. Review controller.image.tag and executor.image.tag as documented. Preserve unrelated values and apply the complete reviewed file with helm upgrade argo-workflows argo/argo-workflows -n %s --version 1.0.24 --reset-values --values <reviewed-values.yaml>; --reuse-values does not remove omitted keys", namespace, namespace)
	}
	return fmt.Errorf("cannot reconcile Argo security defaults: %w. Configure these exact native keys on the live controller ConfigMap referenced by the Deployment in namespace %s, under data.config (or equivalent split data entries); preserve unrelated configuration:\n%s%s", cause, namespace, strings.Join(keys, "\n"), hint)
}

// Follow the live controller's ConfigMap reference rather than trusting Helm's
// stored intent. Both native Helm and namespace-install configurations use the
// same Argo parser. This checks defaults, not controller identity or images.
type argoSecurityDefaultsMissing struct{ message string }

func (e *argoSecurityDefaultsMissing) Error() string { return e.message }

func checkLiveArgoSecurity(ctx context.Context, deps Deps, namespace string) error {
	if deps.KubeClient == nil {
		return fmt.Errorf("cannot read the live Argo controller configuration")
	}
	deployments, err := deps.KubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot read live Argo controller Deployments")
	}
	found := false
	for _, deployment := range deployments.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			controller := false
			for _, command := range container.Command {
				controller = controller || path.Base(command) == "workflow-controller"
			}
			labels := deployment.Spec.Template.Labels
			controller = controller || (container.Name == "workflow-controller" && (labels["app"] == "workflow-controller" || labels["app.kubernetes.io/name"] == argoControllerLabelSelectorValue))
			if !controller {
				continue
			}
			found = true
			name := common.ConfigMapName
			args := append(append([]string{}, container.Command...), container.Args...)
			for i, arg := range args {
				if arg == "--configmap" {
					if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
						return fmt.Errorf("live Argo controller has an invalid --configmap argument")
					}
					name = args[i+1]
				} else if value, ok := strings.CutPrefix(arg, "--configmap="); ok {
					if value == "" {
						return fmt.Errorf("live Argo controller has an empty --configmap argument")
					}
					name = value
				}
			}
			config, err := argoconfig.NewController(namespace, name, deps.KubeClient).Get(ctx)
			if err != nil {
				return fmt.Errorf("cannot read or parse live Argo controller ConfigMap %s/%s", namespace, name)
			}
			if err := checkLiveArgoWorkflowSecurity(config, namespace, name); err != nil {
				return err
			}
		}
	}
	if !found {
		return fmt.Errorf("no live Argo controller Deployment found in namespace %s", namespace)
	}
	return nil
}

func checkLiveArgoWorkflowSecurity(config *argoconfig.Config, namespace, name string) error {
	var pod *corev1.PodSecurityContext
	location := fmt.Sprintf("live Argo controller ConfigMap %s/%s", namespace, name)
	if config.WorkflowDefaults != nil {
		spec := &config.WorkflowDefaults.Spec
		if err := validateArgoWorkflowDefaults(location+" workflowDefaults.spec", spec); err != nil {
			return err
		}
		pod, _ = effectiveArgoWorkflowPodSecurity(spec)
	}
	if config.MainContainer == nil || !argoWorkflowSecurityContextMeetsFloor(config.MainContainer.SecurityContext, pod) {
		return &argoSecurityDefaultsMissing{location + " lacks required mainContainer.securityContext defaults"}
	}
	if config.Executor == nil || !argoWorkflowSecurityContextMeetsFloor(config.Executor.SecurityContext, pod) {
		return &argoSecurityDefaultsMissing{location + " lacks required executor.securityContext defaults"}
	}
	return nil
}

const argoControllerLabelSelectorValue = "argo-workflows-workflow-controller"

func approveArgoSecurity(deps Deps) error {
	if deps.Output == nil {
		return fmt.Errorf("the Argo security changes require a visible review")
	}
	_, _ = fmt.Fprint(deps.Output, "Review: fill only the absent Argo security defaults shown above on this existing release? [y/N]: ")
	// Read exactly one line so subsequent installer prompts retain their input.
	var answer strings.Builder
	reader := deps.Input
	var buf [1]byte
	for {
		_, err := io.ReadFull(reader, buf[:])
		b := buf[0]
		if err != nil {
			return fmt.Errorf("the Argo security defaults were not approved")
		}
		if b == '\n' {
			break
		}
		answer.WriteByte(b)
	}
	if a := strings.TrimSpace(answer.String()); !strings.EqualFold(a, "y") && !strings.EqualFold(a, "yes") {
		return fmt.Errorf("the Argo security defaults were not approved")
	}
	return nil
}
