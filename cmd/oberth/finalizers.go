package main

import (
	"context"
	"fmt"
	"time"

	argoclient "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

const finalizerTimeout = 10 * time.Minute

func finalizerClientConfig(config *rest.Config) *rest.Config {
	config = rest.CopyConfig(config)
	config.QPS, config.Burst, config.Timeout = serverClientQPS, serverClientBurst, 30*time.Second
	return config
}

func releaseInstallationFinalizers(ctx context.Context, args []string) error {
	if len(args) != 2 || len(validation.IsDNS1123Label(args[0])) != 0 || len(validation.IsDNS1123Label(args[1])) != 0 {
		return fmt.Errorf("provide the pipeline namespace and installation namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, finalizerTimeout)
	defer cancel()
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := argoclient.NewForConfig(finalizerClientConfig(config))
	if err != nil {
		return err
	}
	return installer.ReleaseRunFinalizers(ctx, client, installer.FinalizerScope{Namespace: args[0], Instance: args[1]})
}
