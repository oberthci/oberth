package setuptui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------- typed messages for async verification results ----------

// clusterInfoMsg carries the result of the cluster preflight probe.
type clusterInfoMsg struct {
	context   string
	server    string
	version   string
	engine    string // k3s, kind, gke, eks, unknown
	isLocal   bool
	nodeCount int
	cores     int
	nodeName  string // first node's hostname (for TLS SANs)
	nodeIP    string // first node's internal IP (for TLS SANs)
	err       error
}

// forgeDiscoveryMsg carries the result of upstream forge discovery.
type forgeDiscoveryMsg struct {
	repos []discoveredRepo
	err   error
}

type discoveredRepo struct {
	name    string
	adopted bool // has .oberth/ directory
	errMsg  string
}

// ---------- tea.Cmd functions for async probes ----------

// probeCluster returns a tea.Cmd that loads kubeconfig for the given context
// and runs preflight checks against the cluster.
func probeCluster(contextName string) tea.Cmd {
	return func() tea.Msg {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		overrides := &clientcmd.ConfigOverrides{}
		if contextName != "" {
			overrides.CurrentContext = contextName
		}
		config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

		rawConfig, err := config.RawConfig()
		if err != nil {
			return clusterInfoMsg{err: fmt.Errorf("load kubeconfig: %w", err)}
		}
		selectedContext := rawConfig.CurrentContext
		if contextName != "" {
			selectedContext = contextName
		}

		restConfig, err := config.ClientConfig()
		if err != nil {
			return clusterInfoMsg{
				context: selectedContext,
				err:     fmt.Errorf("build REST config: %w", err),
			}
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
		isLocal := false
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
				isLocal = true
			}
		}
		// Heuristic: check kubelet version for k3s marker.
		if engine == "unknown" && nodeCount > 0 {
			kubeletVersion := nodes.Items[0].Status.NodeInfo.KubeletVersion
			if strings.Contains(kubeletVersion, "k3s") {
				engine = "k3s"
				isLocal = true
			} else if strings.Contains(kubeletVersion, "kind") {
				engine = "kind"
				isLocal = true
			}
		}

		return clusterInfoMsg{
			context:   selectedContext,
			server:    restConfig.Host,
			version:   serverVersion.GitVersion,
			engine:    engine,
			isLocal:   isLocal,
			nodeCount: nodeCount,
			cores:     totalCores,
			nodeName:  nodeName,
			nodeIP:    nodeIP,
		}
	}
}

// probeForge returns a tea.Cmd that discovers repositories on the upstream forge.
// This is a stub — the real implementation would call the forge API.
func probeForge(_, _ string) tea.Cmd {
	return func() tea.Msg {
		return forgeDiscoveryMsg{
			err: fmt.Errorf("looking up repositories from here is coming soon — the install discovers them in your organization"),
		}
	}
}
