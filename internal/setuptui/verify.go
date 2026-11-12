package setuptui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------- typed messages for async verification results ----------

var errNoCluster = errors.New("no Kubernetes cluster configured")

// clusterInfoMsg carries the result of the cluster preflight probe.
type clusterInfoMsg struct {
	nodes     []corev1.Node
	context   string
	server    string
	version   string
	engine    string // k3s, kind, gke, eks, unknown
	nodeCount int
	cores     int
	nodeName  string // first node's hostname (for TLS SANs)
	nodeIP    string // first node's internal IP (for TLS SANs)
	err       error
}

// ---------- tea.Cmd functions for async probes ----------

// probeClusterWithPath returns a tea.Cmd that loads a kubeconfig (path, or
// the default when empty) for the given context and checks the cluster.
func probeClusterWithPath(contextName, path string) tea.Cmd {
	return func() tea.Msg {
		rules := kubeconfig.Rules("")
		rules.ExplicitPath = path
		rawConfig, restConfig, err := kubeconfig.Client(rules, contextName)
		if err != nil {
			return clusterInfoMsg{err: fmt.Errorf("load kubeconfig: %w", err)}
		}
		if len(rawConfig.Contexts) == 0 {
			return clusterInfoMsg{err: errNoCluster}
		}
		selectedContext := rawConfig.CurrentContext
		if contextName != "" {
			selectedContext = contextName
		}

		restConfig.Timeout = 10 * time.Second

		client, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return clusterInfoMsg{
				context: selectedContext,
				err:     fmt.Errorf("create client: %w", err),
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		serverVersion, err := client.Discovery().ServerVersion()
		if err != nil {
			return clusterInfoMsg{
				context: selectedContext,
				server:  restConfig.Host,
				err:     fmt.Errorf("cluster unreachable: %w", err),
			}
		}

		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return clusterInfoMsg{
				context: selectedContext,
				server:  restConfig.Host,
				version: serverVersion.GitVersion,
				err:     fmt.Errorf("list nodes: %w", err),
			}
		}

		nodeCount := len(nodes.Items)
		totalCores := 0
		engine := "unknown"
		var nodeName, nodeIP string

		for i, node := range nodes.Items {
			cpu := node.Status.Capacity["cpu"]
			totalCores += int(cpu.Value())

			// Extract the first node's hostname and internal IP for TLS SANs.
			if i == 0 {
				nodeName = node.Name
				for _, addr := range node.Status.Addresses {
					if addr.Type == "InternalIP" && nodeIP == "" {
						nodeIP = addr.Address
					}
				}
			}

			if _, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
				if v, ok := node.Labels["cloud.google.com/gke-nodepool"]; ok && v != "" {
					engine = "gke"
				} else if _, ok := node.Labels["eks.amazonaws.com/nodegroup"]; ok {
					engine = "eks"
				}
			}
			if v, ok := node.Labels["node.kubernetes.io/instance-type"]; ok && v == "k3s" {
				engine = "k3s"
			}
		}
		// Heuristic: check kubelet version for k3s marker.
		if engine == "unknown" && nodeCount > 0 {
			kubeletVersion := nodes.Items[0].Status.NodeInfo.KubeletVersion
			if strings.Contains(kubeletVersion, "k3s") {
				engine = "k3s"
			} else if strings.Contains(kubeletVersion, "kind") {
				engine = "kind"
			}
		}

		return clusterInfoMsg{
			nodes:     nodes.Items,
			context:   selectedContext,
			server:    restConfig.Host,
			version:   serverVersion.GitVersion,
			engine:    engine,
			nodeCount: nodeCount,
			cores:     totalCores,
			nodeName:  nodeName,
			nodeIP:    nodeIP,
		}
	}
}
