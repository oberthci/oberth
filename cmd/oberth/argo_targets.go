package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	argoscheme "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/scheme"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/resourcesampler"
)

// This file is administrator configuration. Repositories and API requests
// cannot supply a target, transport, credentials or executor configuration.
type argoTargetPolicy struct {
	Schema          string         `json:"schema"`
	ID              string         `json:"id"`
	Kubeconfig      string         `json:"kubeconfig"`
	Context         string         `json:"context"`
	ResourceSampler string         `json:"resource_sampler"`
	Config          argojob.Config `json:"config"`
}

func readArgoTargetPolicy(name string) (*argoTargetPolicy, error) {
	if name == "" {
		return nil, nil
	}
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("serve: execution target policy requires a clean absolute path")
	}
	// #nosec G304 -- administrator startup flag, never a request or repository.
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 {
		return nil, errors.New("serve: execution target policy too large")
	}
	if err := checkTargetPolicyJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var policy argoTargetPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("serve: execution target requires one object")
	}
	if policy.Schema != "oberth.execution-target/v1" || policy.ID == "" || policy.ID == "local" || policy.Context == "" || !filepath.IsAbs(policy.Kubeconfig) || filepath.Clean(policy.Kubeconfig) != policy.Kubeconfig || !policy.Config.ExecutionPlacement.BranchOnly() {
		return nil, errors.New("serve: complete explicit branch-only target policy required")
	}
	policy.Config, err = argojob.DefaultedConfig(policy.Config)
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

func checkTargetPolicyJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("serve: execution target policy nesting exceeds bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("serve: duplicate execution target policy member")
			}
			seen[name] = true
			if err := checkTargetPolicyJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := checkTargetPolicyJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("serve: invalid execution target policy JSON")
	}
	_, err = decoder.Token()
	return err
}

func buildArgoTarget(policy *argoTargetPolicy) ([]app.ExecutionTarget, error) {
	if policy == nil {
		return nil, nil
	}
	// Only this explicit file/context is loaded; no ambient or in-cluster
	// fallback participates in remote client construction.
	loader := &clientcmd.ClientConfigLoadingRules{ExplicitPath: policy.Kubeconfig}
	transport, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{CurrentContext: policy.Context}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load selected execution target transport: %w", err)
	}
	transport.Timeout = 10 * time.Second
	if err := policy.Config.ExecutionPlacement.VerifyTarget(transport); err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(transport)
	if err != nil {
		return nil, err
	}
	workflows, err := wfclientset.NewForConfig(transport)
	if err != nil {
		return nil, err
	}
	vmi, err := dynamic.NewForConfig(transport)
	if err != nil {
		return nil, err
	}
	create, err := rest.UnversionedRESTClientFor(dynamic.ConfigFor(transport))
	if err != nil {
		return nil, err
	}
	target := app.ExecutionTarget{ID: policy.ID, Config: policy.Config, Transport: transport, Kubernetes: &singlePostKubernetes{Interface: kube}, Workflows: func(namespace string) argojob.WorkflowClient {
		return &singlePostWorkflows{WorkflowClient: workflows.ArgoprojV1alpha1().Workflows(namespace), client: workflows.ArgoprojV1alpha1().RESTClient(), namespace: namespace}
	}, Exec: newArgoExecStreamer(transport, kube), VMI: vmi, VMICreate: create}
	if policy.ResourceSampler != "" {
		config, err := resourcesampler.ReadConfig(policy.ResourceSampler)
		if err != nil {
			return nil, err
		}
		source, err := resourcesampler.NewClient(config)
		if err != nil {
			return nil, err
		}
		target.ResourceSource = source
	}
	return []app.ExecutionTarget{target}, nil
}

// A journaled intent authorizes one possible POST, including inside client-go.
// Generated clients otherwise permit Retry-After retries hidden from custody.
type singlePostWorkflows struct {
	argojob.WorkflowClient
	client    rest.Interface
	namespace string
}

func (c *singlePostWorkflows) Create(ctx context.Context, object *wfv1.Workflow, options metav1.CreateOptions) (*wfv1.Workflow, error) {
	result := &wfv1.Workflow{}
	err := c.client.Post().Namespace(c.namespace).Resource("workflows").VersionedParams(&options, argoscheme.ParameterCodec).Body(object).MaxRetries(0).Do(ctx).Into(result)
	return result, err
}

type singlePostKubernetes struct{ kubernetes.Interface }

func (c *singlePostKubernetes) CoreV1() typedcore.CoreV1Interface {
	return &singlePostCore{CoreV1Interface: c.Interface.CoreV1()}
}

type singlePostCore struct{ typedcore.CoreV1Interface }

func (c *singlePostCore) Pods(namespace string) typedcore.PodInterface {
	return &singlePostPods{PodInterface: c.CoreV1Interface.Pods(namespace), client: c.RESTClient(), namespace: namespace}
}
func (c *singlePostCore) PersistentVolumeClaims(namespace string) typedcore.PersistentVolumeClaimInterface {
	return &singlePostClaims{PersistentVolumeClaimInterface: c.CoreV1Interface.PersistentVolumeClaims(namespace), client: c.RESTClient(), namespace: namespace}
}

type singlePostPods struct {
	typedcore.PodInterface
	client    rest.Interface
	namespace string
}

func (c *singlePostPods) Create(ctx context.Context, object *corev1.Pod, options metav1.CreateOptions) (*corev1.Pod, error) {
	result := &corev1.Pod{}
	err := c.client.Post().Namespace(c.namespace).Resource("pods").VersionedParams(&options, scheme.ParameterCodec).Body(object).MaxRetries(0).Do(ctx).Into(result)
	return result, err
}

type singlePostClaims struct {
	typedcore.PersistentVolumeClaimInterface
	client    rest.Interface
	namespace string
}

func (c *singlePostClaims) Create(ctx context.Context, object *corev1.PersistentVolumeClaim, options metav1.CreateOptions) (*corev1.PersistentVolumeClaim, error) {
	result := &corev1.PersistentVolumeClaim{}
	err := c.client.Post().Namespace(c.namespace).Resource("persistentvolumeclaims").VersionedParams(&options, scheme.ParameterCodec).Body(object).MaxRetries(0).Do(ctx).Into(result)
	return result, err
}
