package installer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type ClientLoadBalancerProposal struct {
	InstallFlags []string `json:"install_flags"`
	Advice       string   `json:"advice"`
}

// WaitClientLoadBalancer reads only the selected plan's kubeconfig and context.
// The approved install creates the Service; this helper never creates resources.
func WaitClientLoadBalancer(ctx context.Context, plan InstallPlan, namespace string) (string, error) {
	if plan.Version != 1 || plan.Identity.Context == "" || len(plan.Identity.Files) == 0 {
		return "", errors.New("LoadBalancer wait requires the selected plan's kubeconfig files and context")
	}
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: plan.Identity.Files}
	_, restConfig, err := kubeconfig.Client(rules, plan.Identity.Context)
	if err != nil {
		return "", errors.New("cannot load selected plan's kubeconfig")
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", errors.New("cannot create selected cluster client")
	}
	return waitClientLoadBalancer(ctx, kube, namespace, time.Second)
}

func waitClientLoadBalancer(ctx context.Context, kube kubernetes.Interface, namespace string, interval time.Duration) (string, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	// Even callers without a deadline get a bounded wait.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		service, err := kube.CoreV1().Services(namespace).Get(ctx, "oberth", metav1.GetOptions{})
		if err == nil && service.Spec.Type == "LoadBalancer" {
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				address := ingress.Hostname
				if address == "" {
					address = ingress.IP
				}
				if address != "" {
					normalized, err := NormalizeClientHost(address)
					if err != nil {
						return "", errors.New("LoadBalancer returned an invalid address")
					}
					return normalized, nil
				}
			}
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("read Oberth LoadBalancer Service: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("wait for Oberth LoadBalancer address: %w; inspect Service oberth in namespace %s and its events, then retry or choose port-forward", ctx.Err(), namespace)
		case <-timer.C:
		}
	}
}
