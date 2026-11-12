package installer

import (
	"context"
	"strings"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
)

type policyModelStream struct {
	checks  int
	release chan struct{}
}
type policyTrafficModel struct {
	kube         *fake.Clientset
	mode, denial string
	// Source faults use only source identity and request count, never policy CRUD.
	fault    func(string, int) string
	mu       sync.Mutex
	requests map[string]int
	streams  map[string]*policyModelStream
	stages   []string
}

func (m *policyTrafficModel) outcome(ctx context.Context, namespace, pod string) (string, error) {
	m.mu.Lock()
	if m.requests == nil {
		m.requests = map[string]int{}
	}
	m.requests[pod]++
	n := m.requests[pod]
	m.mu.Unlock()
	if m.fault != nil {
		if state := m.fault(pod, n); state != "" {
			return state, nil
		}
	}
	if m.mode == "absent" || m.mode == "ingress-only" {
		return "allowed", nil
	}
	object, err := m.kube.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	domain, err := m.kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	policies, err := m.kube.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, p := range policies.Items {
		selector, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			return "", err
		}
		if !selector.Matches(labels.Set(object.Labels)) {
			continue
		}
		if m.mode == "namespace-exclusion" && domain.Labels["domain"] == "pipeline" {
			continue
		}
		if m.mode == "partial-node" && object.Spec.NodeName == "other-node" {
			continue
		}
		// This data plane never converges inside the model's short observation window.
		if m.mode == "programming-slower-than-window" {
			continue
		}
		for _, kind := range p.Spec.PolicyTypes {
			if kind == networkingv1.PolicyTypeEgress {
				return m.denial, nil
			}
		}
	}
	return "allowed", nil
}

func policyModelArgs(args []string) (namespace, pod, script string, values []string) {
	for i, arg := range args {
		if arg == "--namespace" && i+1 < len(args) {
			namespace = args[i+1]
		}
		if arg == "exec" && i+1 < len(args) {
			pod = args[i+1]
		}
		if arg == "-c" && i+2 < len(args) {
			script = args[i+1]
			values = args[i+3:]
			break
		}
	}
	return
}

func (m *policyTrafficModel) run(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
	namespace, pod, script, values := policyModelArgs(args)
	if script == policyOverlapReleaseScript {
		m.mu.Lock()
		defer m.mu.Unlock()
		for key, stream := range m.streams {
			if strings.HasSuffix(key, ":"+values[0]) && stream.checks >= 2 {
				close(stream.release)
				return []byte("released"), nil
			}
		}
		return []byte("invalid"), nil
	}
	if script == policyOverlapReadyScript {
		m.mu.Lock()
		defer m.mu.Unlock()
		stream := m.streams[pod+":"+values[0]]
		if stream == nil {
			return []byte("pending"), nil
		}
		stream.checks++
		return []byte("open"), nil
	}
	state, err := m.outcome(ctx, namespace, pod)
	m.mu.Lock()
	m.stages = append(m.stages, pod+":"+state)
	m.mu.Unlock()
	if err != nil || state != "allowed" || script != policyOverlapScript {
		return []byte(state), err
	}
	key := pod + ":" + values[1]
	stream := &policyModelStream{release: make(chan struct{})}
	m.mu.Lock()
	if m.streams == nil {
		m.streams = map[string]*policyModelStream{}
	}
	m.streams[key] = stream
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.streams, key); m.mu.Unlock() }()
	select {
	case <-stream.release:
		return []byte("allowed"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func policyModelTiming() policyProbeTiming {
	return policyProbeTiming{3 * time.Second, 100 * time.Millisecond, 250 * time.Millisecond, 100 * time.Millisecond, time.Millisecond}
}

// Test fixtures identify source A independently of which source is selected.
func policyModelSourceA(name string) bool { return strings.HasPrefix(name, "client-") }
