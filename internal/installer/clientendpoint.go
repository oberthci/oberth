package installer

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func defaultClientHTTPSPort(serviceType string) string {
	if serviceType == "ClusterIP" {
		return "443"
	}
	return httpsNodePort
}
func clientPorts(deps Deps) (string, string) {
	ssh, https := deps.ClientSSHPort, deps.ClientHTTPSPort
	if ssh == "" {
		ssh = sshNodePort
		if deps.ClientServiceType == "ClusterIP" {
			ssh = "2222"
		}
	}
	if https == "" {
		https = defaultClientHTTPSPort(deps.ClientServiceType)
	}
	return ssh, https
}

// Discover the installed Service, including custom port values. ClusterIP
// endpoints describe internal reachability; they never imply node exposure.
func configureClientEndpoint(ctx context.Context, namespace, serviceType string, deps Deps) (Deps, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	deps.ClientServiceType = serviceType
	var service *corev1.Service
	if deps.KubeClient != nil {
		found, err := deps.KubeClient.CoreV1().Services(namespace).Get(ctx, "oberth", metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return deps, fmt.Errorf("read installed Oberth Service: %w", err)
		}
		if err == nil {
			service = found
		}
	}
	return WithServiceEndpoint(service, namespace, deps), nil
}

// WithServiceEndpoint describes the installed Service without performing I/O.
// Both the installer and wizard use the same port and reachability rules.
func WithServiceEndpoint(service *corev1.Service, namespace string, deps Deps) Deps {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if service != nil {
		deps.ClientServiceType = string(service.Spec.Type)
	}
	if deps.ClientServiceType == "ClusterIP" && deps.ClientHost == "" {
		deps.ClientHost = "oberth." + namespace + ".svc"
	}
	if service != nil {
		useServicePorts := service.Spec.Type == corev1.ServiceTypeClusterIP
		if service.Spec.Type == corev1.ServiceTypeLoadBalancer && deps.KindClusterName == "" {
			// Keep the configured/certificate-bound host. An automatically allocated
			// ingress address may not be on the installed TLS certificate.
			host := clientReachableHost(deps)
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				address := ingress.Hostname
				if address == "" {
					address = ingress.IP
				}
				if address != "" && (deps.ClientHost != "" || address == host) {
					useServicePorts = true
					break
				}
			}
		}

		for _, port := range service.Spec.Ports {
			number := port.NodePort
			if useServicePorts {
				number = port.Port
			}
			if number <= 0 {
				continue
			}
			switch port.Name {
			case "ssh":
				deps.ClientSSHPort = strconv.Itoa(int(number))
			case "https":
				deps.ClientHTTPSPort = strconv.Itoa(int(number))
			}
		}
	}
	deps.ClientSSHPort, deps.ClientHTTPSPort = clientPorts(deps)
	deps.ClientHost = clientReachableHost(deps)
	return deps
}

// withClientPorts applies the client-visible ports the operator named
// (--client-ssh-port, --client-https-port) over the Service's own.
func withClientPorts(cfg Config, deps Deps) Deps {
	if cfg.ClientSSHPort != "" {
		deps.ClientSSHPort = cfg.ClientSSHPort
	}
	if cfg.ClientHTTPSPort != "" {
		deps.ClientHTTPSPort = cfg.ClientHTTPSPort
	}
	return deps
}
