package installer

import "fmt"

// ClientKindRemedy is printed before install. Creating a replacement cluster is
// an operator choice; the helper neither deletes nor recreates any cluster.
type ClientKindRemedy struct {
	Config        string `json:"config"`
	CreateCommand string `json:"create_command"`
	Advice        string `json:"advice"`
}

func clientKindRemedy(https, ssh string) *ClientKindRemedy {
	return &ClientKindRemedy{
		Config: fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30022
        hostPort: %s
        listenAddress: "127.0.0.1"
        protocol: TCP
      - containerPort: 30443
        hostPort: %s
        listenAddress: "127.0.0.1"
        protocol: TCP
`, ssh, https),
		CreateCommand: "test ! -e oberth-kind.kubeconfig && test ! -L oberth-kind.kubeconfig && kind create cluster --name oberth-onboarding --config oberth-kind.yaml --kubeconfig oberth-kind.kubeconfig",
		Advice:        "Required published mappings could not be verified for the selected kind cluster. Save this complete config as oberth-kind.yaml and use the create command for a new cluster, or choose port-forward. Do not delete the existing cluster. If oberth-onboarding already exists, choose an unused name. Use --kubeconfig oberth-kind.kubeconfig with its new context and repeat plan/ports before the same install Review. These mappings serve clients on this machine only.",
	}
}
