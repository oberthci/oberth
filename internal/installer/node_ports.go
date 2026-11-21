package installer

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) visibleServices(ctx context.Context, c Choices) (*corev1.ServiceList, string, error) {
	services, err := e.Cluster.Client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	scope := ""
	if apierrors.IsForbidden(err) {
		scope = "Ports checked in installation namespaces only."
		services = &corev1.ServiceList{}
		for _, ns := range []string{c.Namespace, c.PipelineNamespace} {
			list, readErr := e.Cluster.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
			if apierrors.IsNotFound(readErr) {
				continue
			}
			if readErr != nil {
				return nil, "", problem("cluster", readErr)
			}
			services.Items = append(services.Items, list.Items...)
		}
		err = nil
	}
	if err != nil {
		return nil, "", problem("cluster", err)
	}
	return services, scope, nil
}

func (e *Engine) checkNodePorts(ctx context.Context, c Choices) (string, error) {
	services, scope, err := e.visibleServices(ctx, c)
	if err != nil {
		return "", err
	}
	for _, service := range services.Items {
		if service.Namespace == c.Namespace && ownedByRelease(&service, c.Namespace) {
			continue
		}
		for _, port := range service.Spec.Ports {
			code := ""
			if int(port.NodePort) == c.HTTPSNodePort {
				code = "https-node-port"
			}
			if int(port.NodePort) == c.SSHNodePort {
				code = "ssh-node-port"
			}
			if code != "" {
				return "", described(code, nil, fmt.Sprintf("Service: %s/%s; node port: %d", service.Namespace, service.Name, port.NodePort))
			}
		}
	}
	return scope, e.dryRunNodePorts(ctx, c, services)
}
