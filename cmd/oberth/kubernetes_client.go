package main

import (
	"time"

	"k8s.io/client-go/rest"
)

func serverKubernetesConfig(config *rest.Config, options serveOptions) *rest.Config {
	configured := rest.CopyConfig(config)
	configured.Timeout = 10 * time.Second
	configured.QPS = float32(options.kubernetesQPS)
	configured.Burst = options.kubernetesBurst
	return configured
}
