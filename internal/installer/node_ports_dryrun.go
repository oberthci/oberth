package installer

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (e *Engine) dryRunNodePorts(ctx context.Context, c Choices, services *corev1.ServiceList) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{GenerateName: "oberth-port-check-", Namespace: c.Namespace}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}
	update := false
	for _, existing := range services.Items {
		if existing.Namespace == c.Namespace && existing.Name == "oberth" && ownedByRelease(&existing, c.Namespace) {
			service = existing.DeepCopy()
			update = true
			break
		}
	}
	service.Spec.Ports = []corev1.ServicePort{
		{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt(8443), NodePort: int32(c.HTTPSNodePort)},
		{Name: "ssh", Protocol: corev1.ProtocolTCP, Port: 22, TargetPort: intstr.FromInt(2222), NodePort: int32(c.SSHNodePort)},
	}
	_, err := e.Cluster.Client.CoreV1().Namespaces().Get(ctx, c.Namespace, metav1.GetOptions{})
	// A dry-run create uses default until the chosen server namespace exists.
	if apierrors.IsNotFound(err) {
		service.Namespace = "default"
	} else if err != nil {
		return problem("cluster", err)
	}
	api := e.Cluster.Client.CoreV1().Services(service.Namespace)
	if update {
		_, err = api.Update(ctx, service, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
	} else {
		_, err = api.Create(ctx, service, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	}
	if err != nil {
		return serviceDryRunError(err, update)
	}
	return nil
}

func serviceDryRunError(err error, update bool) error {
	if apierrors.IsForbidden(err) {
		if update {
			return problem("service-update", err)
		}
		return problem("service-create", err)
	}
	var status apierrors.APIStatus
	if apierrors.IsInvalid(err) && errors.As(err, &status) && status.Status().Details != nil {
		for _, cause := range status.Status().Details.Causes {
			if strings.HasSuffix(cause.Field, ".nodePort") {
				return problem("node-port-api", err)
			}
		}
	}
	return problem("cluster", err)
}
