package installer

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	clientHostAnnotation      = "oberth.ci/client-host"
	clientSSHPortAnnotation   = "oberth.ci/client-ssh-port"
	clientHTTPSPortAnnotation = "oberth.ci/client-https-port"
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
	return withInstalledServiceEndpoint(service, namespace, deps)
}

func withInstalledServiceEndpoint(service *corev1.Service, namespace string, deps Deps) (Deps, error) {
	stored, err := withStoredClientEndpoint(service, Deps{})
	if err != nil {
		return deps, err
	}
	// The stored host must participate in LoadBalancer port discovery too.
	if deps.ClientHost == "" {
		deps.ClientHost = stored.ClientHost
	}
	deps = WithServiceEndpoint(service, namespace, deps)
	return withClientEndpoint(Config{ClientHost: stored.ClientHost, ClientSSHPort: stored.ClientSSHPort, ClientHTTPSPort: stored.ClientHTTPSPort}, deps), nil
}

// Service annotations are Helm-owned public advertised endpoints. They can
// differ from Service ports when a host forwards traffic into the cluster.
func withStoredClientEndpoint(service *corev1.Service, deps Deps) (Deps, error) {
	if service == nil {
		return deps, nil
	}
	if host := service.Annotations[clientHostAnnotation]; host != "" {
		normalized, err := NormalizeClientHost(host)
		if err != nil {
			return deps, fmt.Errorf("invalid stored client host")
		}
		deps.ClientHost = normalized
	}
	for _, port := range []struct {
		key         string
		destination *string
	}{{clientSSHPortAnnotation, &deps.ClientSSHPort}, {clientHTTPSPortAnnotation, &deps.ClientHTTPSPort}} {
		if value := service.Annotations[port.key]; value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != value {
				return deps, fmt.Errorf("invalid stored client port")
			}
			*port.destination = value
		}
	}
	return deps, nil
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

// withClientEndpoint applies explicit client flags over discovered and stored endpoints.
func withClientEndpoint(cfg Config, deps Deps) Deps {
	if cfg.ClientHost != "" {
		deps.ClientHost = cfg.ClientHost
	}
	if cfg.ClientSSHPort != "" {
		deps.ClientSSHPort = cfg.ClientSSHPort
	}
	if cfg.ClientHTTPSPort != "" {
		deps.ClientHTTPSPort = cfg.ClientHTTPSPort
	}
	return deps
}
