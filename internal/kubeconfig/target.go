// Package kubeconfig opens an explicit Kubernetes target without changing its files.
//
// Spec: docs/plans/c9-install-and-upgrade.md#invariants
package kubeconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/version"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Target binds all installation calls to one selected context.
type Target struct {
	Context string
	Config  *rest.Config
	Client  kubernetes.Interface
}

// Open requires an explicit context, including when a current-context is saved.
func Open(path, name string) (Target, error) {
	if name == "" {
		return Target{}, refusal("context", nil, "")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: name})
	raw, err := config.RawConfig()
	if err != nil {
		return Target{}, refusal("kubeconfig", err, "Kubeconfig: "+path)
	}
	if raw.Contexts[name] == nil {
		return Target{}, refusal("context-missing", nil, "Context: "+name+"; kubeconfig: "+path)
	}
	cfg, err := config.ClientConfig()
	if err != nil {
		return Target{}, refusal("kubeconfig", err, "Context: "+name+"; kubeconfig: "+path)
	}
	cfg.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Target{}, refusal("kubeconfig", err, "Context: "+name)
	}
	return Target{Context: name, Config: cfg, Client: client}, nil
}

// UID reads the stable cluster identity; it performs no discovery writes.
func (t Target) UID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ns, err := t.Client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read cluster identity: %w", err)
	}
	if ns.UID == "" {
		return "", fmt.Errorf("cluster identity is missing; check kube-system namespace")
	}
	return string(ns.UID), nil
}

// Version inherits the operation context even for the discovery read.
func (t Target) Version(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	discovery := t.Client.Discovery()
	if discovery.RESTClient() == nil {
		v, err := discovery.ServerVersion()
		if err != nil {
			return "", err
		}
		return v.GitVersion, nil
	}
	var v version.Info
	raw, err := discovery.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", err
	}
	err = json.Unmarshal(raw, &v)
	return v.GitVersion, err
}

func refusal(code string, cause error, detail string) error {
	text := "The selected kubeconfig could not be loaded.\nCheck its path, context and credential plugin configuration."
	if code == "context" {
		text = "Select an explicit kubeconfig context.\nPass --context with a context from your kubeconfig."
	}
	if code == "context-missing" {
		text = "The selected context is not in kubeconfig.\nChoose a context listed in the selected kubeconfig file."
	}
	if cause != nil {
		return fmt.Errorf("%s\n%s: %w", text, detail, cause)
	}
	return fmt.Errorf("%s\n%s", text, detail)
}
