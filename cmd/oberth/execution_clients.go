package main

import (
	"context"

	"github.com/oberthci/oberth/internal/app"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func schedulingClients(_ context.Context, config *rest.Config, options *serveOptions) (kubernetes.Interface, []app.ExecutionTarget, error) {
	policy, err := readArgoTargetPolicy(options.argoExecutionTargetConfig)
	if err != nil {
		return nil, nil, err
	}
	targets, err := buildArgoTarget(policy)
	if err != nil {
		return nil, nil, err
	}
	kube, err := kubernetes.NewForConfig(config)
	return kube, targets, err
}
