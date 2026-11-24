package installer

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type namespaceState struct {
	serverAbsent bool
	server       *corev1.Namespace
	pipeline     *corev1.Namespace
}

func (e *Engine) namespaceSnapshot(ctx context.Context, c Choices) (namespaceState, error) {
	var state namespaceState
	for _, name := range []string{c.Namespace, c.PipelineNamespace} {
		ns, err := e.Cluster.Client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			state.serverAbsent = state.serverAbsent || name == c.Namespace
			continue
		}
		if err != nil {
			return namespaceState{}, problem("cluster", err)
		}
		owner := ns.Annotations["meta.helm.sh/release-namespace"]
		release := ns.Annotations["meta.helm.sh/release-name"]
		if owner != "" && (owner != c.Namespace || release != "oberth") {
			return namespaceState{}, namespaceOwner(name, owner, release)
		}
		if name == c.PipelineNamespace {
			state.pipeline = ns
		} else {
			state.server = ns
		}
	}
	if err := e.checkNamespaceReleases(ctx, c); err != nil {
		return namespaceState{}, err
	}
	if state.pipeline != nil {
		return state, pipelinePodSecurity(state.pipeline)
	}
	return state, nil
}

func namespaceOwner(name, namespace, release string) error {
	return described("namespace-owner", nil, fmt.Sprintf("Namespace: %s; Helm release: %s/%s", name, namespace, release))
}

func checkPipelineChoice(c Choices, stored map[string]any) error {
	previous := deployedChoices(Choices{}, stored).PipelineNamespace
	requested := c.PipelineNamespace

	if previous != "" && requested != "" && previous != requested {
		return problem("pipeline-change", nil)
	}
	return nil
}

// Publish reviewed answers only after the final namespace membership read.
// A concurrent create is a refusal; it never becomes an update or adoption.
func (e *Engine) beginInstallation(ctx context.Context, p *Plan, absent bool) error {
	if err := e.checkReviewedOwner(ctx, p); err != nil {
		return err
	}
	if err := e.saveInitialAnswers(p); err != nil {
		return err
	}
	if !absent {
		return nil
	}
	name := p.choices.Namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name,
		Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm", podSecurityEnforce: "restricted"},
		Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": name},
	}}
	_, err := e.Cluster.Client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		return problem("cluster", err)
	}
	return nil
}
